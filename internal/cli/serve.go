package cli

// `qfn serve`: the always-on console — hot-reloads config.toml each call,
// wipes sessions on restart, and mounts the proxy when serve.proxy is on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/BishopCodes/qfn-pgx/internal/auth"
	"github.com/BishopCodes/qfn-pgx/internal/collector"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
	"github.com/BishopCodes/qfn-pgx/internal/proxy"
	"github.com/BishopCodes/qfn-pgx/internal/server"
	"github.com/BishopCodes/qfn-pgx/internal/service"
)

func addServe(root *cobra.Command, app *App) {
	var (
		portFlag int
		bindFlag string
		noProxy  bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web console + proxy (foreground; systemd keeps it alive via `qfn service install`)",
		Args:  cobra.NoArgs, // without this, `qfn serve start` silently FOREGROUNDS the server
		RunE: func(c *cobra.Command, args []string) error {
			if err := firstRunGuard(app); err != nil {
				return err
			}
			// A service-managed console already owns the port; running a
			// second one by hand produces a baffling bind error — name the
			// owner and the right command instead (INVOCATION_ID proves WE
			// are the service when systemd launched us).
			if systemdActive() && os.Getenv("INVOCATION_ID") == "" {
				return fmt.Errorf("console is already running as a systemd service — `qfn serve restart` to reload it (or `sudo systemctl stop %s.service` to take over manually)", service.ServeUnit)
			}
			if pid, ok := readPid(app); ok {
				return fmt.Errorf("console is already running (pid %d) — `qfn serve restart` to cycle it, `qfn serve stop` to end it", pid)
			}
			return runServe(c.Context(), app, portFlag, bindFlag, noProxy)
		},
	}
	cmd.Flags().IntVar(&portFlag, "port", 0, "override serve.port")
	cmd.Flags().StringVar(&bindFlag, "bind", "", "override serve.bind")
	cmd.Flags().BoolVar(&noProxy, "no-proxy", false, "console only; don't mount /v1")

	cmd.AddCommand(serveStartCmd(app), serveStopCmd(app), serveRestartCmd(app), serveStatusCmd(app))
	root.AddCommand(cmd)
}

// ---- console stop / restart / status ----
//
// Same story as the web "Restart console" button: if the systemd unit is
// active, systemctl is authoritative (and survives logout); otherwise we use
// the pidfile that every `qfn serve` writes next to its state.

func pidPath(app *App) string { return app.StateDir() + "/serve.pid" }

func writePidfile(app *App) {
	_ = os.MkdirAll(filepath.Dir(pidPath(app)), 0o700)
	_ = os.WriteFile(pidPath(app), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func readPid(app *App) (int, bool) {
	b, err := os.ReadFile(pidPath(app))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	return pid, true
}

func systemdActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", service.ServeUnit+".service").Run() == nil
}

func systemctl(verb string) error {
	unit := service.ServeUnit + ".service"
	if err := exec.Command("systemctl", verb, unit).Run(); err == nil {
		return nil
	}
	// System units need root: retry via sudo (it prompts on the terminal).
	if err := exec.Command("sudo", "systemctl", verb, unit).Run(); err != nil {
		return fmt.Errorf("systemctl %s %s failed even via sudo: %w", verb, unit, err)
	}
	return nil
}

func serveStopCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running web console (systemd-aware, pidfile fallback)",
		RunE: func(c *cobra.Command, _ []string) error {
			if systemdActive() {
				if err := systemctl("stop"); err != nil {
					return err
				}
				okf("console stopped (systemd)")
				return nil
			}
			pid, ok := readPid(app)
			if !ok {
				pids := scanConsolePids()
				if len(pids) == 0 {
					dimf("no console running (systemd unit inactive, no live pidfile)\n")
					return nil
				}
				for _, p := range pids {
					_ = syscall.Kill(p, syscall.SIGTERM)
				}
				okf("SIGTERM → console pid(s) %v (found by process scan)", pids)
				return nil
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return err
			}
			okf("SIGTERM → console pid %d", pid)
			return nil
		},
	}
}

func serveRestartCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the console — picks up a newly installed qfn binary",
		RunE: func(c *cobra.Command, _ []string) error {
			if systemdActive() {
				if err := systemctl("restart"); err != nil {
					return err
				}
				okf("console restarted by systemd — now running %s", Version())
				return nil
			}
			if pid, ok := readPid(app); ok {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				time.Sleep(400 * time.Millisecond)
			}
			// /dev/null in, stateDir/serve.log out — never the caller's
			// terminal (detached child + closed pty = EIO death; see 35134e7).
			pid, logPath, err := respawnConsole(app)
			if err != nil {
				return err
			}
			okf("console restarted (pid %d) — console log: %s", pid, logPath)
			return nil
		},
	}
}

func serveStartCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the console if it isn't running (systemd unit when installed)",
		RunE: func(c *cobra.Command, _ []string) error {
			if systemdActive() {
				if err := systemctl("start"); err != nil {
					return err
				}
				okf("console started via systemd")
				return nil
			}
			if pid, ok := readPid(app); ok {
				dimf("console already running (pid %d) — `qfn serve restart` to cycle it\n", pid)
				return nil
			}
			pid, logPath, err := respawnConsole(app)
			if err != nil {
				return err
			}
			okf("console started (pid %d) — console log: %s", pid, logPath)
			return nil
		},
	}
}

