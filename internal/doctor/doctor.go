// Package doctor validates the machine before anything heavy runs, and powers
// the launch preflight shared by `qfn up` and the web console: the guard that
// refuses to start a second boot when the unified-memory pool can't hold the
// model is exactly the "GB10 memory trap" lesson from dgx-spark-qwen38.
package doctor

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	engineassets "github.com/BishopCodes/qfn-pgx/engine"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

// Check is one verdict.
type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok|warn|bad
	Msg    string `json:"msg"`
	Hint   string `json:"hint,omitempty"`
}

// Deps wires what doctor touches.
type Deps struct {
	Cfg    func() config.Config
	Docker engine.Docker
	Status func(config.Engine) engine.Status // snapshot/hybrid probe
}

// Run executes the full battery (quick=true skips network/probe-heavy checks).
func Run(ctx context.Context, d Deps, quick bool) []Check {
	cfg := d.Cfg()
	var out []Check
	add := func(c Check) { out = append(out, c) }

	// docker daemon
	ver, err := d.Docker.Run(ctx, "version", "--format", "{{.Server.Version}}")
	switch {
	case err != nil:
		add(Check{ID: "docker.daemon", Status: "bad", Msg: "docker not reachable: " + errOr(err, ver), Hint: "is docker installed and the daemon running?"})
	default:
		add(Check{ID: "docker.daemon", Status: "ok", Msg: "docker " + strings.TrimSpace(ver)})
	}

	// NVIDIA container runtime
	info, err := d.Docker.Run(ctx, "info", "--format", "{{json .Runtimes}}")
	if err == nil && strings.Contains(info, "nvidia") {
		add(Check{ID: "docker.gpuruntime", Status: "ok", Msg: "nvidia container runtime present"})
	} else {
		add(Check{ID: "docker.gpuruntime", Status: "bad", Msg: "nvidia runtime not in docker info", Hint: "install the NVIDIA Container Toolkit — the lane cannot start without it"})
	}

	// engine image (+ base digest when not quick)
	exists, digest, err := engine.ImageExists(ctx, d.Docker, cfg.Engine.Image)
	switch {
	case err != nil:
		add(Check{ID: "engine.image", Status: "warn", Msg: "image probe failed: " + err.Error()})
	case !exists:
		add(Check{ID: "engine.image", Status: "bad", Msg: "image " + cfg.Engine.Image + " not built", Hint: "run `qfn build`"})
	default:
		msg := "image " + cfg.Engine.Image + " present"
		if digest != "" {
			msg += " (" + digest + ")"
		}
		add(Check{ID: "engine.image", Status: "ok", Msg: msg})
	}
	if !quick {
		if base := engineassets.BaseImageRef(); base != "" {
			if _, err := d.Docker.Run(ctx, "image", "inspect", base); err != nil {
				add(Check{ID: "engine.base_digest", Status: "warn", Msg: "pinned base " + shorten(base) + " not local", Hint: "build pulls it (~30 GB) or `docker pull` it first"})
			} else {
				add(Check{ID: "engine.base_digest", Status: "ok", Msg: "pinned base digest present: " + engineassets.Digest()})
			}
		}
	}

	// snapshot & hybrid
	st := d.Status(cfg.Engine)
	switch {
	case !st.RepoExists:
		add(Check{ID: "checkpoint", Status: "bad", Msg: "no checkpoint snapshot", Hint: "run `qfn pull`"})
	default:
		add(Check{ID: "checkpoint", Status: "ok", Msg: "snapshot " + st.Snapshot})
		switch cfg.Engine.Mode {
		case "hybrid":
			if st.HybridPrepared {
				add(Check{ID: "checkpoint.hybrid", Status: "ok", Msg: "hybrid variant prepared"})
			} else {
				add(Check{ID: "checkpoint.hybrid", Status: "bad", Msg: "mode=hybrid but not prepared", Hint: "run `qfn prepare-hybrid` (~10 min)"})
			}
		default:
			if st.HybridPrepared {
				add(Check{ID: "checkpoint.hybrid", Status: "ok", Msg: "hybrid variant available (switch with a profile: mode=hybrid)"})
			}
		}
	}

	// disk free under the HF cache (upstream: ~130 GB nvfp4, +13 GB hybrid)
	if free, ok := statFreeKB(config.ExpandHome(cfg.Paths.HFCache)); ok {
		gib := free / 1048576
		need := uint64(130)
		if cfg.Engine.Mode == "hybrid" {
			need = 143
		}
		if gib >= need {
			add(Check{ID: "disk", Status: "ok", Msg: fmt.Sprintf("%d GiB free under %s", gib, cfg.Paths.HFCache)})
		} else {
			add(Check{ID: "disk", Status: "warn", Msg: fmt.Sprintf("only %d GiB free (want ≥ %d GiB for this mode)", gib, need)})
		}
	}

	// memory posture (read-only report; the hard refusal lives in Preflight)
	mm, err := readMemInfo()
	if err != nil {
		add(Check{ID: "memory", Status: "warn", Msg: "cannot read /proc/meminfo: " + err.Error()})
	} else {
		avail := mm.MemAvailableKiB / 1048576
		swapUsedPct := 0.0
		if mm.SwapTotalKiB > 0 {
			swapUsedPct = 100 * float64(mm.SwapTotalKiB-mm.SwapFreeKiB) / float64(mm.SwapTotalKiB)
		}
		status := "ok"
		msg := fmt.Sprintf("%d GiB available of %d GiB pool; swap %.0f%% used",
			avail, mm.MemTotalKiB/1048576, swapUsedPct)
		hint := ""
		if swapUsedPct > 20 {
			status = "warn"
			msg += " — swap already busy; a boot may thrash"
			hint = "close other tenants or reboot before launching"
		}
		add(Check{ID: "memory", Status: status, Msg: msg, Hint: hint})
	}

	// PSI one-shot
	psiMsg := []string{}
	for _, res := range []string{"cpu", "memory", "io"} {
		b, err := os.ReadFile("/proc/pressure/" + res)
		if err != nil {
			continue
		}
		var some float64
		if _, err := fmt.Sscanf(firstLine(string(b)), "some avg10=%f", &some); err == nil && some > 5 {
			psiMsg = append(psiMsg, fmt.Sprintf("%s some avg10=%.1f", res, some))
		}
	}
	if len(psiMsg) == 0 {
		add(Check{ID: "pressure", Status: "ok", Msg: "PSI nominal"})
	} else {
		add(Check{ID: "pressure", Status: "warn", Msg: "stalled: " + strings.Join(psiMsg, ", ")})
	}

	if !quick {
		// GPU telemetry availability
		if _, err := execLookPath("nvidia-smi"); err != nil {
			add(Check{ID: "gpu.telemetry", Status: "warn", Msg: "nvidia-smi missing — dashboard shows host-only GPU panels"})
		} else {
			add(Check{ID: "gpu.telemetry", Status: "ok", Msg: "nvidia-smi present (util/power panels active; memory stays host-side on GB10)"})
		}
	}
	sortChecks(out)
	return out
}

