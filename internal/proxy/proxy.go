package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Proxy is the OpenAI-compatible front door (mounted at /v1 by the console).
type Proxy struct {
	// Target returns the live engine base URL (profiles can move the port).
	Target func() string
	// Key returns the lockdown bearer for the upstream ("" when no lockdown).
	Key func() string
	// MaxPromptTokens returns the prompt ceiling (0 = no ceiling).
	MaxPromptTokens func() int
	// Sampling returns checkpoint-recommended sampling defaults to fill into
	// requests that omit the fields (FreeToken's --sampling-defaults trick);
	// nil or empty map = feature off.
	Sampling func() map[string]any

	Registry  *Registry
	Keepalive time.Duration // SSE injection interval (default 10 s)

	client *http.Client
}

// New wires defaults.
func New(reg *Registry) *Proxy {
	return &Proxy{
		Registry:  reg,
		Keepalive: 10 * time.Second,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 32,
				// never time out streaming bodies; dial guard instead:
				DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			},
		},
	}
}

func (p *Proxy) key() string {
	if p.Key == nil {
		return ""
	}
	return p.Key()
}

func (p *Proxy) target() string {
	if p.Target == nil {
		return ""
	}
	return strings.TrimRight(p.Target(), "/")
}

func (p *Proxy) maxPrompt() int {
	if p.MaxPromptTokens == nil {
		return 0
	}
	return p.MaxPromptTokens()
}

// tracked reports whether a request deserves a registry row.
func tracked(path string) bool {
	switch {
	case strings.HasSuffix(path, "/chat/completions"),
		strings.HasSuffix(path, "/completions"),
		strings.HasSuffix(path, "/messages"):
		return true
	}
	return false
}

// ServeHTTP implements the front door. Auth is the *caller's* middleware;
// the proxy assumes the request was authorized.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.target() == "" {
		writeJSONError(w, http.StatusServiceUnavailable, "engine_unavailable", "no engine target configured (is anything running?)")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 128<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "cannot read body")
		return
	}

	if isAnthropic(r.URL.Path) {
		p.serveMessages(w, r, body)
		return
	}

	var reqMeta struct {
		Model  string          `json:"model"`
		Stream bool            `json:"stream"`
		Prompt json.RawMessage `json:"prompt"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &reqMeta)

	// Prompt ceiling via the cheap /tokenize pre-check (tricks contract: the
	// 503 path must NOT consult /tokenize — an engine that is loading answers
	// 4xx/5xx there and a body would then mis-report as context_too_long).
	if lim := p.maxPrompt(); lim > 0 && tracked(r.URL.Path) {
		if n, ok := p.countPromptTokens(r.Context(), body); ok && n > lim {
			writeJSONError(w, http.StatusBadRequest, "context_too_long",
				itoa(n)+" prompt tokens exceeds serve.max_prompt_tokens="+itoa(lim))
			return
		}
	}

	body = p.applySampling(body)

	reqCtx, cancel := context.WithCancel(r.Context())
	defer cancel()

	up, err := http.NewRequestWithContext(reqCtx, r.Method, p.target()+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	for k, vs := range r.Header {
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			up.Header.Add(k, v)
		}
	}
	if k := p.key(); k != "" {
		up.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := p.client.Do(up)
	if err != nil {
		// Engine refused/reset (stopped, crashed, still loading): explicit 503,
		// never a 400 — the client must not be told its prompt was the problem.
		p.recordError(w, r, reqMeta, "engine_unreachable")
		return
	}
	defer resp.Body.Close()

	if !tracked(r.URL.Path) {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	req := &Request{
		ID:        p.Registry.NextID(),
		Endpoint:  endpointOf(r.URL.Path),
		Model:     reqMeta.Model,
		Client:    clientIP(r),
		StartedAt: time.Now(),
		Phase:     "prefill",
		Stream:    reqMeta.Stream || strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"),
	}
	p.Registry.Add(req)

	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if !isSSE {
		p.relayJSON(w, r, resp, req)
		return
	}
	p.relaySSE(reqCtx, cancel, w, r, resp, req)
}

// relayJSON streams through the JSON body (buffered read is fine — it's the
// whole answer) and records usage.
func (p *Proxy) relayJSON(w http.ResponseWriter, r *http.Request, resp *http.Response, req *Request) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	now := time.Now()
	req.DoneAt = &now
	req.Status = resp.StatusCode
	if err == nil {
		var u struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				OutputTokens     int `json:"output_tokens"` // anthropic
			} `json:"usage"`
		}
		if json.Unmarshal(body, &u) == nil {
			req.PromptTokens = u.Usage.PromptTokens
			req.Tokens = u.Usage.CompletionTokens
			if req.Tokens == 0 {
				req.Tokens = u.Usage.OutputTokens
			}
		}
	}
	first := req.StartedAt
	req.FirstTokenAt = &first // JSON answers have no prefill/decode split to show
	p.Registry.Finish(req.ID, func(x *Request) {
		x.Tokens = req.Tokens
		x.PromptTokens = req.PromptTokens
		x.Status = req.Status
		if resp.StatusCode >= 400 {
			x.Phase = "error"
		} else {
			x.Phase = "done"
			x.FirstTokenAt = &first
		}
		x.DoneAt = &now
	})
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// relaySSE implements the streaming path: line-level relay, event-boundary
// tracking, keepalive injection only at boundaries, token counting from the
// protocol payloads, and abort-on-client-disconnect.
func (p *Proxy) relaySSE(reqCtx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request, resp *http.Response, req *Request) {
	flusher, _ := w.(http.Flusher)
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if flusher != nil {
		flusher.Flush()
	}

	lines := make(chan string) // raw SSE lines (without newline)
	errs := make(chan error, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs <- err
		}
	}()

	ticker := time.NewTicker(p.keepalive())
	defer ticker.Stop()
	atBoundary := true  // stream start is a boundary
	wroteSincePing := false
	done := false
	clientGone := r.Context().Done()

	for !done {
		select {
		case <-clientGone:
			cancel() // abort upstream: no zombie generations
			p.Registry.Finish(req.ID, func(x *Request) {
				now := time.Now()
				x.DoneAt = &now
				x.Aborted = true
				x.Phase = "aborted"
			})
			return
		case <-ticker.C:
			if atBoundary && wroteSincePing == false && !done {
				if _, err := io.WriteString(w, ": keepalive\n\n"); err == nil {
					if flusher != nil {
						flusher.Flush()
					}
					wroteSincePing = true
					atBoundary = false
				}
			}
		case line, ok := <-lines:
			if !ok {
				done = true
				break
			}
			wroteSincePing = false
			if line == "" {
				atBoundary = true // blank line ends an event — safe injection point
			} else if strings.HasPrefix(line, "data:") {
				p.consumeData(line, req)
			}
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				cancel()
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	select {
	case <-errs: // upstream stream errored; the relay already finished
	default:
	}

	now := time.Now()
	p.Registry.Finish(req.ID, func(x *Request) {
		x.DoneAt = &now
		x.Status = resp.StatusCode
		if x.Phase != "error" {
			x.Phase = "done"
		}
	})
}

// consumeData parses one SSE data payload for token events.
func (p *Proxy) consumeData(line string, req *Request) {
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	var ev struct {
		Type string `json:"type"` // anthropic
		Usage struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			InputTokens      *int `json:"input_tokens"`
			OutputTokens     *int `json:"output_tokens"`
		} `json:"usage"`
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
			Text string `json:"text"`
		} `json:"choices"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
			PartialJson string `json:"partial_json"`
		} `json:"delta"`
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	if ev.Error != nil {
		p.Registry.Update(req.ID, func(x *Request) { x.Phase = "error" })
		return
	}
	tok := false
	for _, c := range ev.Choices {
		if c.Delta.Content != "" || c.Text != "" {
			tok = true
		}
	}
	if ev.Type == "content_block_delta" && (ev.Delta.Text != "" || ev.Delta.Thinking != "" || ev.Delta.PartialJson != "") {
		tok = true
	}
	if !tok {
		return
	}
	p.Registry.Update(req.ID, func(x *Request) {
		if x.FirstTokenAt == nil {
			now := time.Now()
			x.FirstTokenAt = &now
			x.Phase = "decoding"
		}
		x.Tokens++
	})
}

