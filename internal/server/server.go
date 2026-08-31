// Package server is the always-on console: static embedded UI, login gate,
// SSE multiplexer (collector snapshots + proxy request feed + engine ops),
// and browser-driven engine lifecycle — the same Manager and launch spec the
// CLI uses, so web and CLI can never diverge.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/BishopCodes/qfn-pgx/internal/auth"
	"github.com/BishopCodes/qfn-pgx/internal/collector"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
	"github.com/BishopCodes/qfn-pgx/internal/proxy"
	"github.com/BishopCodes/qfn-pgx/web"
)

// Deps wires the console to the rest of the system; everything is a function
// or interface so `qfn serve` hot-reloads config without a restart.
type Deps struct {
	Cfg       func() config.Config
	Store     *auth.Store
	Sessions  *auth.Sessions
	Limiter   *auth.Limiter
	Collector *collector.Collector
	Manager   *engine.Manager
	Registry  *proxy.Registry
	Proxy     http.Handler // nil = passive mode (no /v1 mount)

	Profiles        func() []string
	// Resolve layers config < profile < "" (no flags) and returns the resolved
	// launch engine, its lockdown API key ("" when lockdown off), and any error.
	Resolve         func(profile string) (engine config.Engine, key string, err error)
	Locator         func() engine.SnapshotLocator
	Preflight       func(ctx context.Context, e config.Engine) error
	Meta            func() map[string]any
	// NoLogPump disables the background docker-logs pump (tests assert on
	// exact docker invocations; the pump would race them). Never set in prod.
	NoLogPump     bool
	Version         string
	CookieName      string
}

type Server struct {
	deps Deps
	bus  *logBus
}

func New(d Deps) *Server {
	if d.CookieName == "" {
		d.CookieName = "qfn_session"
	}
	s := &Server{deps: d}
	s.bus = newLogBus(d.Manager, func() string { return d.Cfg().Engine.Name })
	return s
}

// Handler builds the route table (Go 1.22 method-patterns in net/http).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static := http.FileServerFS(asFS(web.FS))
	mux.HandleFunc("GET /api/health", s.hHealth)
	mux.HandleFunc("POST /api/login", s.hLogin)
	mux.HandleFunc("POST /api/logout", s.hLogout)

	mux.Handle("GET /api/state", s.auth(http.HandlerFunc(s.hState)))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.hEvents)))
	mux.Handle("GET /api/engine/status", s.auth(http.HandlerFunc(s.hEngineStatus)))
	mux.Handle("GET /api/engine/logs", s.auth(http.HandlerFunc(s.hEngineLogs)))
	mux.Handle("POST /api/engine/up", s.auth(http.HandlerFunc(s.hEngineUp)))
	mux.Handle("POST /api/engine/restart", s.auth(http.HandlerFunc(s.hEngineRestart)))
	mux.Handle("POST /api/engine/down", s.auth(http.HandlerFunc(s.hEngineDown)))
	mux.Handle("POST /api/console/restart", s.auth(http.HandlerFunc(s.hConsoleRestart)))

	// ONE log pump for the whole process (see logbus.go).
	if !s.deps.NoLogPump {
		s.bus.start(context.Background())
	}

	if s.deps.Proxy != nil {
		mux.Handle("POST /v1/", s.auth(s.deps.Proxy))
		mux.Handle("GET /v1/", s.auth(s.deps.Proxy))
	}
	mux.Handle("/", static)
	return mux
}

func asFS(fsys fs.FS) fs.FS { return rootFS{fsys} }

type rootFS struct{ fsys fs.FS }

func (r rootFS) Open(name string) (fs.File, error) {
	if name == "/" {
		name = "/index.html"
	}
	return r.fsys.Open(strings.TrimPrefix(name, "/"))
}

// ---- auth ----

type authed int

