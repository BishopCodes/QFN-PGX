// Package proxy is QFN-PGX's OpenAI-compatible front door: the only sanctioned
// client path to the engine. It records live per-request telemetry (the
// dashboard's request feed), injects SSE keepalives at event boundaries while
// the engine buffers tool-call arguments (the dgx-spark-qwen38 trick — agent
// CLIs abort silent streams), aborts upstream the moment a client leaves (no
// zombie generations on a memory-tight box), and answers 503 engine_unavailable
// — never 400s — when the engine is unreachable (proxy-guard contract).
package proxy

import (
	"sync"
	"time"
)

// Request is one tracked generation.
type Request struct {
	ID           string    `json:"id"`
	Endpoint     string    `json:"endpoint"` // chat/completions|completions|messages|other
	Model        string    `json:"model"`
	Client       string    `json:"client"` // remote addr
	StartedAt    time.Time `json:"started_at"`
	FirstTokenAt *time.Time `json:"first_token_at,omitempty"`
	DoneAt       *time.Time `json:"done_at,omitempty"`
	Phase        string    `json:"phase"` // prefill|decoding|done|error|aborted
	Stream       bool      `json:"stream"`
	Tokens       int       `json:"tokens"`         // generated so far (SSE chunks / usage)
	PromptTokens int       `json:"prompt_tokens"`  // from usage, when reported
	Status       int       `json:"status"`         // upstream status once known
	Aborted      bool      `json:"aborted"`        // client left first
}

// TPS is tokens/s since first token (0 pre-decode).
func (r *Request) TPS() float64 {
	if r.FirstTokenAt == nil {
		return 0
	}
	end := time.Now()
	if r.DoneAt != nil {
		end = *r.DoneAt
	}
	d := end.Sub(*r.FirstTokenAt).Seconds()
	if d <= 0 {
		return 0
	}
	return float64(r.Tokens) / d
}

// Registry keeps live rows plus a bounded ring of finished ones.
type Registry struct {
	mu     sync.Mutex
	live   map[string]*Request
	done   []*Request
	cap    int
	seq    int
	subs   map[chan struct{}]struct{}
}

// NewRegistry with the finished-request ring bound (plan: 200).
func NewRegistry(ringCap int) *Registry {
	return &Registry{live: map[string]*Request{}, cap: ringCap, subs: map[chan struct{}]struct{}{}}
}

// NextID mints request ids like r7.
func (rg *Registry) NextID() string {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.seq++
	return "r" + itoa(rg.seq)
}

func (rg *Registry) Add(req *Request) {
	rg.mu.Lock()
	rg.live[req.ID] = req
	rg.mu.Unlock()
	rg.notify()
}

// Update applies fn under lock and notifies subscribers.
func (rg *Registry) Update(id string, fn func(*Request)) {
	rg.mu.Lock()
	if r, ok := rg.live[id]; ok {
		fn(r)
	}
	rg.mu.Unlock()
	rg.notify()
}

// Finish moves a request to the ring.
func (rg *Registry) Finish(id string, fn func(*Request)) {
	rg.mu.Lock()
	r, ok := rg.live[id]
	if ok {
		delete(rg.live, id)
		if fn != nil {
			fn(r)
		}
		rg.done = append(rg.done, r)
		if len(rg.done) > rg.cap {
			rg.done = rg.done[len(rg.done)-rg.cap:]
		}
	}
	rg.mu.Unlock()
	rg.notify()
}

// Snapshot returns [live..., recent finished descending].
func (rg *Registry) Snapshot() []Request {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	out := make([]Request, 0, len(rg.live)+len(rg.done))
	for _, r := range rg.live {
		cp := *r
		out = append(out, cp)
	}
	for i := len(rg.done) - 1; i >= 0; i-- {
		cp := *rg.done[i]
		out = append(out, cp)
	}
	return out
}

// CountLive is the gauge for running count cross-checks.
func (rg *Registry) CountLive() int {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return len(rg.live)
}

// Subscribe returns change signals (coalesced, non-blocking).
func (rg *Registry) Subscribe() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	rg.mu.Lock()
	rg.subs[ch] = struct{}{}
	rg.mu.Unlock()
	return ch, func() {
		rg.mu.Lock()
		delete(rg.subs, ch)
		rg.mu.Unlock()
	}
}

func (rg *Registry) notify() {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	for ch := range rg.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
