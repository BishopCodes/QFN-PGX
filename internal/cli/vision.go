// CLI-side assembly of the vision doctor check: locates the snapshot's
// config.json, grabs the live container argv, and runs the 1×1 probe.
package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

func (a *App) visionCheck(ctx context.Context, images []int) *doctor.Check {
	// One snapshot probe, shared with doctor: both facts come from the same
	// locator every other check uses — including whether config.json was
	// readable at all, which must never be reported as "no vision tower".
	mm := doctor.SnapshotMMAt(engine.SnapshotHostDir(a.Locator(), a.Cfg.Engine))
	ch := doctor.VisionCheck(ctx, doctor.VisionDeps{
		Base:              func() string { return a.EngineBaseURL() },
		Key:               func() string { return a.engineKeyOnly() },
		Args:              containerArgs(ctx, a),
		Locator:           a.Locator(),
		Engine:            a.Cfg.Engine,
		SnapshotHasVision: mm.Tower,
		SnapshotReadable:  mm.Config,
		Images:            images,
		Post:              doctor.HTTPPost,
		// Both seams exist to answer "registry wiring or checkpoint files?"
		// with the engine's own words instead of a guess: /v1/models for the
		// declared modalities, the container log for the traceback the HTTP
		// 400 never carries.
		Modalities: func(ctx context.Context) ([]string, bool) {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return doctor.DeclaredModalities(cctx, a.EngineBaseURL(), a.engineKeyOnly())
		},
		Logs: func(ctx context.Context) string { return containerLogTail(ctx, a) },
	})
	return &ch
}

// containerLogTail is the engine's last words — where a Python traceback
// names the class or processor that failed. `docker logs` merges the
// container's stderr into stdout, so the traceback survives the seam; the
// tail stays short because only the exception line matters.
func containerLogTail(ctx context.Context, a *App) string {
	out, err := a.Docker.Run(ctx, "logs", "--tail", "40", a.Cfg.Engine.Name)
	if err != nil && out == "" {
		return ""
	}
	return out
}

// containerArgs returns the running engine's argv (entrypoint args + cmd);
// empty when the container isn't up or docker refuses.
func containerArgs(ctx context.Context, a *App) []string {
	out, err := a.Docker.Run(ctx, "inspect", "-f", "{{json .Args}}|{{json .Config.Cmd}}", a.Cfg.Engine.Name)
	if err != nil {
		return nil
	}
	var all []string
	for _, part := range strings.SplitN(strings.TrimSpace(out), "|", 2) {
		var arr []string
		if json.Unmarshal([]byte(part), &arr) == nil {
			all = append(all, arr...)
		}
	}
	return all
}

// writeVisionReceipt persists a doctor vision-matrix run under
// <state_dir>/vision-receipts/ — the artifact a matrix run exists to produce.
func writeVisionReceipt(a *App, ch doctor.Check, images []int) {
	dir := filepath.Join(config.ExpandHome(a.Cfg.Paths.StateDir), "vision-receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		warnf("vision receipt not saved: %v", err)
		return
	}
	rec := struct {
		Time   string       `json:"time"`
		Model  string       `json:"model"`
		Images []int        `json:"images_probed"`
		Check  doctor.Check `json:"check"`
	}{time.Now().UTC().Format(time.RFC3339), a.Cfg.Engine.Model, images, ch}
	b, _ := json.MarshalIndent(rec, "", "  ")
	p := filepath.Join(dir, "vision-"+time.Now().UTC().Format("20060102T150405Z")+".json")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		warnf("vision receipt not saved: %v", err)
		return
	}
	dimf("vision receipt: %s", p)
}
