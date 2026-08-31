package cli

// Operator tools: doctor, bench, chat, stats, and the asset-driven image/
// checkpoint commands (build/pull/prepare-hybrid) — the last three stream
// docker output directly since they run minutes-to-hours long.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	engineassets "github.com/BishopCodes/qfn-pgx/engine"
	"github.com/BishopCodes/qfn-pgx/internal/bench"
	"github.com/BishopCodes/qfn-pgx/internal/chat"
	"github.com/BishopCodes/qfn-pgx/internal/collector"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

func addTools(root *cobra.Command, app *App) {
	// ---- doctor ----
	doc := &cobra.Command{
		Use:   "doctor", Short: "Pre-flight the machine (docker, runtime, checkpoint, memory, pressure)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			quick, _ := c.Flags().GetBool("quick")
			asJSON, _ := c.Flags().GetBool("json")
			checks := doctor.Run(c.Context(), doctor.Deps{
				Cfg:    func() config.Config { return app.Cfg },
				Docker: app.Docker,
				Status: func(e config.Engine) engine.Status { return engine.StatusOf(app.Locator(), e) },
			}, quick)
			checks = append(checks, doctor.ServiceState(c.Context())...)
			if !quick {
				if v := app.visionCheck(c.Context()); v != nil {
					checks = append(checks, *v)
				}
			}
			if asJSON {
				b, _ := json.MarshalIndent(checks, "", "  ")
				fmt.Println(string(b))
			} else {
				for _, ch := range checks {
					color := ansiOK
					switch ch.Status {
					case "warn":
						color = "\x1b[33m"
					case "bad":
						color = ansiBad
					}
					fmt.Println(styled(fmt.Sprintf("  %-4s %-20s %s", ch.Status, ch.ID, ch.Msg), color))
					if ch.Hint != "" {
						dimf("       → %s", ch.Hint)
					}
				}
			}
			for _, ch := range checks {
				if ch.Status == "bad" {
					return errors.New("doctor: failing checks above")
				}
			}
			return nil
		},
	}
	doc.Flags().Bool("quick", false, "skip slow probes (base-image inspect)")
	doc.Flags().Bool("json", false, "machine-readable")
	root.AddCommand(doc)

	// ---- bench ----
	bc := &cobra.Command{
		Use:   "bench", Short: "Benchmark via the front door (proxy preferred) or the engine directly",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			base, _ := c.Flags().GetString("base")
			key, _ := c.Flags().GetString("key")
			model, _ := c.Flags().GetString("model")
			jsonOut, _ := c.Flags().GetBool("json")
			needle, _ := c.Flags().GetInt("needle")
			var probes []string
			for _, f := range []string{"ttft", "prefill", "determinism", "decode", "battery"} {
				if v, _ := c.Flags().GetBool(f); v {
					probes = append(probes, f)
				}
			}
			client := bench.NewClient(base, key)
			if base == "" {
				client.Base = app.benchBase()
				client.Key = app.benchKey()
			}
			if model != "" {
				client.Model = model
			}
			var results []bench.Result
			run := func(name string) {
				switch name {
				case "ttft":
					results = append(results, client.TTFT(c.Context())...)
				case "prefill":
					results = append(results, client.Prefill(c.Context())...)
				case "determinism":
					results = append(results, client.Determinism(c.Context())...)
				case "decode":
					results = append(results, client.Decode(c.Context())...)
				}
			}
			for _, p := range probes {
				run(p)
			}
			if len(probes) == 0 {
				results = client.Battery(c.Context(), needle)
			}
			if needle > 0 && len(probes) > 0 {
				results = append(results, client.Needle(c.Context(), needle, 50)...)
			}
			if jsonOut {
				b, _ := json.MarshalIndent(results, "", "  ")
				fmt.Println(string(b))
			} else {
				for _, r := range results {
					mark := "✓"
					color := ansiOK
					if !r.OK {
						mark, color = "✗", ansiBad
					}
					line := fmt.Sprintf("  %s %-12s %s", mark, r.Probe, r.Detail)
					if r.Error != "" {
						line = fmt.Sprintf("  %s %-12s %s", mark, r.Probe, r.Error)
					}
					fmt.Println(styled(line, color))
					if r.Value > 0 {
						dimf("      %.2f %s", r.Value, r.Unit)
					}
				}
			}
			app.appendBenchHistory(results)
			return nil
		},
	}
	for _, f := range []string{"ttft", "prefill", "determinism", "decode", "battery"} {
		bc.Flags().Bool(f, false, "run just the "+f+" probe")
	}
	bc.Flags().Int("needle", 0, "needle-in-haystack at N words context (0=off)")
	bc.Flags().String("base", "", "endpoint override (default: console, else engine)")
	bc.Flags().String("key", "", "bearer override (default: front/engine key)")
	bc.Flags().String("model", "", "model name override")
	bc.Flags().Bool("json", false, "machine-readable")
	root.AddCommand(bc)

	// ---- chat ----
	ch := &cobra.Command{
		Use:   "chat", Short: "Interactive REPL through the console proxy (dashboard sees it)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			noThink, _ := c.Flags().GetBool("no-think")
			sys, _ := c.Flags().GetString("system")
			direct, _ := c.Flags().GetBool("direct")
			img, _ := c.Flags().GetString("image")
			opts := chat.Options{NoThink: noThink, System: sys, Image: img}
			if direct {
				opts.Base = app.EngineBaseURL()
				opts.Key = app.engineKeyOnly()
			} else {
				opts.Base = app.ServeBaseURL()
				opts.Key = app.FrontKey()
				if !app.consoleUp(c.Context()) {
					return errors.New("console not reachable on " + opts.Base + " — start `qfn serve` or use --direct")
				}
			}
			return chat.Run(c.Context(), opts)
		},
	}
	ch.Flags().Bool("no-think", false, "disable reasoning mode (chat_template_kwargs.enable_thinking=false)")
	ch.Flags().String("system", "", "system prompt")
	ch.Flags().Bool("direct", false, "bypass the console and hit the engine directly")
	ch.Flags().String("image", "", "attach an image to your first message (vision probe — errors show why if the engine declines)")
	root.AddCommand(ch)

	// ---- stats ----
	st := &cobra.Command{
		Use:   "stats", Short: "One-shot metrics snapshot (host + engine)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			watch, _ := c.Flags().GetBool("watch")
			asJSON, _ := c.Flags().GetBool("json")
			col := collector.New(collector.Config{
				EngineBase:    func() string { return app.EngineBaseURL() },
				EngineKey:     func() string { return app.engineKeyOnly() },
				ContainerName: app.Cfg.Engine.Name,
				HFCacheHost:   config.ExpandHome(app.Cfg.Paths.HFCache),
				Interval:      2 * time.Second,
			}, collector.IO{})
			render := func() {
				snap := col.SampleOnce(c.Context())
				if asJSON {
					b, _ := json.Marshal(snap)
					fmt.Println(string(b))
					return
				}
				fmt.Print(styled(fmt.Sprintf("── %s ──\n", time.Now().Format("15:04:05")), ansiAcc))
				h, e, g := snap.Host, snap.Engine, snap.GPU
				fmt.Printf("  pool %d/%d GiB · swap %.0f%% · psi mem %v io %v · load %v\n",
					(h.MemTotalKiB-h.MemAvailableKiB)/1048576, h.MemTotalKiB/1048576,
					swapPctOf(h), psiAvg(h, "memory"), psiAvg(h, "io"), h.Load1)
				if g.Available {
					fmt.Printf("  gpu %v%% · %v W · %v°C\n", g.UtilPct, g.PowerW, g.TempC)
				}
				if e.Reachable {
					fmt.Printf("  engine up · run %v wait %v · kv %v%% · gen %v tok/s · ttft p50 %vs\n",
						e.Running, e.Waiting, e.KVUsagePct, e.GenTokPerS, e.TTFTP50)
				} else {
					fmt.Println("  engine not reachable")
				}
			}
			if !watch {
				col.SampleOnce(c.Context()) // warm counters
				time.Sleep(300 * time.Millisecond)
				render()
				return nil
			}
			col.SampleOnce(c.Context())
			for {
				render()
				select {
				case <-c.Context().Done():
					return nil
				case <-time.After(2 * time.Second):
				}
			}
		},
	}
	st.Flags().BoolP("watch", "w", false, "refresh every 2s")
	st.Flags().Bool("json", false, "machine-readable")
	root.AddCommand(st)

	// ---- build ----
	root.AddCommand(&cobra.Command{
		Use:   "build", Short: "Build the engine image from the embedded (vendored) Dockerfile",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			tmp, err := os.MkdirTemp("", "qfn-build-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			ctxDir, err := engineassets.WriteToDisk(tmp)
			if err != nil {
				return err
			}
			fmt.Printf("  context %s\n  base %s\n", ctxDir, engineassets.BaseImageRef())
			return streamDocker(c, "build", "-t", app.Cfg.Engine.Image, ctxDir)
		},
	})

	// ---- pull ----
	root.AddCommand(&cobra.Command{
		Use:   "pull", Short: "Download the checkpoint into the HF cache (resumable, ~122 GiB)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			hf := config.ExpandHome(app.Cfg.Paths.HFCache)
			if err := os.MkdirAll(hf, 0o755); err != nil {
				return err
			}
			args := []string{"run", "--rm", "--name", "qfn-pull",
				"-e", "HF_HOME=/hf", "-e", "HF_HUB_DISABLE_XET=1"}
			if tok := os.Getenv("HF_TOKEN"); tok != "" {
				args = append(args, "-e", "HF_TOKEN")
			} else if tok := os.Getenv("HUGGING_FACE_HUB_TOKEN"); tok != "" {
				args = append(args, "-e", "HUGGING_FACE_HUB_TOKEN", "-e", "HF_TOKEN="+tok)
			} else {
				warnf("no HF_TOKEN in env — Hub will rate-limit unauthenticated downloads")
			}
			args = append(args, "-v", hf+":/hf", "--entrypoint", "bash", app.Cfg.Engine.Image,
				"-c", fmt.Sprintf("hf download '%s' --max-workers 8", app.Cfg.Engine.Model))
			dimf("resumable — safe to Ctrl+C and re-run; ~130 GiB free needed under %s", hf)
			return streamDocker(c, args...)
		},
	})

	// ---- prepare-hybrid ----
	root.AddCommand(&cobra.Command{
		Use:   "prepare-hybrid", Short: "One-time fp8 side-layer conversion (~10 min, ~13 GiB extra disk)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			e := app.Cfg.Engine
			st := engine.StatusOf(app.Locator(), e)
			if !st.RepoExists || st.Snapshot == "" {
			return errors.New("checkpoint not found — run `qfn pull` first")
			}
			hf := config.ExpandHome(app.Cfg.Paths.HFCache)
			srcIn := fmt.Sprintf("/hf/hub/models--%s/snapshots/%s", strings.ReplaceAll(e.Model, "/", "--"), st.Snapshot)
			dstIn := srcIn + "-fp8hybrid"
			// Tool comes from the embedded assets; mount it read-only.
			tmp, err := os.MkdirTemp("", "qfn-hybrid-*")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			if _, err := engineassets.WriteToDisk(tmp); err != nil {
				return err
			}
			script := fmt.Sprintf(`
set -euo pipefail
rm -rf '%[2]s'
cp -a '%[1]s' '%[2]s'
cp --remove-destination "$(readlink -f '%[2]s/model.safetensors.index.json')" '%[2]s/model.safetensors.index.json'
python3 /tools/fp8_convert.py '%[2]s' | tee '%[2]s/fp8_convert.log'
n=$(python3 -c "import json;print(sum(1 for k in json.load(open('%[2]s/model.safetensors.index.json'))['weight_map'] if k.endswith('weight_scale_inv')))")
[ "$n" -gt 0 ] || { echo '!! conversion produced no fp8 tensors'; exit 1; }
chmod 644 '%[2]s'/*.safetensors '%[2]s'/model.safetensors.index.json
touch '%[2]s/.prepared'
echo ">> done: $n fp8 side-layer tensors"
`, srcIn, dstIn)
			dimf("preparing %s (original snapshot is never touched)", dstIn)
			return streamDocker(c, "run", "--rm", "--name", "qfn-fp8convert",
				"-v", hf+":/hf", "-v", filepath.Join(tmp, "engine", "tools")+":/tools:ro",
				"--entrypoint", "bash", e.Image, "-c", script)
		},
	})
}

