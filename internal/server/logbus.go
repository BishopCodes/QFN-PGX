// Log bus: ONE `docker logs -f` pump per console process feeding a ring
// buffer + fan-out to SSE subscribers, plus the live boot-progress status
// model (phase/percent/ETA) derived from the same lines via BootTracker.
//
// Why: the old design let every open tab (and every EventSource reconnect —
// which fires every few seconds while the engine is down) spawn its own
// `docker logs -f`, replaying the container's ENTIRE history each time. That
// is the log "jumping" and the request storm that felt like pinging. Now the
// pump is single, lines are streamed once, reconnects get one bounded replay.
package server

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

const (
	ringLines  = 800   // replay ceiling per reconnect
	subBuf     = 256   // per-subscriber backlog before we drop + resync
	pumpIdle   = 2 * time.Second
	statusTick = 5 * time.Second
)

// Status is the push-side engine story: phase + percent + ETA.
type Status struct {
	Phase     string  `json:"phase"`      // down|created|loading weights|capturing cuda graphs|ready|failed
	Detail    string  `json:"detail"`     // e.g. "shards 8/19"
	Pct       float64 `json:"pct"`        // 0..100 heuristic
	EtaS      float64 `json:"eta_s"`      // projected remaining, 0 when unknown
	Running   bool    `json:"running"`    // container state
	StartedAt float64 `json:"started_at"` // boot epoch (for elapsed math client-side)
	FailHint  string  `json:"fail_hint,omitempty"`
}

type logBus struct {
	inspect func(ctx context.Context, name string) (engine.ContainerState, error)
	logs    func(ctx context.Context, name string, w io.Writer) error
	nameFn  func() string

	once sync.Once

	mu    sync.Mutex
	buf   []string
	subs  map[chan string]struct{}
	st    Status
	stChs map[chan Status]struct{}
}

func newLogBus(m *engine.Manager, nameFn func() string) *logBus {
	return &logBus{
		inspect: func(ctx context.Context, name string) (engine.ContainerState, error) {
			return engine.Inspect(ctx, m.Docker(), name)
		},
		logs:   m.Logs,
		nameFn: nameFn,
		subs:   map[chan string]struct{}{},
		stChs:  map[chan Status]struct{}{},
		st:     Status{Phase: "down"},
	}
}

// start launches the pump exactly once for the process lifetime.
func (b *logBus) start(ctx context.Context) {
	b.once.Do(func() { go b.run(ctx) })
}

func (b *logBus) run(ctx context.Context) {
	for ctx.Err() == nil {
		name := b.nameFn()
		st, err := b.inspect(ctx, name)
		up := err == nil && st.Running
		b.setRunning(up)
		if !up {
			if !sleepCtx(ctx, pumpIdle) {
				return
			}
			continue
		}
		bt := &engine.BootTracker{}
		pctx, cancel := context.WithCancel(ctx)
		pr, pw := io.Pipe()
		go func() { _ = b.logs(pctx, name, pw); _ = pw.Close() }()
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
		for sc.Scan() {
			line := sc.Text()
			ph, det := bt.Feed(line)
			b.publish(line, bt, ph, det)
			if ph == engine.PhaseFailed {
				break
			}
		}
		cancel()
		if ctx.Err() != nil {
			return
		}
		if !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// publish broadcasts a line and recomputes the status model.
func (b *logBus) publish(line string, bt *engine.BootTracker, ph engine.Phase, det string) {
	b.mu.Lock()
	b.buf = append(b.buf, line)
	if len(b.buf) > ringLines {
		b.buf = b.buf[len(b.buf)-ringLines:]
	}
	prev := b.st.Phase + "\x00" + b.st.Detail
	s := b.st
	s.Running = true
	s.Phase = ph.String()
	s.Detail = det
	el := time.Since(time.Unix(int64(s.StartedAt), 0)).Seconds()
	switch ph {
	case engine.PhaseCreated:
		s.Pct = 3
	case engine.PhaseWeights:
		s.Pct = 5 + shardFrac(det)*80
	case engine.PhaseGraphs:
		s.Pct = 88 + min(9, el/20) // graph capture ~180 s observed
	case engine.PhaseReady:
		s.Pct = 100
	case engine.PhaseFailed:
		s.Pct = min(s.Pct, 95)
		s.FailHint = det
	}
	if s.Pct > 5 && s.Pct < 100 {
		s.EtaS = el * (100 - s.Pct) / s.Pct
	} else {
		s.EtaS = 0
	}
	b.st = s
	changed := prev != s.Phase+"\x00"+s.Detail
	for c := range b.subs {
		select {
		case c <- line:
		default: // slow consumer: drop, they resync from the ring on next visit
		}
	}
	if changed {
		for c := range b.stChs {
			select {
			case c <- s:
			default:
			}
		}
	}
	b.mu.Unlock()
}

func (b *logBus) setRunning(up bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if up && !b.st.Running {
		// Fresh container run: reset the boot story so percentages start
		// from this stream, not the previous one.
		b.st = Status{Running: true, Phase: "created", Pct: 3,
			StartedAt: float64(time.Now().UnixMilli()) / 1000}
	} else if !up && b.st.Phase != "down" {
		b.st = Status{Phase: "down"}
	} else {
		return
	}
	for c := range b.stChs {
		select {
		case c <- b.st:
		default:
		}
	}
}

func (b *logBus) snapshot() (Status, []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := make([]string, len(b.buf))
	copy(cp, b.buf)
	return b.st, cp
}

func (b *logBus) subscribe() ([]string, func(), chan string) {
	ch := make(chan string, subBuf)
	b.mu.Lock()
	replay := make([]string, len(b.buf))
	copy(replay, b.buf)
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return replay, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}, ch
}

func (b *logBus) subscribeStatus() (Status, func(), chan Status) {
	ch := make(chan Status, 8)
	b.mu.Lock()
	cur := b.st
	b.stChs[ch] = struct{}{}
	b.mu.Unlock()
	return cur, func() {
		b.mu.Lock()
		delete(b.stChs, ch)
		b.mu.Unlock()
	}, ch
}

// statusTicker feeds periodic ETA refreshes to /api/events subscribers.
func (b *logBus) statusTicker() *time.Ticker { return time.NewTicker(statusTick) }

var shardRe = regexp.MustCompile(`shards (\d+)/(\d+)`)

func shardFrac(det string) float64 {
	m := shardRe.FindStringSubmatch(det)
	if m == nil {
		return 0
	}
	a, _ := strconv.Atoi(m[1])
	bb, _ := strconv.Atoi(m[2])
	if bb == 0 {
		return 0
	}
	return float64(a) / float64(bb)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ---- console self-restart (web button / qfn serve restart) ----

// restartMode picks the mechanism: under a systemd unit we exit and let
// Restart=always relaunch the (possibly freshly installed) binary; standalone
// we respawn a detached child first, then exit.
func restartMode(underSystemd bool) string {
	if underSystemd {
		return "systemd-relaunch"
	}
	return "respawn"
}
