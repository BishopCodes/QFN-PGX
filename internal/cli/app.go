// Package cli wires the cobra commands. Shared plumbing (config, docker,
// manager, resolution chain) lives on App so every command behaves the same.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/BishopCodes/qfn-pgx/internal/auth"
	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
)

var version = "dev"

// Version exposes the build stamp to root.
func Version() string { return version }

// App is the shared dependency hub.
type App struct {
	Cfg    config.Config
	HasCfg bool

	Docker *engine.CLIDocker
	Mgr    *engine.Manager
	Store  *auth.Store
}

// Load reads config (defaults if absent — commands decide whether a missing
// config is an error) and wires state.
func Load() (*App, error) {
	cfg, ok, err := config.Load()
	if err != nil {
		return nil, err
	}
	a := &App{
		Cfg:    cfg,
		HasCfg: ok,
		Docker: &engine.CLIDocker{},
	}
	a.Mgr = engine.NewManager(a.Docker)
	a.Store = auth.NewStore(config.Dir())
	return a, nil
}

// StateDir expands paths.state_dir.
func (a *App) StateDir() string { return config.ExpandHome(a.Cfg.Paths.StateDir) }

// Locator for the current HF cache.
func (a *App) Locator() engine.SnapshotLocator {
	return engine.NewSnapshotLocator(config.ExpandHome(a.Cfg.Paths.HFCache))
}

// Resolve applies the precedence chain built-ins < config < named profile.
// (CLI flag overlays are applied by the up command before calling Resolve("")
// with the resulting override profile.)
func (a *App) Resolve(profile string) (config.Engine, string, error) {
	eng := a.Cfg.Engine
	name := profile
	if name == "" {
		name = a.Cfg.Meta.DefaultProfile
	}
	if name != "" {
		p, err := profiles.Load(name)
		if err != nil {
			return eng, "", fmt.Errorf("profile %q: %w", name, err)
		}
		p.Apply(&eng)
	}
	key := ""
	if eng.Lockdown {
		k, err := a.Store.EngineKey()
		if err != nil || k == "" {
			return eng, "", errors.New("engine lockdown is on but the stored engine key is missing — run `qfn lockdown on` or `qfn init`")
		}
		key = k
	}
	return eng, key, nil
}

// lastUpFile records the profile/port of the last successful `up` so the
// console scrapes the right engine port even when a non-default profile ran.
func (a *App) lastUpFile() string { return filepath.Join(a.StateDir(), "last-up.json") }

type lastUp struct {
	Profile string `json:"profile"`
	Port    int    `json:"port"`
	Mode    string `json:"mode"`
	Ctx     int    `json:"ctx"`
	MTP     int    `json:"mtp"`
	Prefix  bool   `json:"prefix_cache"`
	Topk    bool   `json:"exact_topk"`
}

// SaveLastUp persists what actually launched.
func (a *App) SaveLastUp(profile string, e config.Engine) {
	_ = os.MkdirAll(a.StateDir(), 0o755)
	b, _ := json.Marshal(lastUp{Profile: profile, Port: e.Port, Mode: e.Mode, Ctx: e.Ctx, MTP: e.MTP, Prefix: e.PrefixCache, Topk: e.ExactTopK})
	_ = os.WriteFile(a.lastUpFile(), b, 0o644)
}

// LastUp returns the recorded launch (zero value if never).
func (a *App) LastUp() lastUp {
	var lu lastUp
	b, err := os.ReadFile(a.lastUpFile())
	if err == nil {
		_ = json.Unmarshal(b, &lu)
	}
	if lu.Port == 0 {
		lu.Port = a.Cfg.Engine.Port
	}
	return lu
}

// EngineBaseURL for scraping/chat/bench: prefer last-up, fallback config.
func (a *App) EngineBaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.LastUp().Port)
}

// ServeBaseURL is the console front door URL.
func (a *App) ServeBaseURL() string {
	return "http://127.0.0.1:" + strconv.Itoa(a.Cfg.Serve.Port)
}

// FrontKey returns the machine bearer if one exists (chat/bench auth).
func (a *App) FrontKey() string {
	k, _ := a.Store.FrontKey()
	return k
}

// ctx helper for commands.
func cmdCtx(c *cobra.Command) context.Context { return c.Context() }

// requireConfig fails commands that can't run without a completed setup.
func (a *App) requireConfig() error {
	if !a.Cfg.Meta.FirstRunDone {
		if os.Geteuid() == 0 && os.Getenv("SUDO_USER") != "" {
			return errors.New("setup incomplete — run `qfn init` first (config not found for root; SUDO_USER=" + os.Getenv("SUDO_USER") + " home=" + config.Dir() + ")")
		}
		return errors.New("setup incomplete — run `qfn init` first")
	}
	return nil
}
