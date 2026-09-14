package engine

import (
	"regexp"
	"strings"
	"time"
)

// BootHorizon bounds the longest plausible cold boot on this lane (weights
// via mmap + graph capture). A container running meaningfully longer than
// this has booted — status readers may seed "ready" without log archaeology.
const BootHorizon = 10 * time.Minute

// PastBootHorizon reports whether a container has been up longer than any
// plausible boot, so its markers have scrolled out of the log tail and readers
// may seed "ready" without log archaeology. A zero StartedAt (docker inspect
// parse failed — docker.go discards that error) is NOT past the horizon: we
// cannot date the boot, so we must keep reading logs instead of declaring
// success on a container that may still be loading weights.
func PastBootHorizon(startedAt time.Time) bool {
	return !startedAt.IsZero() && time.Since(startedAt) > BootHorizon
}

// Boot-phase parsing for `status -w` and the web console. A cold boot is
// weights loading → graph capture → API server up, and the log lines below are
// the markers vLLM/the image emit (observed on this lane's startup).
type Phase int

const (
	PhaseCreated Phase = iota
	PhaseWeights
	PhaseGraphs
	PhaseReady
	PhaseFailed
)

func (p Phase) String() string {
	switch p {
	case PhaseCreated:
		return "starting"
	case PhaseWeights:
		return "loading weights"
	case PhaseGraphs:
		return "capturing cuda graphs"
	case PhaseReady:
		return "ready"
	case PhaseFailed:
		return "failed"
	}
	return "unknown"
}

// PhaseRank orders phases for monotonic tracking; Failed is terminal.
func (p Phase) rank() int {
	switch p {
	case PhaseCreated:
		return 0
	case PhaseWeights:
		return 1
	case PhaseGraphs:
		return 2
	case PhaseReady:
		return 3
	case PhaseFailed:
		return 4
	}
	return -1
}

var (
	weightsMarkers = []string{
		"Loading safetensors checkpoint shards",
		"init_engine",
		"Starting vLLM API server",
		"model weights take",
	}
	graphsMarkers = []string{
		"Capturing CUDA graph",
		"Capturing cudagraph",
		"capturing",
		"Graph capturing finished",
	}
	readyMarkers = []string{
		"Application startup complete", // uvicorn — the upstream "ready" signal
	}
	failMarkers = []string{
		"Traceback (most recent call last)",
		"OutOfMemoryError",
		"CUDA error:",
		"illegal memory access",
		"Engine core initialization failed",
	}

	shardProgress = regexp.MustCompile(`Completed \| (\d+)/(\d+)`)
)

// BootTracker folds boot log lines into a monotonic phase + progress detail.
// It is intentionally forgiving: unknown lines keep the current phase.
type BootTracker struct {
	phase  Phase
	detail string // e.g. "shards 8/19"
}

// NewBootTrackerSeeded returns a tracker pre-positioned at p — for attaching
// to a container that booted long before we listened, whose markers have
// scrolled out of the log tail. Without seeding such a session would report
// "starting" forever on a fully-serving engine.
func NewBootTrackerSeeded(p Phase) *BootTracker { return &BootTracker{phase: p} }

// Feed consumes one log line and reports the (possibly new) phase.
func (b *BootTracker) Feed(line string) (Phase, string) {
	lower := strings.ToLower(line)
	for _, m := range failMarkers {
		if strings.Contains(line, m) {
			b.phase, b.detail = PhaseFailed, m
			return b.phase, b.detail
		}
	}
	for _, m := range readyMarkers {
		if strings.Contains(line, m) {
			b.phase, b.detail = PhaseReady, m
			return b.phase, b.detail
		}
	}
	for _, m := range graphsMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			if b.phase.rank() < PhaseGraphs.rank() {
				b.phase, b.detail = PhaseGraphs, ""
			}
			return b.phase, b.detail
		}
	}
	for _, m := range weightsMarkers {
		if strings.Contains(line, m) {
			if b.phase.rank() < PhaseWeights.rank() {
				b.phase = PhaseWeights
			}
			if sm := shardProgress.FindStringSubmatch(line); sm != nil {
				b.detail = "shards " + sm[1] + "/" + sm[2]
			}
			return b.phase, b.detail
		}
	}
	return b.phase, b.detail
}

// Phase reports the current phase.
func (b *BootTracker) Phase() Phase { return b.phase }
