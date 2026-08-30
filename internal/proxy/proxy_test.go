package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEngine mimics the vLLM shapes the relay depends on.
type fakeEngine struct {
	mu        sync.Mutex
	hits      map[string]int
	abortedCh chan string
	slow      func() // hook for streaming pacing
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{hits: map[string]int{}, abortedCh: make(chan string, 4)}
}

func (f *fakeEngine) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits["chat"]++
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			chunk := func(i int, content string) {
				fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%s\"}}]}\n\n", content)
				fl.Flush()
			}
			chunk(0, "hel")
			chunk(1, "lo")
			// A long silence — exactly what the keepalive must paper over.
			select {
			case <-time.After(300 * time.Millisecond):
			case <-r.Context().Done():
				f.abortedCh <- "aborted"
				return
			}
			chunk(2, " world")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			return
		}
		// non-streaming JSON with usage
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "hello world"}}},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 7},
		})
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits["tokenize"]++
		f.mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		n := 42
		if !strings.Contains(string(body), "x") {
			n = 5
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []int{}, "count": n})
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer secret" {
			http.Error(w, "nope", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]string{"id": "qwen3.8-flash-next"}}})
	})
	return mux
}

func newTestProxy(t *testing.T, target string, maxPrompt int) (*Proxy, *Registry) {
	t.Helper()
	reg := NewRegistry(50)
	p := New(reg)
	p.Target = func() string { return target }
	p.Key = func() string { return "secret" }
	p.MaxPromptTokens = func() int { return maxPrompt }
	p.Keepalive = 60 * time.Millisecond
	return p, reg
}

func TestSSEStreamRelayWithKeepalive(t *testing.T) {
	eng := newFakeEngine()
	srv := httptest.NewServer(eng.handler())
	defer srv.Close()
	p, reg := newTestProxy(t, srv.URL, 0)

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"qwen3.8-flash-next","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hel") || !strings.Contains(body, " world") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("relay lost upstream chunks:\n%s", body)
	}
	nPing := strings.Count(body, ": keepalive\n\n")
	if nPing < 1 {
		t.Fatalf("expected ≥1 keepalive during the 300 ms silence, got %d:\n%s", nPing, body)
	}
	// Injection must land at event boundaries: every keepalive comment is
	// itself a complete event (\n\n-terminated) and the byte just before each
	// injection point must be a blank-line boundary from a real event.
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if l == ": keepalive" {
			if i == 0 || lines[i-1] != "" {
				t.Fatalf("keepalive injected mid-event at line %d:\n%q", i, body)
			}
		}
	}
	// Registry telemetry.
	rows := reg.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows %d", len(rows))
	}
	row := rows[0]
	if row.Phase != "done" || row.Tokens != 3 || row.Stream != true {
		t.Fatalf("row %+v", row)
	}
	if row.FirstTokenAt == nil || row.DoneAt == nil {
		t.Fatalf("timestamps missing: %+v", row)
	}
}

func TestClientAbortCancelsUpstream(t *testing.T) {
	eng := newFakeEngine()
	srv := httptest.NewServer(eng.handler())
	defer srv.Close()
	p, reg := newTestProxy(t, srv.URL, 0)

	ctx, cancel := context.WithCancel(context.Background())
	// A recorder can't cancel mid-stream; use a live server for this one.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r)
	}))
	defer srv2.Close()
	httpReq, _ := http.NewRequestWithContext(ctx, "POST", srv2.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Scan() // first event line arrives, then we leave mid-silence
	resp.Body.Close()
	cancel()

	select {
	case <-eng.abortedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never observed the client abort")
	}
	deadline := time.Now().Add(time.Second)
	for {
		rows := reg.Snapshot()
		for _, r := range rows {
			if r.Aborted && r.Phase == "aborted" {
				return // success
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("registry never marked the abort: %+v", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEngineDownIs503Not400(t *testing.T) {
	p, reg := newTestProxy(t, "http://127.0.0.1:1", 0) // nothing listening
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"x x x"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "engine_unavailable") {
		t.Fatalf("body %s", rec.Body.String())
	}
	_ = reg
}

func TestPromptCeiling(t *testing.T) {
	eng := newFakeEngine()
	srv := httptest.NewServer(eng.handler())
	defer srv.Close()
	p, _ := newTestProxy(t, srv.URL, 10) // fake tokenize returns 42 for bodies containing "x"

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"m","prompt":"xxx","max_tokens":5}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "context_too_long") {
		t.Fatalf("ceiling not enforced: %d %s", rec.Code, rec.Body.String())
	}
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.hits["chat"] != 0 {
		t.Fatal("over-ceiling prompt must never reach the engine")
	}
}

func TestJSONUsageRecorded(t *testing.T) {
	eng := newFakeEngine()
	srv := httptest.NewServer(eng.handler())
	defer srv.Close()
	p, reg := newTestProxy(t, srv.URL, 0)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"m","messages":[{"role":"user","content":"hello there x"}]}`))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "hello world") {
		t.Fatalf("relay %s", rec.Body.String())
	}
	row := reg.Snapshot()[0]
	if row.PromptTokens != 12 || row.Tokens != 7 || row.Phase != "done" {
		t.Fatalf("usage row %+v", row)
	}
}

func TestLockdownKeyInjected(t *testing.T) {
	eng := newFakeEngine()
	srv := httptest.NewServer(eng.handler()) // /v1/models requires Bearer secret
	defer srv.Close()
	p, _ := newTestProxy(t, srv.URL, 0)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("proxy must inject the lockdown key: %d", rec.Code)
	}
	// And without the key the same request would have been 401 upstream — the
	// passthrough copies the upstream status, proving injection is what worked.
	p.Key = func() string { return "" }
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected upstream 401 without key, got %d", rec2.Code)
	}
}