const authedKey authed = iota

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.deps.Cfg()
		r.Header.Del("X-Qfn-Client") // attribution is ours to assign, never the caller's
		if !cfg.Serve.AuthEnabled {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(s.deps.CookieName); err == nil && s.deps.Sessions.Validate(c.Value) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authedKey, true)))
			return
		}
		// Programmatic: front-door machine key bearer (when set). Cookie OR
		// key both authorize today; named per-client keys land behind
		// serve.require_api_key with the api_keys table.
		if tok := bearer(r); tok != "" {
			if fk, err := s.deps.Store.FrontKey(); err == nil && fk != "" &&
				subtle.ConstantTimeCompare([]byte(tok), []byte(fk)) == 1 {
				r.Header.Set("X-Qfn-Client", "front-key")
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authedKey, true)))
				return
			}
			// Named machine keys (qfn keys add): revocable individually and
			// attributed by name in the request feed.
			if name, ok := s.deps.Store.MatchAPIKey(tok); ok {
				r.Header.Set("X-Qfn-Client", name)
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authedKey, true)))
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "authentication required")
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) hLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if ok, retry := s.deps.Limiter.Allow(ip); !ok {
		writeErrStatus(w, http.StatusTooManyRequests, "locked", retry.Round(time.Second).String())
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil {
		writeErr(w, http.StatusBadRequest, "expected {password}")
		return
	}
	if !s.deps.Store.VerifyPassword(body.Password) {
		s.deps.Limiter.Fail(ip)
		clearFields(body)
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	s.deps.Limiter.Success(ip)
	tok, err := s.deps.Sessions.Create()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "session mint failed")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.deps.CookieName, Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600,
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) hLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(s.deps.CookieName); err == nil {
		s.deps.Sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.deps.CookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) hHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": s.deps.Version,
		"auth_required": s.deps.Cfg().Serve.AuthEnabled,
	})
}

// ---- state & events ----

type requestRow struct {
	*proxy.Request
	TPS float64 `json:"tps"`
}

func (s *Server) rowsWithTPS() []requestRow {
	rows := s.deps.Registry.Snapshot()
	out := make([]requestRow, 0, len(rows))
	for i := range rows {
		out = append(out, requestRow{Request: &rows[i], TPS: rows[i].TPS()})
	}
	return out
}

func (s *Server) hState(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Cfg()
	resp := map[string]any{
		"ok": true,
		"version": s.deps.Version,
		"profiles": s.callProfiles(),
		"default_profile": cfg.Meta.DefaultProfile,
		"requests": s.rowsWithTPS(),
		"meta":     s.callMeta(),
		"serve": map[string]any{
			"port": cfg.Serve.Port, "bind": cfg.Serve.Bind, "proxy": cfg.Serve.Proxy,
			"auth": cfg.Serve.AuthEnabled,
		},
	}
	if snap := s.deps.Collector.Last(); snap != nil {
		resp["snapshot"] = snap
	}
	if op, ok := s.deps.Manager.LastOp(); ok {
		resp["op"] = op
	}
	status, _ := s.bus.snapshot()
	resp["status"] = status
	if keys, err := s.deps.Store.ListAPIKeys(); err == nil && len(keys) > 0 {
		resp["keys"] = keys
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) hEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no flusher")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	snapCh, unsubSnap := s.deps.Collector.Subscribe()
	reqCh, unsubReq := s.deps.Registry.Subscribe()
	opCh, unsubOp := s.deps.Manager.Subscribe()
	curStatus, unsubStatus, statusCh := s.bus.subscribeStatus()
	defer unsubSnap()
	defer unsubReq()
	defer unsubOp()
	defer unsubStatus()

	ctx := r.Context()
	tick := time.NewTicker(15 * time.Second) // SSE comment keep-warm
	defer tick.Stop()
	eta := s.bus.statusTicker() // ETA refresh while booting
	defer eta.Stop()
	sse(w, fl, "status", curStatus)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": warm\n\n")
			fl.Flush()
		case <-eta.C:
			st, _ := s.bus.snapshot()
			if st.Pct > 0 && st.Pct < 100 {
				sse(w, fl, "status", st) // boot ETA moves with time
			}
		case snap := <-snapCh:
			sse(w, fl, "snapshot", snap)
		case <-reqCh:
			sse(w, fl, "requests", s.rowsWithTPS())
		case ev := <-opCh:
			sse(w, fl, "ops", ev)
		case st := <-statusCh:
			sse(w, fl, "status", st)
		}
	}
}

func sse(w io.Writer, fl http.Flusher, event string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	fl.Flush()
}

// ---- engine ops ----

func (s *Server) callProfiles() []string {
	if s.deps.Profiles == nil {
		return nil
	}
	return s.deps.Profiles()
}
func (s *Server) callMeta() map[string]any {
	if s.deps.Meta == nil {
		return map[string]any{}
	}
	return s.deps.Meta()
}

type opReq struct {
	Profile *string `json:"profile"`
}

func (s *Server) hEngineUp(w http.ResponseWriter, r *http.Request) {
	var body opReq
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	s.engineOp(w, r, "up", body.Profile)
}

func (s *Server) hEngineRestart(w http.ResponseWriter, r *http.Request) {
	var body opReq
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	s.engineOp(w, r, "restart", body.Profile)
}

func (s *Server) hEngineDown(w http.ResponseWriter, r *http.Request) {
	s.engineOp(w, r, "down", nil)
}

