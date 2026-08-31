package collector

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Snapshot is the 1 Hz world state the console renders. Engine-agnostic field
// names so an SGLang collector can land behind the same JSON schema later.
type Snapshot struct {
	TS        time.Time       `json:"ts"`
	Host      HostState       `json:"host"`
	Engine    EngineState     `json:"engine"`
	GPU       GPU             `json:"gpu"`
	Container *ContainerState `json:"container,omitempty"` // nil until a container is found
}

// HostState covers memory/swap/backpressure/CPU/disk.
type HostState struct {
	MemTotalKiB     uint64  `json:"mem_total_kib"`
	MemAvailableKiB uint64  `json:"mem_available_kib"`
	MemUsedKiB      uint64  `json:"mem_used_kib"`
	SwapTotalKiB    uint64  `json:"swap_total_kib"`
	SwapUsedKiB     uint64  `json:"swap_used_kib"`
	CachedKiB       uint64  `json:"cached_kib"`
	Psi             map[string]PSI `json:"psi"` // cpu|memory|io
	SwapInPerS      float64 `json:"swap_in_per_s"`
	SwapOutPerS     float64 `json:"swap_out_per_s"`
	MajFaultPerS    float64 `json:"maj_fault_per_s"` // PLE page-cache pressure
	CPU             CPUUsage `json:"cpu"`
	Load1           float64  `json:"load1"`
	HFFreeKiB       uint64   `json:"hf_free_kib"`
}

// ContainerState is the engine container's cgroup slice.
type ContainerState struct {
	ID        string  `json:"id"`
	MemBytes  uint64  `json:"mem_bytes"`
	CPUPerS   float64 `json:"cpu_cores"`      // cores in use
	ReadBPS   float64 `json:"read_bytes_per_s"` // NVMe reads — PLE table traffic
	WriteBPS  float64 `json:"write_bytes_per_s"`
}

// EngineState is the vLLM view (windowed).
type EngineState struct {
	Reachable      bool     `json:"reachable"`
	Running        float64  `json:"running"`
	Waiting        float64  `json:"waiting"`
	Swapped        float64  `json:"swapped"`
	KVUsagePct     float64  `json:"kv_usage_pct"`
	PromptTokPerS  float64  `json:"prompt_tok_per_s"`
	GenTokPerS     float64  `json:"gen_tok_per_s"`
	PrefixHitRatio float64  `json:"prefix_hit_ratio"` // window hits/queries
	TTFTP50        float64  `json:"ttft_p50"`
	TTFTP90        float64  `json:"ttft_p90"`
	ITLP50         float64  `json:"itl_p50"`  // seconds per token (1/itl = tok/s)
	ITLP90         float64  `json:"itl_p90"`
	E2EP50         float64  `json:"e2e_p50"`
	TTFTSamples    int      `json:"ttft_samples"` // window bucket total (0 = idle)
	Spec           SpecStats `json:"spec"`        // MTP speculative decoding
}

// Config points the collector at things.
type Config struct {
	// EngineBase returns the live engine base URL (profile-dependent).
	EngineBase func() string
	// EngineKey returns the lockdown bearer key ("" = no lockdown).
	EngineKey func() string
	// EngineModel returns the served model id for /v1 requests (informational).
	ContainerName string
	HFCacheHost   string
	Interval      time.Duration
}

// IO seams so tests can feed fixtures instead of a real /proc + docker.
type IO struct {
	ReadFile     func(path string) ([]byte, error)
	StatFreeKB   func(path string) (uint64, bool)
	GPU          func(ctx context.Context) GPU
	Scrape       func(ctx context.Context, url, bearer string) (string, error)
	ContainerID  func(ctx context.Context) (string, error) // docker inspect; "" = not running
}

// Collector samples at 1 Hz and broadcasts snapshots to subscribers.
type Collector struct {
	cfg  Config
	io   IO
	hist *HistState

	mu       sync.Mutex
	last     *Snapshot
	subs     map[chan Snapshot]struct{}

	// previous counters for rate math
	prevVM     vmPrev
	prevStat   cpuTimes
	prevCores  []cpuTimes
	prevEng    engPrev
	metTick    int // scrape clock for the dead-engine backoff
	metFail    int
	prevCG     cgPrev
	prevAt     time.Time
	gpuCached  GPU
	gpuAt      time.Time
	cgID       string
	cgIDAt     time.Time

	hc *http.Client
}

