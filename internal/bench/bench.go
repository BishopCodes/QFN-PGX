// Package bench ports the upstream smoke-test battery to Go: cold-prefill
// rate, prefix-cache hit, determinism, decode speed, and optional needle-in-
// haystack for YaRN contexts. Methodology matches qwen3.8-Flash-DGX
// scripts/smoke-test.sh (never ignore_eos on this model — it yields
// meaningless numbers).
package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible endpoint (proxy front door or engine).
type Client struct {
	Base  string // http://127.0.0.1:8799 (proxy) or :18300 (engine)
	Key   string // bearer (front key or lockdown key)
	Model string
	HC    *http.Client
}

// NewClient defaults HC/Model.
func NewClient(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, Model: "qwen3.8-flash-next",
		HC: &http.Client{Timeout: 0}}
}

// Result is one probe outcome.
type Result struct {
	Probe  string  `json:"probe"`
	OK     bool    `json:"ok"`
	Value  float64 `json:"value,omitempty"`
	Unit   string  `json:"unit,omitempty"`
	Detail string  `json:"detail,omitempty"`
	Error  string  `json:"error,omitempty"`
}

func (c *Client) doJSON(ctx context.Context, path string, body any, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out)
}

// CompletionResp is the subset of a /v1/completions answer we need.
type CompletionResp struct {
	Choices []struct {
		Text     string            `json:"text"`
		Logprobs *struct {
			TopLogprobs []map[string]float64 `json:"top_logprobs"`
		} `json:"logprobs"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (c *Client) completion(ctx context.Context, prompt string, maxTokens int, logprobs int) (*CompletionResp, time.Duration, error) {
	body := map[string]any{
		"model": c.Model, "prompt": prompt, "max_tokens": maxTokens, "temperature": 0,
	}
	if logprobs > 0 {
		body["logprobs"] = logprobs
	}
	start := time.Now()
	var out CompletionResp
	err := c.doJSON(ctx, "/v1/completions", body, &out)
	return &out, time.Since(start), err
}

// Prefill: ~8k-token prompt cold, then the same prompt again to prove the
// prefix cache hit (upstream: HIT if the second call is <50% of the first).
func (c *Client) Prefill(ctx context.Context) []Result {
	prompt := businessPrompt(5800)
	r1, d1, err := c.completion(ctx, prompt, 1, 0)
	if err != nil {
		return []Result{{Probe: "prefill", Error: err.Error()}}
	}
	n := float64(r1.Usage.PromptTokens)
	res := []Result{{Probe: "prefill", OK: true, Value: n / d1.Seconds(), Unit: "tok/s",
		Detail: fmt.Sprintf("%d tok in %.2fs (cold)", int(n), d1.Seconds())}}
	r2, d2, err := c.completion(ctx, prompt, 1, 0)
	if err != nil {
		return append(res, Result{Probe: "prefix_cache", Error: err.Error()})
	}
	hit := d2 < d1/2
	res = append(res, Result{Probe: "prefix_cache", OK: hit, Value: d2.Seconds(), Unit: "s",
		Detail: fmt.Sprintf("re-serve of %d tok in %.2fs (was %.2fs) — %s",
			r2.Usage.PromptTokens, d2.Seconds(), d1.Seconds(), map[bool]string{true: "HIT", false: "no speedup (prefix caching off?)"}[hit])})
	_ = r2
	return res
}

// Determinism: identical prompt+logprobs twice must be byte- and bit-identical
// (EXACT_TOPK=1 contract).
func (c *Client) Determinism(ctx context.Context) []Result {
	prompt := businessPrompt(400) + "\n\nAnswer with one number:"
	r1, _, e1 := c.completion(ctx, prompt, 4, 3)
	r2, _, e2 := c.completion(ctx, prompt, 4, 3)
	if e1 != nil || e2 != nil {
		return []Result{{Probe: "determinism", Error: fmt.Sprint(e1, e2)}}
	}
	same := len(r1.Choices) > 0 && len(r1.Choices) == len(r2.Choices) &&
		r1.Choices[0].Text == r2.Choices[0].Text
	if same && r1.Choices[0].Logprobs != nil && r2.Choices[0].Logprobs != nil {
		a := r1.Choices[0].Logprobs.TopLogprobs
		b := r2.Choices[0].Logprobs.TopLogprobs
		if len(a) != len(b) {
			same = false
		} else {
			for i := range a {
				for k, v := range a[i] {
					if bv, ok := b[i][k]; !ok || v != bv {
						same = false
					}
				}
			}
		}
	}
	return []Result{{Probe: "determinism", OK: same,
		Detail: map[bool]string{true: "identical outputs + logprob vectors at T=0", false: "MISMATCH — stock top-k kernel? set exact_topk=true"}[same]}}
}

// Decode: a real ~300-word answer (non-stream), tok/s including TTFT — the
// upstream smoke-test's honest decode number.
func (c *Client) Decode(ctx context.Context) []Result {
	body := map[string]any{
		"model": c.Model, "max_tokens": 400, "temperature": 0,
		"messages": []any{map[string]string{
			"role": "user",
			"content": "Explain in about 300 words how a page cache works and why random reads from an NVMe-backed mmap get faster over time. /no_think",
		}},
	}
	var out struct {
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	start := time.Now()
	if err := c.doJSON(ctx, "/v1/chat/completions", body, &out); err != nil {
		return []Result{{Probe: "decode", Error: err.Error()}}
	}
	d := time.Since(start).Seconds()
	return []Result{{Probe: "decode", OK: true, Value: float64(out.Usage.CompletionTokens) / d,
		Unit: "tok/s", Detail: fmt.Sprintf("%d tok in %.1fs (incl. TTFT)", out.Usage.CompletionTokens, d)}}
}

// TTFT: time-to-first-token on a short prompt, streamed honestly.
func (c *Client) TTFT(ctx context.Context) []Result {
	start := time.Now()
	body, err := json.Marshal(map[string]any{
		"model": c.Model, "prompt": "The capital of France is", "max_tokens": 8,
		"temperature": 0, "stream": true,
	})
	if err != nil {
		return []Result{{Probe: "ttft", Error: err.Error()}}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return []Result{{Probe: "ttft", Error: err.Error()}}
	}
	defer resp.Body.Close()
	sc := newLineScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"text"`) {
			return []Result{{Probe: "ttft", OK: true, Value: time.Since(start).Seconds(), Unit: "s",
				Detail: "first streamed chunk"}}
		}
	}
	return []Result{{Probe: "ttft", Error: "no chunks received"}}
}

