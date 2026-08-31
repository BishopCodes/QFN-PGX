package engine

import (
	"regexp"
	"strings"
)

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
