// CLI-side assembly of the vision doctor check: locates the snapshot's
// config.json, grabs the live container argv, and runs the 1×1 probe.
package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
)

func (a *App) visionCheck(ctx context.Context) *doctor.Check {
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
