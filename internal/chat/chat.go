// Package chat: interactive streaming REPL against the console front door
// (or the engine directly when the console isn't running). Requests go
// through the proxy when it's up, so the dashboard sees `qfn chat` traffic.
package chat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// Options configure one session.
type Options struct {
	Base    string // console (:8799) preferred; falls back to engine if refused
	Key     string // bearer for that endpoint
	Model   string
	NoThink bool
	System  string
}

// Session is a multi-turn conversation.
type Session struct {
	opts     Options
	messages []map[string]string
	hc       *http.Client
}

// New starts a session and prints the banner.
func New(opts Options) *Session {
	if opts.Model == "" {
		opts.Model = "qwen3.8-flash-next"
	}
	return &Session{
		opts: opts,
		hc:   &http.Client{},
		messages: func() []map[string]string {
			msgs := []map[string]string{}
			if opts.System != "" {
				msgs = append(msgs, map[string]string{"role": "system", "content": opts.System})
			}
			return msgs
		}(),
	}
}

// Send one user turn; returns assistant text.
func (s *Session) Send(ctx context.Context, text string, out io.Writer) (string, error) {
	s.messages = append(s.messages, map[string]string{"role": "user", "content": text})
	body := map[string]any{
		"model": s.opts.Model, "messages": s.messages, "stream": true, "temperature": 0.7,
	}
	if s.opts.NoThink {
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.opts.Base, "/")+"/v1/chat/completions", bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.opts.Key != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.Key)
	}
	start := time.Now()
	resp, err := s.hc.Do(req)
	if err != nil {
		s.popUser()
		return "", err
	}
	defer resp.Body.Close()

	// Pre-first-token silence on a cold prompt can run seconds — give the
	// user evidence of life (and elapsed time) until real tokens arrive.
	spinnerDone := make(chan struct{})
	defer close(spinnerDone)
	if isTTY(out) {
		go spin(out, spinnerDone, start)
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		s.popUser()
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var (
		sb       strings.Builder
		flush    = bufio.NewWriter(out)
		ttft     time.Duration
		tokens   int
		firstSet bool
	)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		for _, c := range ev.Choices {
			if c.Delta.Content == "" {
				continue
			}
			if !firstSet {
				ttft = time.Since(start)
				firstSet = true
				select { case <-spinnerDone: default: close(spinnerDone) } // stop spinner, clear line
			}
			tokens++
			sb.WriteString(c.Delta.Content)
			fmt.Fprint(flush, c.Delta.Content)
		}
		_ = flush.Flush()
	}
	if ctx.Err() != nil {
		fmt.Fprintln(out, "\n  ⏹ cancelled")
	}
	s.messages = append(s.messages, map[string]string{"role": "assistant", "content": sb.String()})
	dur := time.Since(start)
	if tokens > 0 {
		fmt.Fprintf(out, "\n  · ttft %s · %d tok · %.1f tok/s\n",
			ttft.Round(10*time.Millisecond), tokens, float64(tokens)/dur.Seconds())
	} else {
		fmt.Fprintln(out)
	}
	return sb.String(), sc.Err()
}

// spin animates a braille spinner + elapsed clock until done closes.
func spin(out io.Writer, done <-chan struct{}, start time.Time) {
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	t := time.NewTicker(90 * time.Millisecond)
	defer t.Stop()
	i := 0
	for {
		select {
		case <-done:
			_, _ = io.WriteString(out, "\r\x1b[2K")
			return
		case <-t.C:
			el := time.Since(start).Truncate(time.Second)
			_, _ = io.WriteString(out, fmt.Sprintf("\r\x1b[2K  %c thinking… %s since send", frames[i%len(frames)], el))
			i++
		}
	}
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}
func (s *Session) popUser() {
	if len(s.messages) > 0 {
		s.messages = s.messages[:len(s.messages)-1]
	}
}

// Clear resets history (banner note out).
func (s *Session) Clear() {
	s.messages = s.messages[:0]
	if s.opts.System != "" {
		s.messages = append(s.messages, map[string]string{"role": "system", "content": s.opts.System})
	}
}

// Run reads stdin until EOF/Ctrl-D; ctx cancellation aborts the in-flight turn.
func Run(ctx context.Context, opts Options) error {
	s := New(opts)
	in := bufio.NewScanner(os.Stdin)
	fmt.Printf("chat · %s via %s — type, blank line to send; :clear / :quit / Ctrl+D\n", opts.Model, opts.Base)
	var pending []string
	for {
		if len(pending) == 0 {
			fmt.Print("\n\u276f ")
		} else {
			fmt.Print("  \u00b7 ")
		}
		if !in.Scan() {
			return nil
		}
		text := strings.TrimSpace(in.Text())
		if text == "" {
			if len(pending) == 0 {
				continue
			}
			msg := strings.Join(pending, "\n")
			pending = pending[:0]
			if _, err := s.Send(ctx, msg, os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "  error: %v\n", err)
			}
			continue
		}
		switch text {
		case ":quit", ":q":
			return nil
		case ":clear":
			s.Clear()
			pending = pending[:0]
			fmt.Println("  (history cleared)")
			continue
		}
		pending = append(pending, text)
	}
}
