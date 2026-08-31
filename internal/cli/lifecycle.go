package cli

// Engine lifecycle: up / down / restart / status / logs. `up` runs the same
// preflight as the web console, so neither path can skip the memory guard.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
)

type upFlags struct {
	profile     string
	ask         bool
	yesIKnow    bool
	mode        string
	ctx         int
	yarn        bool
	mtp         int
	seqs        int
	gpuMem      float64
	prewarm     bool
	prefixCache bool
	exactTopK   bool
	port        int
	bind        string
	cpuset      string
	extra       string
	flagsSet    map[string]bool
}

func addLifecycle(root *cobra.Command, app *App) {
	uf := &upFlags{flagsSet: map[string]bool{}}

	up := &cobra.Command{
		Use:   "up [profile]",
		Short: "Launch the engine (preflight-guarded)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := firstRunGuard(app); err != nil {
				return err
			}
			if len(args) == 1 {
				uf.profile = args[0]
			}
			return runUp(c.Context(), app, uf)
		},
	}
	up.Flags().BoolVar(&uf.ask, "ask", false, "walk through launch options first")
	up.Flags().StringVar(&uf.profile, "profile", "", "named profile to layer over config.toml")
	up.Flags().BoolVar(&uf.yesIKnow, "yes-i-know", false, "override the memory guard (you'll owe the Spark an apology)")
	up.Flags().StringVar(&uf.mode, "mode", "", "nvfp4|hybrid")
	up.Flags().IntVar(&uf.ctx, "ctx", 0, "max context length")
	up.Flags().BoolVar(&uf.yarn, "yarn", false, "YaRN rope scaling (required >262144)")
	up.Flags().IntVar(&uf.mtp, "mtp", -1, "MTP speculative tokens (0..4)")
	up.Flags().IntVar(&uf.seqs, "seqs", 0, "max concurrent sequences")
	up.Flags().Float64Var(&uf.gpuMem, "gpu-mem", 0, "gpu-memory-utilization (≤0.875)")
	up.Flags().BoolVar(&uf.prewarm, "prewarm", false, "prewarm the PLE page cache")
	up.Flags().BoolVar(&uf.prefixCache, "prefix-cache", true, "enable prefix caching")
	up.Flags().BoolVar(&uf.exactTopK, "exact-topk", true, "deterministic exact top-k")
	up.Flags().IntVar(&uf.port, "port", 0, "engine port")
	up.Flags().StringVar(&uf.bind, "bind", "", "docker bind address (127.0.0.1|0.0.0.0)")
	up.Flags().StringVar(&uf.cpuset, "cpuset", "", "pin container to CPU list (e.g. 0-3)")
	up.Flags().StringVar(&uf.extra, "extra", "", "raw extra args appended to the vLLM command")
	up.PreRun = func(c *cobra.Command, args []string) {
		c.Flags().Visit(func(f *pflag.Flag) { uf.flagsSet[f.Name] = true })
	}
	root.AddCommand(up)

	down := &cobra.Command{
		Use:   "down", Short: "Stop and remove the engine container",
		RunE: func(c *cobra.Command, _ []string) error {
			if err := firstRunGuard(app); err != nil {
				return err
			}
			return app.downNow(c.Context())
		},
	}
	root.AddCommand(down)

	restart := &cobra.Command{
		Use:   "restart", Short: "down + up",
		RunE: func(c *cobra.Command, _ []string) error {
			if err := firstRunGuard(app); err != nil {
				return err
			}
			if err := app.downNow(c.Context()); err != nil {
				return err
			}
			return runUp(c.Context(), app, &upFlags{profile: app.Cfg.Meta.DefaultProfile, flagsSet: map[string]bool{}})
		},
	}
	root.AddCommand(restart)

	status := &cobra.Command{
		Use:   "status", Short: "Engine + console + pool summary",
		RunE: func(c *cobra.Command, _ []string) error {
			watch, _ := c.Flags().GetBool("watch")
			return runStatus(c.Context(), app, watch)
		},
	}
	status.Flags().BoolP("watch", "w", false, "live refresh (2s)")
	root.AddCommand(status)

	logs := &cobra.Command{
		Use:   "logs", Short: "Engine container logs",
		RunE: func(c *cobra.Command, _ []string) error {
			follow, _ := c.Flags().GetBool("follow")
			tail, _ := c.Flags().GetInt("tail")
			return runLogs(c.Context(), app, follow, tail)
		},
	}
	logs.Flags().BoolP("follow", "f", false, "stream")
	logs.Flags().Int("tail", 200, "lines from the end")
	root.AddCommand(logs)
}

// downNow stops the container, reporting "was down" quietly.
func (a *App) downNow(ctx context.Context) error {
	op, err := a.Mgr.TryBegin("down", "cli")
	if err != nil {
		return err
	}
	err = a.Mgr.Down(ctx, op, a.Cfg.Engine.Name)
	if err != nil {
		return err
	}
	dimf("engine stopped")
	return nil
}