// Needle: plant a unique fact at `depthPct`% into an N-word context.
func (c *Client) Needle(ctx context.Context, words int, depthPct int) []Result {
	needle := fmt.Sprintf("The emergency code word is GLYPH-%d.", rand.Intn(9000)+1000)
	prefix := businessPrompt(words * depthPct / 100)
	suffix := businessPrompt(words*(100-depthPct)/100)
	prompt := prefix + "\n" + needle + "\n" + suffix +
		"\n\nQuestion: what is the emergency code word stated above? Answer with just the code word."
	r, _, err := c.completion(ctx, prompt, 16, 0)
	if err != nil {
		return []Result{{Probe: "needle", Error: err.Error()}}
	}
	got := strings.TrimSpace(r.Choices[0].Text)
	want := strings.TrimSuffix(strings.TrimPrefix(needle, "The emergency code word is "), ".")
	ok := strings.Contains(got, want)
	return []Result{{Probe: "needle", OK: ok,
		Detail: fmt.Sprintf("%d-word context, needle at %d%% — answered %q (want %q)", words, depthPct, got, want)}}
}

// Battery runs the standard set.
func (c *Client) Battery(ctx context.Context, needleWords int) []Result {
	var out []Result
	out = append(out, c.TTFT(ctx)...)
	out = append(out, c.Prefill(ctx)...)
	out = append(out, c.Determinism(ctx)...)
	out = append(out, c.Decode(ctx)...)
	if needleWords > 0 {
		out = append(out, c.Needle(ctx, needleWords, 50)...)
	}
	return out
}

// ---- prompt helpers (mirroring the upstream methodology) ----

var businessWords = strings.Fields("ledger invoice payroll contract clause annex schedule amount date vendor total net gross tax due paid")

func businessPrompt(n int) string {
	rng := rand.New(rand.NewSource(1)) // seed=1 like the upstream script
	words := make([]string, 0, n)
	for i := 0; i < n; i++ {
		w := businessWords[rng.Intn(len(businessWords))]
		if rng.Float64() < 0.2 {
			w += fmt.Sprint(rng.Intn(9999)+1)
		}
		words = append(words, w)
	}
	return strings.Join(words, " ") +
		"\n\nQuestion: list three numbers that appear right after the word 'invoice', then name the most frequent word. Answer:"
}

func newLineScanner(r io.Reader) *lineScanner { return &lineScanner{r: r} }

type lineScanner struct {
	r   io.Reader
	buf []byte
}

func (l *lineScanner) Scan() bool {
	var cur []byte
	for {
		b := make([]byte, 1)
		n, err := l.r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				l.buf = cur
				return true
			}
			cur = append(cur, b[0])
			continue
		}
		if err != nil {
			if len(cur) > 0 {
				l.buf = cur
				return true
			}
			return false
		}
	}
}

func (l *lineScanner) Text() string { return string(l.buf) }
