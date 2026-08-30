package proxy

// Anthropic /v1/messages translation. vLLM only speaks OpenAI, but Claude
// Code-class agents speak Anthropic — so the front door translates, keeping
// every guard that the OpenAI path enforces (lockdown key, prompt ceiling,
// abort propagation, 503 contract, registry feed). Streaming re-emits the
// Anthropic event protocol: message_start → content_block_{start,delta,stop}
// (text_delta / thinking_delta / input_json_delta) → message_delta →
// message_stop, with ping events doubling as keepalives.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// isAnthropic reports whether this path is the Anthropic messages endpoint.
func isAnthropic(path string) bool { return strings.HasSuffix(path, "/messages") }

// anthropicReq is the subset of the Messages API we translate.
type anthropicReq struct {
	Model        string            `json:"model"`
	System       json.RawMessage   `json:"system"`
	Messages     []json.RawMessage `json:"messages"`
	MaxTokens    *int              `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	Temperature  *float64          `json:"temperature"`
	TopP         *float64          `json:"top_p"`
	TopK         json.RawMessage   `json:"top_k"`
	StopSequences []string         `json:"stop_sequences"`
	Tools        []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	} `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
}

// serveMessages translates Anthropic ⇄ OpenAI. Called from ServeHTTP with the
// (already authed) request body read.
func (p *Proxy) serveMessages(w http.ResponseWriter, r *http.Request, body []byte) {
	var aReq anthropicReq
	if err := json.Unmarshal(body, &aReq); err != nil || len(aReq.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "unparsable Messages request")
		return
	}
	openaiBody, err := buildOpenAIChat(aReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	obi, _ := json.Marshal(openaiBody)
	obi = p.applySampling(obi)

	// Same ceiling pre-check as the OpenAI path, on the *translated* body.
	if lim := p.maxPrompt(); lim > 0 {
		if n, ok := p.countPromptTokens(r.Context(), obi); ok && n > lim {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
				itoa(n)+" input tokens exceeds serve.max_prompt_tokens="+itoa(lim))
			return
		}
	}

	reqCtx, cancel := context.WithCancel(r.Context())
	defer cancel()
	up, err := http.NewRequestWithContext(reqCtx, http.MethodPost, p.target()+"/v1/chat/completions", bytes.NewReader(obi))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	up.Header.Set("Content-Type", "application/json")
	if k := p.key(); k != "" {
		up.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := p.client.Do(up)
	if err != nil {
		// Same contract as the OpenAI path: unreachable ≠ bad request.
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error",
			"engine is stopped, crashing, restarting or still loading (engine_unreachable)")
		return
	}
	defer resp.Body.Close()

	req := &Request{
		ID:        p.Registry.NextID(),
		Endpoint:  "messages",
		Model:     aReq.Model,
		Client:    clientIP(r),
		StartedAt: time.Now(),
		Phase:     "prefill",
		Stream:    aReq.Stream,
	}
	p.Registry.Add(req)

	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		p.relayAnthropicJSON(w, resp, req)
		return
	}
	p.relayAnthropicSSE(reqCtx, cancel, w, r, resp, req)
}

// ---- request translation ----

