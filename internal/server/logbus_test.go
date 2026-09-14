package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

func TestLogBusReplayAndFanout(t *testing.T) {
	b := &logBus{subs: map[chan string]struct{}{}, stChs: map[chan Status]struct{}{}}
	// Pump-equivalent: publish directly (the pump wraps these same calls).
	for i := 0; i < 5; i++ {
		b.publish("line", &engine.BootTracker{}, engine.PhaseCreated, "")
	}
	replay, unsub, ch := b.subscribe()
	if len(replay) != 5 {
		t.Fatalf("replay: %d", len(replay))
	}
	defer unsub()
	b.publish("live", &engine.BootTracker{}, engine.PhaseCreated, "")
	if got := <-ch; got != "live" {
		t.Fatalf("live: %s", got)
	}
	// Slow consumer never blocks the pump.
	slow := make(chan string)
	b.mu.Lock()
	b.subs[slow] = struct{}{}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuf+10; i++ {
			b.publish("x", &engine.BootTracker{}, engine.PhaseCreated, "")
		}
		close(done)
	}()
	<-done
}

func TestLogBusStatusProgression(t *testing.T) {
	b := &logBus{subs: map[chan string]struct{}{}, stChs: map[chan Status]struct{}{}}
	bt := &engine.BootTracker{}
	b.publish("init_engine", bt, engine.PhaseWeights, "")
	st, _ := b.snapshot()
	if st.Pct < 5 || !strings.Contains(st.Phase, "weights") {
		t.Fatalf("weights phase: %+v", st)
	}
	b.publish("Completed | 8/16", bt, engine.PhaseWeights, "shards 8/16")
	st, _ = b.snapshot()
	if st.Pct < 40 || st.Pct > 50 {
		t.Fatalf("shard fraction → pct %v", st.Pct)
	}
	if st.EtaS <= 0 {
		t.Fatalf("eta must be positive mid-boot: %v", st.EtaS)
	}
	b.publish("Application startup complete", bt, engine.PhaseReady, "Application startup complete")
	st, _ = b.snapshot()
	if st.Pct != 100 || st.EtaS != 0 {
		t.Fatalf("ready: %+v", st)
	}
}

func TestShardFrac(t *testing.T) {
	if f := shardFrac("shards 4/16"); f < 0.24 || f > 0.26 {
		t.Fatalf("frac: %v", f)
	}
	if f := shardFrac(""); f != 0 {
		t.Fatalf("empty detail: %v", f)
	}
}

func TestRestartMode(t *testing.T) {
	if got := restartMode(true); got != "systemd-relaunch" {
		t.Fatalf("got %q", got)
	}
	if got := restartMode(false); got != "respawn" {
		t.Fatalf("got %q", got)
	}
}

