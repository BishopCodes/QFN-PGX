package cli

// `qfn serve`: the always-on console — hot-reloads config.toml each call,
// wipes sessions on restart, and mounts the proxy when serve.proxy is on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/BishopCodes/qfn-pgx/internal/auth"
	"github.com/BishopCodes/qfn-pgx/internal/collector"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
	"github.com/BishopCodes/qfn-pgx/internal/proxy"
	"github.com/BishopCodes/qfn-pgx/internal/server"
	"github.com/BishopCodes/qfn-pgx/internal/service"
)

func addServe(root *cobra.Command, app *App) {
	var (
		portFlag int
		bindFlag string
		noProxy  bool
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web console + proxy (foreground; systemd keeps it alive via `qfn service install`)",
		RunE: func(c *cobra.Command, _ []string) error {
			if err := firstRunGuard(app); err != nil {
				return err
			}
			return runServe(c.Context(), app, portFlag, bindFlag, noProxy)
		},
	}
	cmd.Flags().IntVar(&portFlag, "port", 0, "override serve.port")
	cmd.Flags().StringVar(&bindFlag, "bind", "", "override serve.bind")
	cmd.Flags().BoolVar(&noProxy, "no-proxy", false, "console only; don't mount /v1")

	cmd.AddCommand(serveStopCmd(app), serveRestartCmd(app), serveStatusCmd(app))
	root.AddCommand(cmd)
}

// ---- console stop / restart / status ----
//
// Same story as the web "Restart console" button: if the systemd unit is
// active, systemctl is authoritative (and survives logout); otherwise we use
// the pidfile that every `qfn serve` writes next to its state.

func pidPath(app *App) string { return app.StateDir() + "/serve.pid" }

func writePidfile(app *App) {
	_ = os.MkdirAll(filepath.Dir(pidPath(app)), 0o700)
	_ = os.WriteFile(pidPath(app), []byte(strconv.Itoa(os.Getpid())), 0o644)
}

func readPid(app *App) (int, bool) {
	b, err := os.ReadFile(pidPath(app))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return 0, false
	}
	return pid, true
}

func systemdActive() bool {
	return exec.Command("systemctl", "is-active", "--quiet", service.ServeUnit+".service").Run() == nil
}

func systemctl(verb string) error {
	if err := exec.Command("systemctl", verb, service.ServeUnit+".service").Run(); err != nil {
		return fmt.Errorf("systemctl %s failed (try `sudo systemctl %s %s.service`): %w", verb, verb, service.ServeUnit, err)
	}
	return nil
}

func serveStopCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running web console (systemd-aware, pidfile fallback)",
		RunE: func(c *cobra.Command, _ []string) error {
			if systemdActive() {
				if err := systemctl("stop"); err != nil {
					return err
				}
				okf("console stopped (systemd)")
				return nil
			}
			pid, ok := readPid(app)
			if !ok {
				dimf("no console running (systemd unit inactive, no live pidfile)\n")
				return nil
			}
			if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
				return err
			}
			okf("SIGTERM → console pid %d", pid)
			return nil
		},
	}
}

func serveRestartCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the console — picks up a newly installed qfn binary",
		RunE: func(c *cobra.Command, _ []string) error {
			if systemdActive() {
				if err := systemctl("restart"); err != nil {
					return err
				}
				okf("console restarted by systemd — now running %s", Version())
				return nil
			}
			if pid, ok := readPid(app); ok {
				_ = syscall.Kill(pid, syscall.SIGTERM)
				time.Sleep(400 * time.Millisecond)
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			child := exec.Command(exe, "serve")
			child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			if err := child.Start(); err != nil {
				return fmt.Errorf("respawn console: %w", err)
			}
			pid := child.Process.Pid
			_ = child.Process.Release()
			okf("console respawned detached (pid %d) — start it with output redirected if you want logs", pid)
			return nil
		},
	}
}

func serveStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Is the console running, and how?",
		RunE: func(c *cobra.Command, _ []string) error {
			switch {
			case systemdActive():
				fmt.Printf("  console: %s (systemd unit, %s)\n", okStr(), Version())
			default:
				if pid, ok := readPid(app); ok {
					fmt.Printf("  console: running standalone, pid %d\n", pid)
				} else {
					dimf("  console: not running — `qfn serve` (or `sudo qfn service install` for always-on)\n")
				}
			}
			return nil
		},
	}
}

func okStr() string { return "up" }

func runServe(ctx context.Context, app *App, port int, bind string, noProxy bool) error {
	// Hot-reload source: every dependency reads config fresh per call.
	live := func() config.Config {
		cfg, _, err := config.Load()
		if err != nil {
			return app.Cfg
		}
		if port != 0 {
			cfg.Serve.Port = port
		}
		if bind != "" {
			cfg.Serve.Bind = bind
		}
		if noProxy {
			cfg.Serve.Proxy = false
		}
		return cfg
	}
	cfg := live()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if !app.Store.HasPassword() {
		return errors.New("no console password yet — run `qfn init`")
	}

	// Sessions die with the process (documented security posture).
	sessions := auth.NewSessions(12 * time.Hour)
	limiter := auth.NewLimiter(app.StateDir() + "/login-attempts.json")

	engKey := func() string {
		if !app.Cfg.Engine.Lockdown {
			return ""
		}
		k, _ := app.Store.EngineKey()
		return k
	}
	base := func() string { return app.EngineBaseURL() }

	col := collector.New(collector.Config{
		EngineBase:    base,
		EngineKey:     engKey,
		ContainerName: cfg.Engine.Name,
		HFCacheHost:   config.ExpandHome(cfg.Paths.HFCache),
		Interval:      2 * time.Second,
	}, collector.IO{})
	go col.Run(ctx)

	reg := proxy.NewRegistry(200)
	var proxyHandler http.Handler
	if cfg.Serve.Proxy {
		p := proxy.New(reg)
		p.Target = base
		p.Key = engKey
		p.MaxPromptTokens = func() int { return live().Serve.MaxPromptTokens }
		p.Sampling = func() map[string]any {
			c := live()
			if !c.Serve.SamplingDefaults {
				return nil
			}
			return engine.GenerationDefaults(config.ExpandHome(c.Paths.HFCache), c.Engine.Model)
		}
		proxyHandler = p
	}

	srv := server.New(server.Deps{
		Cfg:       live,
		Store:     app.Store,
		Sessions:  sessions,
		Limiter:   limiter,
		Collector: col,
		Manager:   app.Mgr,
		Registry:  reg,
		Proxy:     proxyHandler,
		Profiles: func() []string {
			names, _ := profiles.List()
			return names
		},
		Resolve:   app.Resolve,
		Locator:   app.Locator,
		Preflight: func(ctx context.Context, e config.Engine) error {
			if err := doctor.Preflight(ctx, live(), e, false); err != nil {
				return err
			}
			if exists, _, err := engine.ImageExists(ctx, app.Docker, e.Image); err == nil && !exists {
				return fmt.Errorf("image %s not built — run `qfn build` on the Spark first", e.Image)
			}
			return nil
		},
		Meta:      func() map[string]any { return app.meta() },
		Version:   Version(),
	})

	httpSrv := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", cfg.Serve.Bind, cfg.Serve.Port),
		Handler: srv.Handler(),
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()
	writePidfile(app)
	defer os.Remove(pidPath(app))
	fmt.Printf("qfn console on http://%s (auth: password, sessions wiped on restart)\n", httpSrv.Addr)
	if proxyHandler != nil {
		fmt.Printf("        proxy /v1 → %s (lockdown key %v)\n", base(), app.Cfg.Engine.Lockdown)
	}
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// meta assembles the console's model panel data.
func (a *App) meta() map[string]any {
	lu := a.LastUp()
	return map[string]any{
		"model_name":   engine.ServedModelName,
		"mode":         lu.Mode,
		"ctx":          lu.Ctx,
		"mtp":          lu.MTP,
		"prefix_cache": lu.Prefix,
		"exact_topk":   lu.Topk,
		"serve_port":   a.Cfg.Serve.Port,
	}
}