// runUp: flags→overlay→resolve→preflight→docker run; prints a boot hint.
func runUp(ctx context.Context, app *App, uf *upFlags) error {
	if uf.ask && isTTY() {
		if err := askLaunch(uf); err != nil {
			return err
		}
	}
	// Overlay named profile, then explicit flags (flag > profile > config).
	eng := app.Cfg.Engine
	name := uf.profile
	if name == "" {
		name = app.Cfg.Meta.DefaultProfile
	}
	if name != "" {
		p, err := profiles.Load(name)
		if err != nil {
			return err
		}
		p.Apply(&eng)
	}
	// Only flags the user actually set override the profile.
	setStr(&eng.Mode, uf.mode, uf.flagsSet["mode"])
	setInt(&eng.Ctx, uf.ctx, uf.flagsSet["ctx"])
	setInt(&eng.MTP, uf.mtp, uf.flagsSet["mtp"])
	setInt(&eng.Seqs, uf.seqs, uf.flagsSet["seqs"])
	setInt(&eng.Port, uf.port, uf.flagsSet["port"])
	setF64(&eng.GpuMem, uf.gpuMem, uf.flagsSet["gpu-mem"])
	setBool(&eng.Yarn, uf.yarn, uf.flagsSet["yarn"])
	setBool(&eng.Prewarm, uf.prewarm, uf.flagsSet["prewarm"])
	setBool(&eng.PrefixCache, uf.prefixCache, uf.flagsSet["prefix-cache"])
	setBool(&eng.ExactTopK, uf.exactTopK, uf.flagsSet["exact-topk"])
	setStr(&eng.CPUSet, uf.cpuset, uf.flagsSet["cpuset"])
	setStr(&eng.Extra, uf.extra, uf.flagsSet["extra"])
	if uf.flagsSet["bind"] {
		eng.Bind = uf.bind
		eng.Lockdown = config.IsLoopbackBind(uf.bind) && eng.Lockdown
	}
	probe := app.Cfg
	probe.Engine = eng
	if err := probe.Validate(); err != nil {
		return err
	}

	key := ""
	if eng.Lockdown {
		k, err := app.Store.EngineKey()
		if err != nil || k == "" {
			return errors.New("lockdown is on but no engine key is stored — run `qfn lockdown on`")
		}
		key = k
	}

	// Preflight: the memory guard + snapshot/hybrid probe, same as the web.
	if err := doctor.Preflight(ctx, app.Cfg, eng, uf.yesIKnow); err != nil {
		return err
	}
	loc := app.Locator()
	st := engine.StatusOf(loc, eng)
	if !st.RepoExists {
		return errors.New("no checkpoint snapshot under the HF cache — run `qfn pull`")
	}
	if eng.Mode == "hybrid" && !st.HybridPrepared {
		return errors.New("mode=hybrid but the fp8 variant isn't prepared — run `qfn prepare-hybrid` (~10 min)")
	}
	if exists, _, err := engine.ImageExists(ctx, app.Docker, eng.Image); err == nil && !exists {
		return fmt.Errorf("image %s not built — run `qfn build` first", eng.Image)
	}
	args, err := engine.DockerArgs(eng, loc, engine.LaunchOpts{
		EngineAPIKey: key, HFCacheHost: config.ExpandHome(app.Cfg.Paths.HFCache),
	})
	if err != nil {
		return err
	}

	op, err := app.Mgr.TryBegin("up", "cli")
	if err != nil {
		return err
	}
	dimf("launching %s (profile %q, mode %s, ctx %d, mtp %d)…", eng.Name, displayName(name), eng.Mode, eng.Ctx, eng.MTP)
	if err := app.Mgr.Up(ctx, op, args, eng.Name); err != nil {
		badf("launch failed: %v", err)
		return err
	}
	app.SaveLastUp(name, eng)
	okf("container created — weights stream from NVMe; first responses in ~10 min")
	dimf("watch boot:  qfn status -w    ·  logs: qfn logs -f")
	dimf("console:    http://%s:%d", app.Cfg.Serve.Bind, app.Cfg.Serve.Port)
	return nil
}

func displayName(n string) string {
	if n == "" {
		return "—"
	}
	return n
}