// respawnConsole starts a detached, log-redirected console and verifies it
// survives past its first print. Never inherits the caller's terminal.
func respawnConsole(app *App) (int, string, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, "", err
	}
	logPath := app.StateDir() + "/serve.log"
	_ = os.MkdirAll(app.StateDir(), 0o700)
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return 0, "", err
	}
	child := exec.Command(exe, "serve")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdin = nil
	child.Stdout = lf
	child.Stderr = lf
	if err := child.Start(); err != nil {
		lf.Close()
		return 0, "", fmt.Errorf("spawn console: %w", err)
	}
	pid := child.Process.Pid
	_ = child.Process.Release()
	_ = lf.Close()
	time.Sleep(800 * time.Millisecond)
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, "", fmt.Errorf("console died on startup — see %s (a stale instance holding port %d is the usual cause)", logPath, app.Cfg.Serve.Port)
	}
	return pid, logPath, nil
}

func serveStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is the console running, and how?",
		RunE: func(c *cobra.Command, _ []string) error {
			switch {
			case systemdActive():
				fmt.Printf("  console: %s (systemd unit, %s)\n", okStr(), Version())
			default:
				if pid, ok := readPid(app); ok {
					fmt.Printf("  console: running standalone, pid %d\n", pid)
				} else if pids := scanConsolePids(); len(pids) > 0 {
					fmt.Printf("  console: running standalone, pid %v (pre-pidfile instance — `qfn serve restart` to upgrade tracking)\n", pids)
				} else {
					dimf("  console: not running — `qfn serve` (or `sudo qfn service install` for always-on)\n")
				}
			}
			return nil
		},
	}
}

func okStr() string { return "up" }

func runServe(ctx context.Context, app *App, port int, bind string, noProxy bool) error {
	// Hot-reload source: every dependency reads config fresh per call.
	live := func() config.Config {
		cfg, _, err := config.Load()
		if err != nil {
			return app.Cfg
		}
		if port != 0 {
			cfg.Serve.Port = port
		}
		if bind != "" {
			cfg.Serve.Bind = bind
		}
		if noProxy {
			cfg.Serve.Proxy = false
		}
		return cfg
	}
	cfg := live()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !app.Store.HasPassword() {
		return errors.New("no console password yet — run `qfn init`")
	}

	// Sessions die with the process (documented security posture).
	sessions := auth.NewSessions(12 * time.Hour)
	limiter := auth.NewLimiter(app.StateDir() + "/login-attempts.json")

	engKey := func() string {
		if !app.Cfg.Engine.Lockdown {
			return ""
		}
		k, _ := app.Store.EngineKey()
		return k
	}
	base := func() string { return app.EngineBaseURL() }

	col := collector.New(collector.Config{
		EngineBase:    base,
		EngineKey:     engKey,
		ContainerName: cfg.Engine.Name,
		HFCacheHost:   config.ExpandHome(cfg.Paths.HFCache),
		Interval:      2 * time.Second,
	}, collector.IO{})
	go col.Run(ctx)

	reg := proxy.NewRegistry(200)
	var proxyHandler http.Handler
	if cfg.Serve.Proxy {
		p := proxy.New(reg)
		p.Target = base
		p.Key = engKey
		p.MaxPromptTokens = func() int { return live().Serve.MaxPromptTokens }
		p.Sampling = func() map[string]any {
			c := live()
			if !c.Serve.SamplingDefaults {
				return nil
			}
			return engine.GenerationDefaults(config.ExpandHome(c.Paths.HFCache), c.Engine.Model)
		}
		proxyHandler = p
	}

	srv := server.New(server.Deps{
		Cfg:       live,
		Store:     app.Store,
		Sessions:  sessions,
		Limiter:   limiter,
		Collector: col,
		Manager:   app.Mgr,
		Registry:  reg,
		Proxy:     proxyHandler,
		Profiles: func() []string {
			names, _ := profiles.List()
			return names
		},
		Resolve:   app.Resolve,
		Locator:   app.Locator,
		Preflight: func(ctx context.Context, e config.Engine) error {
			if err := doctor.Preflight(ctx, live(), e, false); err != nil {
				return err
			}
			if exists, _, err := engine.ImageExists(ctx, app.Docker, e.Image); err == nil && !exists {
				return fmt.Errorf("image %s not built — run `qfn build` on the Spark first", e.Image)
			}
			return nil
		},
		Meta:      func() map[string]any { return app.meta() },
		Version:   Version(),
	})

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Serve.Bind, cfg.Serve.Port),
		Handler: srv.Handler(),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	writePidfile(app)
	defer os.Remove(pidPath(app))
	fmt.Printf("qfn console on http://%s (auth: password, sessions wiped on restart)\n", httpSrv.Addr)
	if proxyHandler != nil {
		fmt.Printf("        proxy /v1 → %s (lockdown key %v)\n", base(), app.Cfg.Engine.Lockdown)
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// meta assembles the console's model panel data.
func (a *App) meta() map[string]any {
	lu := a.LastUp()
	return map[string]any{
		"model_name":   engine.ServedModelName,
		"mode":         lu.Mode,
		"ctx":          lu.Ctx,
		"mtp":          lu.MTP,
		"prefix_cache": lu.Prefix,
		"exact_topk":   lu.Topk,
		"serve_port":   a.Cfg.Serve.Port,
	}
}

// scanConsolePids finds running `qfn serve` processes the slow way —
// /proc cmdline — for instances started by an older binary (no pidfile).
// Never matches the scanning process itself.
func scanConsolePids() []int {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) < 2 || filepath.Base(args[0]) != "qfn" {
			continue
		}
		for _, a := range args[1:] {
			if a == "serve" { // subcommands (serve stop etc.) exit instantly; the server is bare "serve"
				out = append(out, pid)
				break
			}
		}
	}
	return out
}
