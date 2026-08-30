package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BishopCodes/qfn-pgx/internal/config"
)

// SnapshotLocator resolves the in-container snapshot path (SNAP_IN in
// serve.sh) and the hybrid-mode env vars.
type SnapshotLocator interface {
	SnapshotInContainer(e config.Engine) (snapIn string, hybridEnv []string, err error)
}

// NewSnapshotLocator returns a locator rooted at the expanded host HF cache.
func NewSnapshotLocator(hfCacheHost string) SnapshotLocator {
	return &pathLocator{hfCacheHost: filepath.Clean(hfCacheHost)}
}

// pathLocator mirrors serve.sh: $HF_CACHE/hub/models--<org>--<name>/snapshots,
// where hybrid mode lives in "<revision>-fp8hybrid" and must carry .prepared.
type pathLocator struct{ hfCacheHost string }

func (p *pathLocator) repoDir(e config.Engine) string {
	model := strings.ReplaceAll(e.Model, "/", "--")
	return filepath.Join(p.hfCacheHost, "hub", "models--"+model)
}

func (p *pathLocator) SnapshotInContainer(e config.Engine) (string, []string, error) {
	dir := filepath.Join(p.repoDir(e), "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		return "", nil, fmt.Errorf("checkpoint not found under %s — run `qfn pull` first", dir)
	}
	var names []string
	for _, ent := range entries {
		if ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	snap := ""
	for _, n := range names {
		if !strings.HasSuffix(n, "-fp8hybrid") {
			snap = n
			break
		}
	}
	if snap == "" {
		return "", nil, fmt.Errorf("no plain snapshot under %s — run `qfn pull`", dir)
	}
	switch e.Mode {
	case "nvfp4":
	case "hybrid":
		hyb := snap + "-fp8hybrid"
		if _, err := os.Stat(filepath.Join(dir, hyb, ".prepared")); err != nil {
			return "", nil, fmt.Errorf("hybrid checkpoint not prepared: run `qfn prepare-hybrid` first (one-time, ~10 min)")
		}
		snap = hyb
	default:
		return "", nil, fmt.Errorf("unknown engine mode %q", e.Mode)
	}
	model := strings.ReplaceAll(e.Model, "/", "--")
	snapIn := "/hf/hub/models--" + model + "/snapshots/" + snap
	var hybridEnv []string
	if e.Mode == "hybrid" {
		hybridEnv = []string{"-e", "VLLM_FP8_HYBRID=1", "-e", "VLLM_USE_DEEP_GEMM=0"}
	}
	return snapIn, hybridEnv, nil
}

// Status is consumed by `qfn doctor`.
type Status struct {
	RepoExists     bool
	Snapshot       string // revision dir name, "" if none
	HybridPrepared bool
}

// Status reports snapshot presence without launching anything.
func (p *pathLocator) Status(e config.Engine) Status {
	var st Status
	dir := filepath.Join(p.repoDir(e), "snapshots")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return st
	}
	st.RepoExists = true
	var names []string
	for _, ent := range entries {
		if ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if !strings.HasSuffix(n, "-fp8hybrid") && st.Snapshot == "" {
			st.Snapshot = n
		}
	}
	if st.Snapshot != "" {
		if _, err := os.Stat(filepath.Join(dir, st.Snapshot+"-fp8hybrid", ".prepared")); err == nil {
			st.HybridPrepared = true
		}
	}
	return st
}

// StatusOf adapts an arbitrary SnapshotLocator for doctor callers that only
// have the interface. Concrete *pathLocator callers should use Status directly.
func StatusOf(loc SnapshotLocator, e config.Engine) Status {
	if pl, ok := loc.(*pathLocator); ok {
		return pl.Status(e)
	}
	// Fallback: probe through the interface in both modes.
	var st Status
	if _, _, err := loc.SnapshotInContainer(modeOf(e, "nvfp4")); err == nil {
		st.RepoExists, st.Snapshot = true, "unknown"
	}
	if _, _, err := loc.SnapshotInContainer(modeOf(e, "hybrid")); err == nil {
		st.HybridPrepared = true
	}
	return st
}

func modeOf(e config.Engine, mode string) config.Engine {
	e.Mode = mode
	return e
}