func buildOpenAIChat(a anthropicReq) (map[string]any, error) {
	msgs := make([]any, 0, len(a.Messages)+1)
	if sys := flattenText(a.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, raw := range a.Messages {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &m) != nil {
			return nil, errors.New("malformed message")
		}
		var blocks []map[string]any
		if json.Unmarshal(m.Content, &blocks) != nil { // plain string content
			var s string
			_ = json.Unmarshal(m.Content, &s)
			msgs = append(msgs, map[string]any{"role": m.Role, "content": s})
			continue
		}
		var text strings.Builder
		var toolCalls []any
		var preMsgs []any // tool_result turns come out as role:tool messages
		for _, b := range blocks {
			switch t, _ := b["type"].(string); t {
			case "text":
				if s, _ := b["text"].(string); s != "" {
					text.WriteString(s)
				}
			case "thinking", "redacted_thinking":
				// History of reasoning is not replayed to the model.
			case "image":
				if url := anthropicImageURL(b); url != "" {
					msgs = append(msgs, map[string]any{"role": m.Role, "content": []any{
						map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}},
					}})
				}
			case "tool_use": // assistant turns invoking tools
				id, _ := b["id"].(string)
				name, _ := b["name"].(string)
				args, _ := json.Marshal(b["input"])
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]string{"name": name, "arguments": string(args)},
				})
			case "tool_result": // user turns returning tool output
				id, _ := b["tool_use_id"].(string)
				content := ""
				switch c := b["content"].(type) {
				case string:
					content = c
				default:
					if raw, err := json.Marshal(b["content"]); err == nil {
						content = flattenRaw(raw)
					}
				}
				preMsgs = append(preMsgs, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
			default:
				return nil, fmt.Errorf("unsupported content block type %q", t)
			}
		}
		// tool_results must sit at the message start in OpenAI ordering.
		msgs = append(msgs, preMsgs...)
		switch m.Role {
		case "assistant":
			mm := map[string]any{"role": "assistant"}
			if text.Len() > 0 {
				mm["content"] = text.String()
			} else if len(toolCalls) > 0 {
				mm["content"] = nil
			} else {
				continue
			}
			if len(toolCalls) > 0 {
				mm["tool_calls"] = toolCalls
			}
			msgs = append(msgs, mm)
		default:
			if text.Len() == 0 && len(preMsgs) > 0 {
				continue // pure tool_result turn already emitted
			}
			msgs = append(msgs, map[string]any{"role": m.Role, "content": text.String()})
		}
	}

	out := map[string]any{
		"model":    a.Model,
		"messages": msgs,
	}
	if a.Model == "" {
		out["model"] = "qwen3.8-flash-next"
	}
	if a.MaxTokens != nil {
		out["max_tokens"] = *a.MaxTokens
	}
	if a.Temperature != nil {
		out["temperature"] = *a.Temperature
	}
	if a.TopP != nil {
		out["top_p"] = *a.TopP
	}
	if len(a.TopK) > 0 {
		var v any
		if json.Unmarshal(a.TopK, &v) == nil {
			out["top_k"] = v
		}
	}
	if len(a.StopSequences) > 0 {
		out["stop"] = a.StopSequences
	}
	if len(a.Tools) > 0 {
		tools := make([]any, 0, len(a.Tools))
		for _, t := range a.Tools {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Name, "description": t.Description, "parameters": json.RawMessage(t.InputSchema),
			}})
		}
		out["tools"] = tools
	}
	if len(a.ToolChoice) > 0 {
		var s string
		if json.Unmarshal(a.ToolChoice, &s) == nil {
			out["tool_choice"] = s
		} else {
			var obj struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(a.ToolChoice, &obj) == nil {
				switch obj.Type {
				case "auto":
					out["tool_choice"] = "auto"
				case "any":
					out["tool_choice"] = "required"
				case "tool":
					out["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": obj.Name}}
				case "none":
					out["tool_choice"] = "none"
				}
			}
		}
	}
	if a.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	return out, nil
}

func anthropicImageURL(b map[string]any) string {
	src, _ := b["source"].(map[string]any)
	if src == nil {
		return ""
	}
	if u, _ := src["url"].(string); u != "" {
		return u
	}
	mt, _ := src["media_type"].(string)
	data, _ := src["data"].(string)
	if mt != "" && data != "" {
		return "data:" + mt + ";base64," + data
	}
	return ""
}

// flattenText renders Anthropic's string-or-blocks fields (system) to text.
func flattenText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return flattenRaw(raw)
}

