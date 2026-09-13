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
)

func (a *App) visionCheck(ctx context.Context, images []int) *doctor.Check {
	hasVision := false
	if snapIn, _, err := a.Locator().SnapshotInContainer(a.Cfg.Engine); err == nil {
		host := filepath.Join(config.ExpandHome(a.Cfg.Paths.HFCache), strings.TrimPrefix(snapIn, "/hf"))
		if b, err := os.ReadFile(filepath.Join(host, "config.json")); err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				_, hasVision = m["vision_config"]
			}
		}
	}
	ch := doctor.VisionCheck(ctx, doctor.VisionDeps{
		Base:              func() string { return a.EngineBaseURL() },
		Key:               func() string { return a.engineKeyOnly() },
		Model:             a.Cfg.Engine.Model,
		Args:              containerArgs(ctx, a),
		SnapshotHasVision: hasVision,
		Images:            images,
		Post:              doctor.HTTPPost,
	})
	return &ch
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