type vmPrev struct{ pswpin, pswpout, majfault uint64 }
type engPrev struct {
	prompt, generation, prefixQ, prefixH    float64
	specAcc, specDraftTok, specDrafts            float64
	specSeen, seen                               bool
}

// SpecStats makes MTP speculative decoding observable: how often drafts are
// accepted and what they contribute — the numbers behind "is MTP earning its
// memory", straight from the /metrics counters.
type SpecStats struct {
	Active       bool    `json:"active"`
	AcceptancePct float64 `json:"acceptance_pct"` // accepted / drafted tokens
	AcceptedPerS float64  `json:"accepted_per_s"` // tokens/s of free output
	DraftedPerS  float64  `json:"drafted_per_s"`
	MeanTokens   float64  `json:"mean_accepted"`  // accepted tokens per draft step
}
type cgPrev struct {
	cpuUsec, readBytes, writeBytes uint64
	seen                               bool
}

// New builds a Collector; nil IO fields get real implementations.
func New(cfg Config, ioOpts IO) *Collector {
	c := &Collector{cfg: cfg, hist: NewHistState(), io: ioOpts, subs: map[chan Snapshot]struct{}{}}
	if c.io.ReadFile == nil {
		c.io.ReadFile = os.ReadFile
	}
	if c.io.StatFreeKB == nil {
		c.io.StatFreeKB = statfsFreeKB
	}
	if c.io.GPU == nil {
		g := newGPUQuery()
		c.io.GPU = g.sample
	}
	if c.io.Scrape == nil {
		c.hc = &http.Client{Timeout: 2 * time.Second}
		c.io.Scrape = c.scrapeHTTP
	}
	if c.io.ContainerID == nil {
		c.io.ContainerID = func(ctx context.Context) (string, error) {
			if c.cfg.ContainerName == "" {
				return "", errors.New("collector: no container name configured")
			}
			out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Id}}",
				c.cfg.ContainerName).Output()
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(string(out)), nil
		}
	}
	if c.cfg.Interval <= 0 {
		c.cfg.Interval = time.Second
	}
	return c
}

func (c *Collector) scrapeHTTP(ctx context.Context, url, bearer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &HTTPError{Status: resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return string(b), err
}

// HTTPError signals a non-200 engine scrape.
type HTTPError struct{ Status int }

func (e *HTTPError) Error() string { return "engine metrics HTTP " + itoa(e.Status) }

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

// Run samples until ctx ends, broadcasting snapshots.
func (c *Collector) Run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap := c.SampleOnce(ctx)
			c.mu.Lock()
			c.last = &snap
			subs := make([]chan Snapshot, 0, len(c.subs))
			for ch := range c.subs {
				subs = append(subs, ch)
			}
			c.mu.Unlock()
			for _, ch := range subs {
				select {
				case ch <- snap:
				default:
				}
			}
		}
	}
}

// Last returns the most recent snapshot (nil before the first tick).
func (c *Collector) Last() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil {
		return nil
	}
	cp := *c.last
	return &cp
}

// Subscribe streams snapshots; slow consumers miss ticks rather than blocking.
func (c *Collector) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 4)
	c.mu.Lock()
	c.subs[ch] = struct{}{}
	c.mu.Unlock()
	return ch, func() {
		c.mu.Lock()
		delete(c.subs, ch)
		c.mu.Unlock()
	}
}