func flattenRaw(raw json.RawMessage) string {
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if t, _ := b["type"].(string); t == "text" {
			if s, _ := b["text"].(string); s != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

// ---- response translation (non-stream) ----

func (p *Proxy) relayAnthropicJSON(w http.ResponseWriter, resp *http.Response, req *Request) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	now := time.Now()
	var openAI struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &openAI)

	req.DoneAt = &now
	req.Status = resp.StatusCode
	req.FirstTokenAt = &req.StartedAt
	req.PromptTokens = openAI.Usage.PromptTokens
	req.Tokens = openAI.Usage.CompletionTokens
	phase := "done"
	if resp.StatusCode >= 400 {
		phase = "error"
	}
	p.Registry.Finish(req.ID, func(x *Request) { x.Phase = phase })

	if resp.StatusCode >= 400 || len(openAI.Choices) == 0 {
		msg := "upstream error"
		if openAI.Error != nil && openAI.Error.Message != "" {
			msg = openAI.Error.Message
		}
		writeAnthropicError(w, http.StatusBadGateway, "api_error", msg)
		return
	}
	ch := openAI.Choices[0]
	content := []any{}
	if ch.Message.ReasoningContent != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": ch.Message.ReasoningContent})
	}
	if ch.Message.Content != "" {
		content = append(content, map[string]any{"type": "text", "text": ch.Message.Content})
	}
	for _, tc := range ch.Message.ToolCalls {
		var input any
		if json.Unmarshal([]byte(tc.Function.Arguments), &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	usage := map[string]any{
		"input_tokens":  openAI.Usage.PromptTokens,
		"output_tokens": openAI.Usage.CompletionTokens,
	}
	if openAI.Usage.PromptDetails.CachedTokens > 0 {
		usage["cache_read_input_tokens"] = openAI.Usage.PromptDetails.CachedTokens
	}
	out := map[string]any{
		"id":            "msg_" + shortID(),
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       content,
		"stop_reason":   mapStopReason(ch.FinishReason),
		"stop_sequence": nil,
		"usage":         usage,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

func mapStopReason(fr string) string {
	switch fr {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// ---- response translation (stream) ----

type anthropicStreamer struct {
	w    io.Writer
	fl   http.Flusher
	idx  int      // next anthropic content-block index
	open string   // "", "thinking", "text", tool id
	cur  string   // tool_use id currently open
	tool map[int]int // openai tool-call index → anthropic block idx
	stop string
	in   int
	out  int
}

func (s *anthropicStreamer) event(ev string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", ev, b)
	if s.fl != nil {
		s.fl.Flush()
	}
}

func (s *anthropicStreamer) closeBlock() {
	if s.open != "" {
		s.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": s.idx - 1})
		s.open, s.cur = "", ""
	}
}

func (s *anthropicStreamer) openBlock(typ string, extra map[string]any) {
	s.closeBlock()
	block := map[string]any{"type": typ}
	for k, v := range extra {
		block[k] = v
	}
	s.event("content_block_start", map[string]any{"type": "content_block_start", "index": s.idx, "content_block": block})
	s.idx++
	s.open = typ
	if id, ok := extra["id"].(string); ok {
		s.cur = id
	}
}

func (p *Proxy) relayAnthropicSSE(reqCtx context.Context, cancel context.CancelFunc, w http.ResponseWriter, r *http.Request, resp *http.Response, req *Request) {
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if fl != nil {
		fl.Flush()
	}

	s := &anthropicStreamer{w: w, fl: fl, tool: map[int]int{}}
	msgID := "msg_" + shortID()
	s.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "model": req.Model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	s.event("ping", map[string]any{"type": "ping"})

	lines := make(chan string)
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
	done := false
	clientGone := r.Context().Done()

	for !done {
		select {
		case <-clientGone:
			cancel()
			p.Registry.Finish(req.ID, func(x *Request) {
				now := time.Now()
				x.DoneAt, x.Aborted, x.Phase = &now, true, "aborted"
			})
			return
		case <-ticker.C:
			s.event("ping", map[string]any{"type": "ping"}) // full events → always at boundary
		case line, ok := <-lines:
			if !ok {
				done = true
				break
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				done = true
				continue
			}
			var ev openAIChunk
			if json.Unmarshal([]byte(payload), &ev) != nil {
				continue
			}
			p.translateChunk(s, req, ev)
			if ev.Usage != nil {
				s.in, s.out = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
				p.Registry.Update(req.ID, func(x *Request) {
					x.PromptTokens, x.Tokens = ev.Usage.PromptTokens, ev.Usage.CompletionTokens
				})
			}
		}
	}
	select {
	case <-errs:
	default:
	}

	s.closeBlock()
	usage := map[string]any{"input_tokens": s.in, "output_tokens": s.out}
	s.event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": mapStopReason(s.stop), "stop_sequence": nil},
		"usage": usage,
	})
	s.event("message_stop", map[string]any{"type": "message_stop"})

	now := time.Now()
	p.Registry.Finish(req.ID, func(x *Request) {
		x.DoneAt, x.Status = &now, resp.StatusCode
		if x.Phase != "error" {
			x.Phase = "done"
		}
	})
}

// openAIChunk is the streamed /v1/chat/completions chunk subset.
type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		PromptDetails    struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// translateChunk maps one OpenAI chat chunk onto Anthropic events and feeds
// the registry row (first-token + token counting like consumeData does).
func (p *Proxy) translateChunk(s *anthropicStreamer, req *Request, ev openAIChunk) {
	if ev.Error != nil {
		s.event("error", map[string]any{"type": "error", "error": map[string]string{
			"type": "api_error", "message": ev.Error.Message}})
		p.Registry.Update(req.ID, func(x *Request) { x.Phase = "error" })
		return
	}
	counted := false
	markTok := func() {
		if counted {
			return
		}
		counted = true
		p.Registry.Update(req.ID, func(x *Request) {
			if x.FirstTokenAt == nil {
				now := time.Now()
				x.FirstTokenAt, x.Phase = &now, "decoding"
			}
			x.Tokens++
		})
	}
	for _, ch := range ev.Choices {
		if ch.Delta.ReasoningContent != "" {
			if s.open != "thinking" {
				s.openBlock("thinking", map[string]any{"thinking": ""})
			}
			s.event("content_block_delta", map[string]any{"type": "content_block_delta",
				"index": s.idx - 1, "delta": map[string]any{"type": "thinking_delta", "thinking": ch.Delta.ReasoningContent}})
			markTok()
		}
		if ch.Delta.Content != "" {
			if s.open != "text" {
				s.openBlock("text", map[string]any{"text": ""})
			}
			s.event("content_block_delta", map[string]any{"type": "content_block_delta",
				"index": s.idx - 1, "delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content}})
			markTok()
		}
		for _, tc := range ch.Delta.ToolCalls {
			blockIdx, seen := s.tool[tc.Index]
			if !seen || (tc.ID != "" && s.cur != tc.ID) {
				s.openBlock("tool_use", map[string]any{
					"id": firstNonZero(tc.ID, "toolu_"+shortID()), "name": tc.Function.Name, "input": map[string]any{},
				})
				blockIdx = s.idx - 1
				s.tool[tc.Index] = blockIdx
			}
			if tc.Function.Arguments != "" {
				s.event("content_block_delta", map[string]any{"type": "content_block_delta",
					"index": blockIdx, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
				markTok()
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.stop = *ch.FinishReason
		}
	}
}

func firstNonZero(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func shortID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	})
}
