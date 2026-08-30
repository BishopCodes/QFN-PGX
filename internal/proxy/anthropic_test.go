package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// captureUpstream is an OpenAI-shaped fake engine recording the last body.
func captureUpstream(t *testing.T, handler http.HandlerFunc) (*httptest.Server, func() map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var last map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		var fresh map[string]any
		if json.Unmarshal(b, &fresh) == nil {
			last = fresh // replace (not merge) so absent keys really are absent
		}
		mu.Unlock()
		handler(w, r)
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tokens": []int{}, "count": 10})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, func() map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

func TestAnthropicNonStreamingTranslation(t *testing.T) {
	up, _ := captureUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hello there","reasoning_content":"thinking hard"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":21,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8}}}`)
	})
	p, reg := newTestProxy(t, up.URL, 0)

	body := `{"model":"qwen3.8-flash-next","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			CachedTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("bad json: %s", rec.Body)
	}
	if out.Type != "message" || out.Role != "assistant" {
		t.Fatalf("envelope: %s", rec.Body)
	}
	if len(out.Content) != 2 || out.Content[0].Type != "thinking" || out.Content[1].Text != "hello there" {
		t.Fatalf("content blocks: %s", rec.Body)
	}
	if out.StopReason != "end_turn" {
		t.Fatalf("stop_reason %q", out.StopReason)
	}
	if out.Usage.InputTokens != 21 || out.Usage.OutputTokens != 5 || out.Usage.CachedTokens != 8 {
		t.Fatalf("usage: %+v", out.Usage)
	}
	row := reg.Snapshot()[0]
	if row.Endpoint != "messages" || row.PromptTokens != 21 || row.Tokens != 5 || row.Phase != "done" {
		t.Fatalf("registry row: %+v", row)
	}
}

func TestAnthropicRequestTranslationShapes(t *testing.T) {
	up, getLast := captureUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	})
	p, _ := newTestProxy(t, up.URL, 0)

	body := `{
	  "model":"m","max_tokens":32,
	  "system":[{"type":"text","text":"be terse"}],
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"weather in Paris?"}]},
	    {"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"get_weather","input":{"city":"Paris"}}]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"18C"}]}
	  ],
	  "tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
	  "tool_choice":{"type":"any"},
	  "stop_sequences":["STOP"]
	}`
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body)
	}
	// Response says tool_use.
	var out struct {
		Content []struct {
			Type  string         `json:"type"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// (finish_reason tool_calls but no tool_calls in the message → still maps stop_reason)
	if out.StopReason != "tool_use" {
		t.Fatalf("stop_reason: %s", out.StopReason)
	}
	// Upstream request shape checks — the heart of the translation.
	req := getLast()
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want system+user+assistant(tool_calls)+tool msgs, got %d: %v", len(msgs), msgs)
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" || sys["content"] != "be terse" {
		t.Fatalf("system: %v", sys)
	}
	as := msgs[2].(map[string]any)
	tcs, _ := as["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls missing: %v", as)
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("tool_call args: %v", fn)
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_1" || toolMsg["content"] != "18C" {
		t.Fatalf("tool result turn: %v", toolMsg)
	}
	tools, _ := req["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools: %v", req["tools"])
	}
	oofn := tools[0].(map[string]any)["function"].(map[string]any)
	if oofn["name"] != "get_weather" || oofn["parameters"] == nil {
		t.Fatalf("tool schema: %v", oofn)
	}
	if req["tool_choice"] != "required" {
		t.Fatalf("tool_choice any→required: %v", req["tool_choice"])
	}
	stop, _ := req["stop"].([]any)
	if len(stop) != 1 || stop[0] != "STOP" {
		t.Fatalf("stop_sequences: %v", req["stop"])
	}
}

func TestAnthropicStreamEventProtocol(t *testing.T) {
	up, _ := captureUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"reasoning_content":"think1"}}]}`,
			`{"choices":[{"delta":{"reasoning_content":"think2"}}]}`,
			`{"choices":[{"delta":{"content":"Hello "}}]}`,
			`{"choices":[{"delta":{"content":"world"}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tu_9","function":{"name":"bash","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":6}}`,
		}
		for _, c := range chunks {
			_, _ = io.WriteString(w, "data: "+c+"\n\n")
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	})
	p, reg := newTestProxy(t, up.URL, 0)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"x"}]}`)))

	events := []string{}
	blocks := []string{}
	var deltas []map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			var d map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &d) == nil {
				deltas = append(deltas, d)
				if d["type"] == "content_block_start" {
					b := d["content_block"].(map[string]any)["type"].(string)
					blocks = append(blocks, b)
				}
			}
		}
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"message_start", "ping", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing event %s in %s", want, joined)
		}
	}
	if events[0] != "message_start" {
		t.Fatalf("must start with message_start: %s", joined)
	}
	if events[len(events)-1] != "message_stop" {
		t.Fatalf("must end with message_stop: %s", joined)
	}
	// thinking → text → tool_use block order
	if strings.Join(blocks, ",") != "thinking,text,tool_use" {
		t.Fatalf("block order: %v", blocks)
	}
	// message_delta carries the mapped stop reason + usage
	var md map[string]any
	for _, d := range deltas {
		if d["type"] == "message_delta" {
			md = d
		}
	}
	if md == nil {
		t.Fatal("no message_delta")
	}
	if md["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason: %v", md["delta"])
	}
	if md["usage"].(map[string]any)["output_tokens"].(float64) != 6 {
		t.Fatalf("usage: %v", md["usage"])
	}
	row := reg.Snapshot()[0]
	if row.Tokens != 6 || row.PromptTokens != 11 || row.Phase != "done" {
		t.Fatalf("registry: %+v", row)
	}
}

func TestAnthropicEngineDownIsAnnotated503(t *testing.T) {
	p, _ := newTestProxy(t, "http://127.0.0.1:1", 0)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"x"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	var out struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Type != "error" || out.Error.Type != "overloaded_error" {
		t.Fatalf("anthropic error envelope missing: %s", rec.Body)
	}
}

func TestSamplingDefaultsInjection(t *testing.T) {
	up, getLast := captureUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"y"},"finish_reason":"stop"}],"usage":{"completion_tokens":1}}`)
	})
	p, _ := newTestProxy(t, up.URL, 0)
	p.Sampling = func() map[string]any { return map[string]any{"temperature": 0.6, "top_p": 0.95} }

	// Omitted fields get filled.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)))
	req := getLast()
	if req["temperature"].(float64) != 0.6 || req["top_p"].(float64) != 0.95 {
		t.Fatalf("defaults not injected: %v", req)
	}
	// Client-set fields are NEVER overridden (temperature 0 is present!).
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","temperature":0,"messages":[{"role":"user","content":"x"}]}`)))
	req = getLast()
	if req["temperature"].(float64) != 0 {
		t.Fatalf("temperature 0 overridden: %v", req)
	}
	if req["top_p"].(float64) != 0.95 {
		t.Fatalf("top_p should still fill: %v", req)
	}
	// Sampling off → untouched.
	p.Sampling = func() map[string]any { return nil }
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`)))
	if _, ok := getLast()["temperature"]; ok {
		t.Fatal("temperature must not appear when sampling defaults are off")
	}
}
