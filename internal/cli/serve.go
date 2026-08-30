package cli

// `qfn serve`: the always-on console — hot-reloads config.toml each call,
// wipes sessions on restart, and mounts the proxy when serve.proxy is on.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	root.AddCommand(cmd)
}

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
		Preflight: func(ctx context.Context, e config.Engine) error { return doctor.Preflight(ctx, live(), e, false) },
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