// SampleOnce performs one full sample; exported for tests and one-shot `qfn stats`.
func (c *Collector) SampleOnce(ctx context.Context) Snapshot {
	now := time.Now()
	dt := 0.0
	if !c.prevAt.IsZero() {
		dt = now.Sub(c.prevAt).Seconds()
	}
	snap := Snapshot{TS: now}

	// --- host ---
	if b, err := c.io.ReadFile("/proc/meminfo"); err == nil {
		if m, err := parseMemInfo(string(b)); err == nil {
			snap.Host.MemTotalKiB = m.MemTotalKiB
			snap.Host.MemAvailableKiB = m.MemAvailableKiB
			snap.Host.MemUsedKiB = m.MemTotalKiB - m.MemAvailableKiB
			snap.Host.SwapTotalKiB = m.SwapTotalKiB
			snap.Host.SwapUsedKiB = m.SwapTotalKiB - m.SwapFreeKiB
			snap.Host.CachedKiB = m.CachedKiB
		}
	}
	snap.Host.Psi = map[string]PSI{}
	for _, res := range []string{"cpu", "memory", "io"} {
		if b, err := c.io.ReadFile("/proc/pressure/" + res); err == nil {
			if p, err := parsePSI(string(b)); err == nil {
				snap.Host.Psi[res] = p
			}
		}
	}
	if b, err := c.io.ReadFile("/proc/vmstat"); err == nil {
		in, out, maj := parseVMStat(string(b))
		if dt > 0 {
			snap.Host.SwapInPerS = rateU(in, c.prevVM.pswpin, dt)
			snap.Host.SwapOutPerS = rateU(out, c.prevVM.pswpout, dt)
			snap.Host.MajFaultPerS = rateU(maj, c.prevVM.majfault, dt)
		}
		c.prevVM = vmPrev{in, out, maj}
	}
	if b, err := c.io.ReadFile("/proc/stat"); err == nil {
		total, cores := parseProcStat(string(b))
		if len(c.prevCores) > 0 {
			snap.Host.CPU.PerCore = usageFrom(c.prevCores, cores)
			one := []cpuTimes{total}
			snap.Host.CPU.Total = usageFrom([]cpuTimes{c.prevStat}, one)[0]
		}
		c.prevStat, c.prevCores = total, cores
	}
	if l1, ok := loadAvg(); ok {
		snap.Host.Load1 = l1
	}
	if free, ok := c.io.StatFreeKB(c.cfg.HFCacheHost); ok {
		snap.Host.HFFreeKiB = free
	}

	// --- GPU (nvidia-smi is an exec; sample at half the cadence) ---
	if c.gpuAt.IsZero() || now.Sub(c.gpuAt) >= 2*c.cfg.Interval {
		c.gpuCached, c.gpuAt = c.io.GPU(ctx), now
	}
	snap.GPU = c.gpuCached

	// --- container cgroup ---
	if c.cfg.ContainerName != "" && (c.cgIDAt.IsZero() || now.Sub(c.cgIDAt) > 5*time.Second) {
		id, err := c.io.ContainerID(ctx)
		if err == nil {
			c.cgID, c.cgIDAt = id, now
		} else {
			c.cgIDAt = now
		}
	}
	if c.cgID != "" {
		cg := readCgroup(c.cgID)
		if cg.Found {
			cs := &ContainerState{ID: c.cgID, MemBytes: cg.MemCurrent}
			if dt > 0 && c.prevCG.seen {
				cs.CPUPerS = rateU(cg.CPUUsec, c.prevCG.cpuUsec, dt) / 1e6
				cs.ReadBPS = rateU(cg.ReadBytes, c.prevCG.readBytes, dt)
				cs.WriteBPS = rateU(cg.WriteBytes, c.prevCG.writeBytes, dt)
			}
			c.prevCG = cgPrev{cg.CPUUsec, cg.ReadBytes, cg.WriteBytes, true}
			snap.Container = cs
		}
	}

	// --- engine ---
	// When the engine is down there is nobody to ask — degrade the /metrics
	// scrape to every 5th tick (10 s) instead of hammering a dead port every
	// 2 s. /proc reads stay live because the box is always answering those.
	if base := cfgStr(c.cfg.EngineBase); base != "" {
		c.metTick++
		if c.metFail >= 3 && c.metTick%5 != 0 {
			snap.Engine.Reachable = false
		} else if text, err := c.io.Scrape(ctx, strings.TrimRight(base, "/")+"/metrics", cfgStr(c.cfg.EngineKey)); true {
		if err == nil {
			c.metFail = 0
			m, perr := ParseMetricsText(text)
			if perr == nil {
				c.fillEngine(&snap, m, dt)
			}
		} else {
			c.metFail++
			// engine down/loading: reset windows so rates restart clean
			c.prevEng = engPrev{}
			snap.Engine.Reachable = false
		}
		}
	}

	c.prevAt = now
	return snap
}