func sortChecks(out []Check) {
	order := map[string]int{"bad": 0, "warn": 1, "ok": 2}
	sort.SliceStable(out, func(i, j int) bool {
		return order[out[i].Status] < order[out[j].Status]
	})
}

// Preflight is the launch gate used by `qfn up` and POST /api/engine/up.
// ErrUnsafe marks a refusal that `--yes-i-know` can override.
type ErrUnsafe struct{ Reason string }

func (e *ErrUnsafe) Error() string { return "memory guard: " + e.Reason }

// Preflight estimates whether the unified pool can take this launch. It is
// deliberately conservative: vLLM's accounting does not see every GB10
// transient allocation (the upstream "memory trap"), so we budget gpu_mem×pool
// plus an 8 GiB safety margin against MemAvailable.
func Preflight(ctx context.Context, cfg config.Config, e config.Engine, override bool) error {
	if override {
		return nil
	}
	mm, err := readMemInfo()
	if err != nil {
		return nil // cannot measure → don't block (doctor reports it)
	}
	poolKiB := mm.MemTotalKiB
	needKiB := uint64(float64(poolKiB)*e.GpuMem) + 8*1048576
	if mm.MemAvailableKiB < needKiB {
		return &ErrUnsafe{Reason: fmt.Sprintf(
			"launch needs ~%d GiB (gpu_mem %.2f × %d GiB pool + 8 GiB margin) but only %d GiB available — something else is eating the pool; use --yes-i-know only if you know better than this guard",
			needKiB/1048576, e.GpuMem, poolKiB/1048576, mm.MemAvailableKiB/1048576)}
	}
	return nil
}

// ---- tiny env helpers (seams for tests) ----

var (
	statFreeKB   = defaultStatFreeKB
	execLookPath = lookPath
)

func errOr(err error, out string) string {
	if err != nil {
		return err.Error()
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func shorten(s string) string {
	if len(s) > 40 {
		return s[:34] + "…" + s[len(s)-5:]
	}
	return s
}
