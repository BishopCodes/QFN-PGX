package engine

import (
	"testing"
	"time"
)

// Zero/unparsable StartedAt (docker.go discards the parse error) must NOT read
// as "past the horizon" — that would print "ready" for a boot we cannot date.
func TestPastBootHorizon(t *testing.T) {
	if PastBootHorizon(time.Time{}) {
		t.Fatal("zero StartedAt must not count as past the horizon")
	}
	if !PastBootHorizon(time.Now().Add(-BootHorizon - time.Minute)) {
		t.Fatal("11 min uptime must be past the horizon")
	}
	if PastBootHorizon(time.Now().Add(-time.Minute)) {
		t.Fatal("1 min uptime must not be past the horizon")
	}
}

// Attaching to a long-running container seeds "ready" — ordinary request-log
// lines must not regress the phase.
func TestSeededTrackerHoldsReady(t *testing.T) {
	bt := NewBootTrackerSeeded(PhaseReady)
	for _, line := range []string{
		`INFO 09-07 12:00:01  loggers.py:259  POST /v1/chat/completions 200 OK`,
		"Adding 1 new requests. Avg. queue length: 0.2",
	} {
		if p, _ := bt.Feed(line); p != PhaseReady {
			t.Fatalf("seeded tracker regressed to %v on %q", p, line)
		}
	}
	// A genuine ready marker keeps it ready; a fresh traceback still trips it.
	if p, _ := bt.Feed("Application startup complete."); p != PhaseReady {
		t.Fatalf("ready marker: %v", p)
	}
	if p, _ := bt.Feed("Traceback (most recent call last):"); p != PhaseFailed {
		t.Fatalf("fail marker must still register: %v", p)
	}
}
