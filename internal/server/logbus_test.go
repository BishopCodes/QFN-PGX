package server

import (
	"encoding/json"
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

func TestConsoleRestartEndpoint(t *testing.T) {
	prev := consoleRestart
	called := ""
	consoleRestart = func(under bool) { called = restartMode(under) }
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
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && called == "" {
		time.Sleep(20 * time.Millisecond)
	}
	if called != "respawn" {
		t.Fatalf("restart hook not fired (%q)", called)
	}
}