// ---- helpers ----

// streamDocker runs docker with inherited stdio (long jobs need progress).
func streamDocker(c *cobra.Command, args ...string) error {
	cmd := exec.CommandContext(c.Context(), "docker", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// consoleUp probes /api/health on the configured console.
func (a *App) consoleUp(ctx context.Context) bool {
	cctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, a.ServeBaseURL()+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// benchBase/chat fallback: console first, engine second.
func (a *App) benchBase() string {
	if a.consoleUp(context.Background()) {
		return a.ServeBaseURL()
	}
	return a.EngineBaseURL()
}

func (a *App) benchKey() string {
	if fk := a.FrontKey(); fk != "" && a.consoleUp(context.Background()) {
		return fk
	}
	return a.engineKeyOnly()
}

func (a *App) engineKeyOnly() string {
	if !a.Cfg.Engine.Lockdown {
		return ""
	}
	k, _ := a.Store.EngineKey()
	return k
}

// appendBenchHistory keeps drift history (one json array line per run).
func (a *App) appendBenchHistory(results []bench.Result) {
	if len(results) == 0 {
		return
	}
	_ = os.MkdirAll(a.StateDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(a.StateDir(), "bench-history.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	rec := struct {
		TS      time.Time       `json:"ts"`
		Launch  lastUp          `json:"launch"`
		Results []bench.Result  `json:"results"`
	}{time.Now(), a.LastUp(), results}
	b, _ := json.Marshal(rec)
	f.Write(append(b, '\n'))
}

func swapPctOf(h collector.HostState) float64 {
	if h.SwapTotalKiB == 0 {
		return 0
	}
	return 100 * float64(h.SwapUsedKiB) / float64(h.SwapTotalKiB)
}

func psiAvg(h collector.HostState, res string) string {
	if p, ok := h.Psi[res]; ok {
		return fmt.Sprintf("%.1f", p.SomeAvg10)
	}
	return "—"
}

var _ = engine.ServedModelName