// countPromptTokens asks the engine's /tokenize (best-effort: when it fails,
// no ceiling applies — prefer serving over refusing).
func (p *Proxy) countPromptTokens(ctx context.Context, body []byte) (int, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var probe struct {
		Model    string          `json:"model"`
		Prompt   json.RawMessage `json:"prompt"`
		Messages json.RawMessage `json:"messages"`
	}
	_ = json.Unmarshal(body, &probe)
	payload, _ := json.Marshal(map[string]any{
		"model": probe.Model,
		"prompt": firstNonEmpty(probe.Prompt, probe.Messages),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.target()+"/tokenize", bytes.NewReader(payload))
	if err != nil {
		return 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	if k := p.key(); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var out struct {
		Count int `json:"count"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out) != nil {
		return 0, false
	}
	return out.Count, out.Count > 0
}

func firstNonEmpty(a, b json.RawMessage) json.RawMessage {
	if len(a) > 0 {
		return a
	}
	return b
}

// applySampling fills omitted sampling params from the checkpoint's
// generation_config.json recommendations (only when serve.sampling_defaults
// is on and the endpoint is a generation one). Request fidelity note: fields
// the client set are never touched; only absent ones are filled.
func (p *Proxy) applySampling(body []byte) []byte {
	if p.Sampling == nil || len(body) == 0 || body[0] != '{' {
		return body
	}
	defs := p.Sampling()
	if len(defs) == 0 {
		return body
	}
	var generic map[string]any
	if json.Unmarshal(body, &generic) != nil {
		return body
	}
	changed := false
	for k, v := range defs {
		if _, present := generic[k]; !present {
			generic[k] = v
			changed = true
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(generic)
	if err != nil {
		return body
	}
	return out
}

func (p *Proxy) keepalive() time.Duration {
	if p.Keepalive <= 0 {
		return 10 * time.Second
	}
	return p.Keepalive
}

func (p *Proxy) recordError(w http.ResponseWriter, r *http.Request, meta any, reason string) {
	writeJSONError(w, http.StatusServiceUnavailable, "engine_unavailable",
		"engine is stopped, crashing, restarting or still loading ("+reason+")")
}

func endpointOf(path string) string {
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		return "chat/completions"
	case strings.HasSuffix(path, "/completions"):
		return "completions"
	case strings.HasSuffix(path, "/messages"):
		return "messages"
	default:
		return "other"
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if strings.EqualFold(k, "Transfer-Encoding") || strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeJSONError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"type": typ, "message": msg},
	})
}