func (s *Server) engineOp(w http.ResponseWriter, r *http.Request, kind string, profile *string) {
	cfg := s.deps.Cfg()
	name := ""
	if profile != nil {
		name = *profile
	}
	op, err := s.deps.Manager.TryBegin(kind, "serve")
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	// Detach from the HTTP request: the op outlives the client tab.
	ctx := context.WithoutCancel(r.Context())

	switch kind {
	case "down":
		go func() {
			err := s.deps.Manager.Down(ctx, op, cfg.Engine.Name)
			if err != nil {
				op.Err = err.Error() // surfaced via the ops event inside Manager
			}
		}()
	default: // up / restart
		eng, key, err := s.deps.Resolve(name)
		if err == nil && s.deps.Preflight != nil {
			err = s.deps.Preflight(ctx, eng)
		}
		var args []string
		if err == nil {
			args, err = engine.DockerArgs(eng, s.deps.Locator(), engine.LaunchOpts{
				EngineAPIKey: key, HFCacheHost: config.ExpandHome(cfg.Paths.HFCache),
			})
		}
		if err != nil {
			s.deps.Manager.Finish(op, err) // 409-style refusal published as finish-with-error
			writeErr(w, http.StatusPreconditionFailed, err.Error())
			return
		}
		go func() {
			if kind == "restart" {
				_ = s.deps.Manager.Down(ctx, mustBegin(s, "down", "serve"), eng.Name)
			}
			_ = s.deps.Manager.Up(ctx, op, args, eng.Name)
		}()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": kind, "op": op})
}

func mustBegin(s *Server, kind, actor string) engine.OpInfo {
	op, err := s.deps.Manager.TryBegin(kind, actor)
	if err != nil {
		// Busy: reuse the last op slot so Down still gets a record.
		last, _ := s.deps.Manager.LastOp()
		return last
	}
	return op
}

func (s *Server) hEngineStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Cfg()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	st, err := engine.Inspect(ctx, s.deps.Manager.Docker(), cfg.Engine.Name)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"container": nil,
			"phase": "down", "reachable": s.deps.Collector.Last() != nil && s.deps.Collector.Last().Engine.Reachable})
		return
	}
	phase := "created"
	detail := ""
	if st.Running {
		bt := &engine.BootTracker{}
		var buf strings.Builder
		lctx, lcancel := context.WithTimeout(ctx, 700*time.Millisecond)
		defer lcancel()
		_ = s.deps.Manager.Logs(lctx, cfg.Engine.Name, &buf)
		for _, line := range strings.Split(buf.String(), "\n") {
			p, d := bt.Feed(line)
			phase, detail = p.String(), d
			if p == engine.PhaseReady {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"container": st, "phase": phase, "detail": detail,
		"reachable": s.deps.Collector.Last() != nil && s.deps.Collector.Last().Engine.Reachable})
}

func (s *Server) hEngineLogs(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "no flusher")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	replay, unsub, ch := s.bus.subscribe()
	defer unsub()
	if len(replay) > 0 {
		sse(w, fl, "replay", replay) // ONE batched event, not N
	}
	ctx := r.Context()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			_, _ = io.WriteString(w, ": warm\n\n") // engine-down stays quiet, no reconnect storm
			fl.Flush()
		case line := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", line)
			fl.Flush()
		}
	}
}

// hConsoleRestart lets the browser (or CLI) reload the console onto a newly
// installed binary: under systemd we exit and Restart=always brings us back;
// standalone we respawn detached first. Session auth required (auth wraps it).
func (s *Server) hConsoleRestart(w http.ResponseWriter, r *http.Request) {
	under := os.Getenv("INVOCATION_ID") != ""
	writeJSON(w, http.StatusAccepted, map[string]any{"restart_in_ms": 400, "mode": restartMode(under)})
	go func() {
		time.Sleep(400 * time.Millisecond) // let the response flush
		consoleRestart(under)
	}()
}

// consoleRestart is a var so tests never os.Exit the test binary.
var consoleRestart = defaultConsoleRestart

func defaultConsoleRestart(underSystemd bool) {
	if !underSystemd {
		if exe, err := os.Executable(); err == nil {
			c := exec.Command(exe, "serve")
			c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			_ = c.Start()
		}
	}
	os.Exit(0)
}

// ---- misc helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeErrStatus(w http.ResponseWriter, code int, msg, retryIn string) {
	writeJSON(w, code, map[string]string{"error": msg, "retry_in": retryIn})
}

// clearFields best-effort zeroing of the decoded password (Go strings are
// immutable; this at least drops the map reference).
func clearFields(v any) { _ = v }

var _ = errors.Is