// A container running far past BootHorizon must not strand the push-side
// phase at "starting" — its boot markers are long gone from the log tail.
// Regression test for "engine shows starting… while everything runs".
func TestLogBusSeedsReadyForLongRunning(t *testing.T) {
	b := &logBus{
		inspect: func(context.Context, string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: "running", Running: true,
				StartedAt: time.Now().Add(-90 * time.Minute)}, nil
		},
		logs: func(ctx context.Context, _ string, w io.Writer) error {
			fmt.Fprintln(w, `INFO: 127.0.0.1:55 - "POST /v1/chat/completions" 200 OK`)
			<-ctx.Done()
			return ctx.Err()
		},
		nameFn: func() string { return "eng" },
		subs:   map[chan string]struct{}{},
		stChs:  map[chan Status]struct{}{},
		st:     Status{Phase: "down"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	b.run(ctx) // bounded by ctx; pump publishes the seeded line
	st, _ := b.snapshot()
	if st.Phase != "ready" || !st.Running || st.Pct != 100 {
		t.Fatalf("long-running container must report ready/100, got %+v", st)
	}
}

// Same correction on the pull side: /api/engine/status must not claim
// "starting" for a long-running container just because the log replay found
// no markers.
func TestEngineStatusReadyPastHorizon(t *testing.T) {
	ts, _, dk, _ := newTestServer(t, nil)
	dk.inspectOut = fmt.Sprintf("running|%s|0001-01-01T00:00:00Z|0",
		time.Now().Add(-3*time.Hour).UTC().Format(time.RFC3339Nano))
	login(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/engine/status", nil)
	for _, c := range sessionCookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		Phase     string `json:"phase"`
		Reachable bool   `json:"reachable"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if got.Phase != "ready" {
		t.Fatalf("phase %q, want ready (body %s)", got.Phase, body)
	}
}

// A seeded session may swallow ONE fail marker (the stale one sitting in the
// replayed log tail). A second traceback is live and must flip the phase — a
// container can keep running with a dead engine inside it.
func TestLogBusSeededSwallowsOneFailMarker(t *testing.T) {
	lines := []string{
		"Traceback (most recent call last):", // stale, from the replayed tail
		`INFO: 127.0.0.1:55 - "POST /v1/chat/completions" 200 OK`,
		"Traceback (most recent call last):", // fresh — must register
	}
	seen := make(chan string, 16)
	stats := make(chan Status, 16)
	b := &logBus{
		inspect: func(context.Context, string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: "running", Running: true,
				StartedAt: time.Now().Add(-90 * time.Minute)}, nil
		},
		logs: func(ctx context.Context, _ string, w io.Writer) error {
			for _, l := range lines {
				fmt.Fprintln(w, l)
			}
			<-ctx.Done()
			return ctx.Err()
		},
		nameFn: func() string { return "eng" },
		subs:   map[chan string]struct{}{seen: {}},
		stChs:  map[chan Status]struct{}{stats: {}},
		st:     Status{Phase: "down"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	b.run(ctx)

	got := []string{}
	for {
		select {
		case s := <-stats:
			got = append(got, s.Phase)
			continue
		default:
		}
		break
	}
	ready, failed := -1, -1
	for i, p := range got {
		if p == "ready" && ready < 0 {
			ready = i
		}
		if p == "failed" {
			failed = i
		}
	}
	if failed < 0 || ready < 0 || ready > failed {
		t.Fatalf("phase must go ready then failed, got %v", got)
	}
	if got[len(got)-1] != "failed" {
		t.Fatalf("must end failed, got %v", got)
	}
	b.mu.Lock()
	n := len(b.buf)
	b.mu.Unlock()
	if n != len(lines) {
		t.Fatalf("all %d lines must stream (stopped early at the stale one?): %d", len(lines), n)
	}
}

// The swallow budget belongs to the container run, not to the log stream: a
// pipe that ends and re-attaches replays the SAME tail, and if the budget came
// back with it, a dead engine would flap failed→ready every pump cycle.
func TestLogBusSeededSwallowSurvivesReattach(t *testing.T) {
	stats := make(chan Status, 64)
	started := time.Now().Add(-90 * time.Minute) // one container run: fixed start
	b := &logBus{
		inspect: func(context.Context, string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: "running", Running: true, StartedAt: started}, nil
		},
		logs: func(_ context.Context, _ string, w io.Writer) error {
			fmt.Fprintln(w, "Traceback (most recent call last):") // stale tail
			return nil                                            // ends at once: forces a re-attach
		},
		nameFn: func() string { return "eng" },
		idle:   10 * time.Millisecond, // re-attach promptly
		stChs:  map[chan Status]struct{}{stats: {}},
		st:     Status{Phase: "down"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	b.run(ctx)

	if !b.seedSwallow {
		t.Fatal("the replayed traceback must have spent the run's swallow budget")
	}
	ready, failed, readyAfterFail := 0, 0, false
	for {
		select {
		case s := <-stats:
			if s.Phase == "ready" {
				ready++
				if failed > 0 {
					readyAfterFail = true
				}
			}
			if s.Phase == "failed" {
				failed++
			}
			continue
		default:
		}
		break
	}
	if readyAfterFail || ready != 1 || failed == 0 {
		t.Fatalf("must go ready once then stay failed: ready=%d failed=%d", ready, failed)
	}
}

// ...and the budget must RESET for a new container run: the tail of a fresh
// container can carry a traceback from the run before it, and honouring that
// would open every `qfn up` with a false failure.
func TestLogBusSwallowBudgetResetsOnNewRun(t *testing.T) {
	started := time.Now().Add(-90 * time.Minute) // this run
	stats := make(chan Status, 8)
	b := &logBus{
		inspect: func(context.Context, string) (engine.ContainerState, error) {
			return engine.ContainerState{Status: "running", Running: true, StartedAt: started}, nil
		},
		logs: func(_ context.Context, _ string, w io.Writer) error {
			fmt.Fprintln(w, "Traceback (most recent call last):") // stale, from the PREVIOUS run
			return nil
		},
		nameFn: func() string { return "eng" },
		idle:   10 * time.Millisecond,
		stChs:  map[chan Status]struct{}{stats: {}},
		st:     Status{Phase: "down"},
		// A budget already spent by the run before this one.
		seedRun:     time.Now().Add(-6 * time.Hour),
		seedSwallow: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	b.run(ctx)

	if !b.seedRun.Equal(started) {
		t.Fatalf("the budget must be re-armed for this run: seedRun=%v want %v", b.seedRun, started)
	}
	first := ""
	for {
		select {
		case s := <-stats:
			if first == "" {
				first = s.Phase
			}
			continue
		default:
		}
		break
	}
	// The run opens SWALLOWED (ready): a brand-new container must not be born
	// failed by a traceback from the run before it. Later re-sightings of that
	// line stay live — see TestLogBusSeededSwallowSurvivesReattach.
	if first != "starting" && first != "ready" {
		t.Fatalf("a new run's stale tail must not read failed at first sight (first=%q)", first)
	}
	if first == "failed" {
		t.Fatalf("new run opened failed: %v", first)
	}
}

func TestConsoleRestartEndpoint(t *testing.T) {
	prev := consoleRestart
	fired := make(chan string, 1) // hook fires from the handler's goroutine
	consoleRestart = func(under bool) { fired <- restartMode(under) }
	defer func() { consoleRestart = prev }()

	ts, _, _, _ := newTestServer(t, nil)
	login(t, ts)
	res := mustPost(t, ts, "/api/console/restart", `{}`)
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d: %s", res.StatusCode, res.body)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(res.body), &body)
	if body["mode"] != "respawn" { // tests never run under systemd
		t.Fatalf("body: %v", body)
	}
	var mode string
	select {
	case mode = <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("restart hook never fired")
	}
	if mode != "respawn" {
		t.Fatalf("restart hook got %q", mode)
	}
}