func (c *Collector) fillEngine(snap *Snapshot, m Metrics, dt float64) {
	e := &snap.Engine
	e.Reachable = true
	e.Running, _ = m.Gauge("vllm:num_requests_running")
	e.Waiting, _ = m.Gauge("vllm:num_requests_waiting")
	e.Swapped, _ = m.Gauge("vllm:num_requests_swapped")
	if kv, ok := m.Gauge("vllm:kv_cache_usage_perc"); ok {
		e.KVUsagePct = kv * 100
	}
	prompt, _ := m.Gauge("vllm:prompt_tokens_total")
	generation, _ := m.Gauge("vllm:generation_tokens_total")
	pq, _ := m.Gauge("vllm:prefix_cache_queries")
	ph, _ := m.Gauge("vllm:prefix_cache_hits")
	if dt > 0 && c.prevEng.seen {
		e.PromptTokPerS = rate(prompt, c.prevEng.prompt, dt)
		e.GenTokPerS = rate(generation, c.prevEng.generation, dt)
		dq := rate(pq, c.prevEng.prefixQ, dt)
		dh := rate(ph, c.prevEng.prefixH, dt)
		if dq > 0 {
			e.PrefixHitRatio = dh / dq
		}
	}
	// --- speculative decoding (MTP) counters ---
	sAcc, aOK := m.Gauge("vllm:spec_decode_num_accepted_tokens_total")
	sDrT, dtOK := m.Gauge("vllm:spec_decode_num_draft_tokens_total")
	sDr, dOK := m.Gauge("vllm:spec_decode_num_drafts_total")
	e.Spec = SpecStats{}
	if aOK && dtOK && dOK && dt > 0 && c.prevEng.seen && c.prevEng.specSeen {
		ra := rate(sAcc, c.prevEng.specAcc, dt)
		rdt := rate(sDrT, c.prevEng.specDraftTok, dt)
		rd := rate(sDr, c.prevEng.specDrafts, dt)
		if rdt > 0 || ra > 0 {
			e.Spec = SpecStats{Active: true, AcceptedPerS: ra, DraftedPerS: rdt}
			if rdt > 0 {
				e.Spec.AcceptancePct = 100 * ra / rdt
			}
			if rd > 0 {
				e.Spec.MeanTokens = ra / rd
			}
		}
	}
	c.prevEng = engPrev{prompt, generation, pq, ph, sAcc, sDrT, sDr, true, true}
	if q, ok := c.hist.Quantile(m, "vllm:time_to_first_token_seconds", 0.5); ok {
		e.TTFTP50 = q
	}
	if q, ok := c.hist.Quantile(m, "vllm:time_to_first_token_seconds", 0.9); ok {
		e.TTFTP90 = q
	}
	if q, ok := c.hist.Quantile(m, "vllm:inter_token_latency_seconds", 0.5); ok {
		e.ITLP50 = q
	}
	if q, ok := c.hist.Quantile(m, "vllm:inter_token_latency_seconds", 0.9); ok {
		e.ITLP90 = q
	}
	if q, ok := c.hist.Quantile(m, "vllm:e2e_request_latency_seconds", 0.5); ok {
		e.E2EP50 = q
	}
	if cur, ok := m.histSnap("vllm:time_to_first_token_seconds"); ok && len(cur.le) > 0 {
		e.TTFTSamples = int(cur.count[len(cur.count)-1])
	}
}

// rateU is rate() for uint64 counters.
func rateU(cur, prev uint64, dt float64) float64 {
	return rate(float64(cur), float64(prev), dt)
}

// rate computes per-second delta of a monotonic counter with reset tolerance.
func rate(cur, prev float64, dt float64) float64 {
	if dt <= 0 {
		return 0
	}
	if cur < prev { // counter reset (engine restart)
		return cur / dt
	}
	return (cur - prev) / dt
}

func cfgStr(f func() string) string {
	if f == nil {
		return ""
	}
	return f()
}