// runStatus renders a compact table; watch=true refreshes every 2s.
func runStatus(ctx context.Context, app *App, watch bool) error {
	render := func() error {
		st, err := engine.Inspect(ctx, app.Docker, app.Cfg.Engine.Name)
		fmt.Printf("engine %s\n", app.Cfg.Engine.Name)
		if err != nil {
			dimf("  container: absent (`qfn up` to launch)")
		} else {
			fmt.Printf("  status:    %s", st.Status)
			if st.Running {
				// Boot phase from the log tail.
				phase, detail := bootPhase(ctx, app)
				fmt.Printf("  ·  boot: %s %s", phase, detail)
			} else if st.ExitCode != 0 {
				fmt.Printf(" (exit %d)", st.ExitCode)
			}
			fmt.Println()
			if !st.StartedAt.IsZero() {
				fmt.Printf("  since:     %s\n", st.StartedAt.Local().Format("2006-01-02 15:04:05"))
			}
		}
		last := app.LastUp()
		fmt.Printf("  endpoint:  http://127.0.0.1:%d/v1 (%s lockdown key %v)\n",
			last.Port, engine.ServedModelName, app.Cfg.Engine.Lockdown)
		fmt.Printf("  console:   http://%s:%d\n", app.Cfg.Serve.Bind, app.Cfg.Serve.Port)
		mm, err := readMem(app)
		if err == nil {
			fmt.Printf("  pool:      %d/%d GiB used · swap %.0f%% · avail %d GiB\n",
				(mm.MemTotal-mm.MemAvail)/1048576, mm.MemTotal/1048576, mm.SwapUsedPct(), mm.MemAvail/1048576)
		}
		if op, ok := app.Mgr.LastOp(); ok {
			line := fmt.Sprintf("  last op:   %s by %s", op.Kind, op.Actor)
			if op.Done {
				line += fmt.Sprintf(" ✓ (%s)", op.Started.Local().Format("15:04:05"))
			} else {
				line += " (running…)"
			}
			if op.Err != "" {
				line += " — " + op.Err
			}
			fmt.Println(line)
		}
		return nil
	}
	if !watch {
		return render()
	}
	fmt.Print("\x1b[2J\x1b[H")
	for {
		fmt.Print("\x1b[H")
		if err := render(); err != nil {
			return err
		}
		dimf("(refreshing every 2s — Ctrl+C quits)")
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func bootPhase(ctx context.Context, app *App) (string, string) {
	c, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	pr, pw := io.Pipe()
	go func() { _ = app.Mgr.Logs(c, app.Cfg.Engine.Name, pw); _ = pw.Close() }()
	bt := &engine.BootTracker{}
	phase, detail := "created", ""
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 512*1024)
	for sc.Scan() {
		p, d := bt.Feed(sc.Text())
		phase, detail = p.String(), d
		if p == engine.PhaseReady {
			break
		}
	}
	return phase, detail
}

func runLogs(ctx context.Context, app *App, follow bool, tail int) error {
	if !follow {
		out, err := app.Docker.Run(ctx, "logs", "--tail", strconv.Itoa(tail), app.Cfg.Engine.Name)
		fmt.Print(out)
		return err
	}
	return app.Mgr.Logs(ctx, app.Cfg.Engine.Name, os.Stdout)
}

// askLaunch is `up --ask`: quick wizard over launch fields.
func askLaunch(uf *upFlags) error {
	var err error
	uf.profile, err = Ask("profile", uf.profile, "Enter for config defaults")
	if err != nil {
		return err
	}
	mode, err := AskChoice("mode", []string{"nvfp4", "hybrid"}, 0, "")
	if err != nil {
		return err
	}
	if mode == 1 {
		uf.mode = "hybrid"
		uf.flagsSet["mode"] = true
	}
	n, err := Ask("override ctx/mtp/gpu-mem individually?", "n", "n keeps profile values")
	if err == nil && n == "y" {
		uf.ctx, err = AskInt("context", 262144, 4096, 1048576, "")
		uf.flagsSet["ctx"] = true
		if err != nil {
			return err
		}
		uf.mtp, err = AskInt("mtp", 2, 0, 4, "")
		uf.flagsSet["mtp"] = true
		if err != nil {
			return err
		}
	}
	return nil
}

// ---- small typed setters honoring flag-set tracking ----

func setStr(dst *string, v string, set bool)    { if set && v != "" { *dst = v } }
func setInt(dst *int, v int, set bool)           { if set && v != 0 { *dst = v } }
func setF64(dst *float64, v float64, set bool)   { if set && v != 0 { *dst = v } }
func setBool(dst *bool, v bool, set bool)        { if set { *dst = v } }


// readMem is the tiny /proc/meminfo view status prints.
type memSnap struct {
	MemTotal, MemAvail, SwapTotal, SwapFree uint64
}

func (m memSnap) SwapUsedPct() float64 {
	if m.SwapTotal == 0 {
		return 0
	}
	return 100 * float64(m.SwapTotal-m.SwapFree) / float64(m.SwapTotal)
}

func readMem(*App) (memSnap, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return memSnap{}, err
	}
	var m memSnap
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "MemTotal":
			m.MemTotal = v
		case "MemAvailable":
			m.MemAvail = v
		case "SwapTotal":
			m.SwapTotal = v
		case "SwapFree":
			m.SwapFree = v
		}
	}
	return m, nil
}
