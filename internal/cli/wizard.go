package cli

// First-run wizard: one question per topic, every field with its default and
// trade-off visible, live cross-field validation, nothing written until the
// final Save. `qfn config` re-enters the same flow in edit mode; `qfn init`
// is the explicit entry point.

import (
	"errors"
	"fmt"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
)

// Wizard runs the flow; edit=true starts from cfg instead of defaults.
// The console password (when captured) is returned out-of-band — it never
// lives inside config.Config, only in credentials.age.
func Wizard(cfg config.Config, edit bool) (config.Config, string, error) {
	out := cfg
	pwCaptured := ""
	if !edit {
		out = config.Defaults()
		fmt.Println()
		fmt.Println(styled("  QFN-PGX setup — Qwen3.8-Flash-Next on one DGX Spark", ansiAcc))
		dimf("  every answer has a default in parentheses; Enter keeps it.")
		fmt.Println()
	} else {
		dimf("  editing %s (Enter keeps each value)", config.Path())
	}

	// [1] Storage
	hf, err := Ask("huggingface cache dir", out.Paths.HFCache, "where the ~122 GiB checkpoint lives")
	if err != nil {
		return out, "", err
	}
	out.Paths.HFCache = hf
	stateDir, err := Ask("state dir", out.Paths.StateDir, "bench history, lockouts, service inventory")
	if err != nil {
		return out, "", err
	}
	out.Paths.StateDir = stateDir

	// [2] Checkpoint mode
	modeOpts := []string{
		"nvfp4 — the checkpoint exactly as published",
		"hybrid — fp8 side layers: +20% decode, +15–20% KV, same quality (needs `qfn prepare-hybrid`, ~10 min one-time)",
	}
	idx := 0
	if out.Engine.Mode == "hybrid" {
		idx = 1
	}
	sel, err := AskChoice("checkpoint mode", modeOpts, idx, "")
	if err != nil {
		return out, "", err
	}
	out.Engine.Mode = []string{"nvfp4", "hybrid"}[sel]

	// [3] Context & speed
	ctxPresets := []string{
		"Balanced — 262k native context, defaults (recommended)",
		"Max tokens/s — hybrid + prewarm + MTP 2 (262k)",
		"Long context — YaRN 500k (slower prefill, gpu_mem 0.80)",
		"Custom — ask me field by field",
	}
	sel, err = AskChoice("speed & context preset", ctxPresets, 0, "")
	if err != nil {
		return out, "", err
	}
	switch sel {
	case 1:
		out.Engine.Mode = "hybrid"
		out.Engine.Prewarm = true
		out.Engine.MTP = 2
		out.Engine.Ctx = 262144
		out.Engine.Yarn = false
	case 2:
		out.Engine.Yarn = true
		out.Engine.Ctx = 500000
		out.Engine.GpuMem = 0.80
	case 3:
		out.Engine.Ctx, err = AskInt("context length", out.Engine.Ctx, 4096, 1048576, "native max 262144")
		if err != nil {
			return out, "", err
		}
		out.Engine.Yarn, err = AskBool("YaRN rope scaling", out.Engine.Yarn || out.Engine.Ctx > 262144, "required above 262144; validated to 500000")
		if err != nil {
			return out, "", err
		}
		out.Engine.GpuMem, err = AskFloat("gpu-memory-utilization", out.Engine.GpuMem, 0.5, 0.875, "fraction of the 128 GB unified pool (0.875 has OOM'd upstream)")
		if err != nil {
			return out, "", err
		}
		out.Engine.MTP, err = AskInt("MTP speculative tokens", out.Engine.MTP, 0, 4, "0 disables; 2 is this checkpoint's measured peak")
		if err != nil {
			return out, "", err
		}
		out.Engine.Seqs, err = AskInt("max concurrent sequences", out.Engine.Seqs, 1, 64, "keep ≥4 when measuring throughput or they queue silently")
		if err != nil {
			return out, "", err
		}
	}
	// ctx/yarn coupling is enforced by Validate at Save; surface it now too.
	if out.Engine.Ctx > 262144 && !out.Engine.Yarn {
		out.Engine.Yarn = true
		dimf("  (ctx %d exceeds the native window — YaRN enabled automatically)", out.Engine.Ctx)
	}

	// [4] Determinism + prefix cache (advanced, one screen)
	out.Engine.ExactTopK, err = AskBool("deterministic exact top-k", out.Engine.ExactTopK, "identical output at temperature 0; costs ~10–40% on long prefills")
	if err != nil {
		return out, "", err
	}
	out.Engine.PrefixCache, err = AskBool("prefix caching", out.Engine.PrefixCache, "repeated prefixes skip prefill (~14s → ~1.4s on a 20k prefix)")
	if err != nil {
		return out, "", err
	}
	out.Engine.Prewarm, err = AskBool("prewarm the PLE page cache at boot", out.Engine.Prewarm, "streams ~48 GiB once; slower boot, steadier first requests")
	if err != nil {
		return out, "", err
	}

	// [5] Engine exposure
	lockOpts := []string{
		"lockdown (recommended) — loopback bind + generated API key only the qfn proxy holds",
		"open — engine reachable directly on the LAN (legacy clients hardwired to :18300)",
	}
	idx = 0
	if !out.Engine.Lockdown || out.Engine.Bind == "0.0.0.0" {
		idx = 1
	}
	sel, err = AskChoice("engine exposure", lockOpts, idx,
		"with lockdown, agents must point at the console port instead of the raw engine")
	if err != nil {
		return out, "", err
	}
	if sel == 0 {
		out.Engine.Lockdown = true
		out.Engine.Bind = "127.0.0.1"
	} else {
		out.Engine.Lockdown = false
		out.Engine.Bind = "0.0.0.0"
	}

	// [6] Console + password
	out.Serve.Port, err = AskInt("console + proxy port", out.Serve.Port, 1024, 65535, "this is the single entry point for UI and agents")
	if err != nil {
		return out, "", err
	}
	out.Serve.Bind, err = Ask("console bind address", out.Serve.Bind, `anything beyond loopback forces auth on (it's always on)`)
	if err != nil {
		return out, "", err
	}
	if !edit || !config.IsLoopbackBind(out.Serve.Bind) {
		dimf("  the console stays up across reboots (`qfn service install`) and can start/stop the engine from the browser —")
		dimf("  so it gets a password. Over the LAN it rides plain HTTP; the safe access path is:")
		dimf("    ssh -L %d:localhost:%d <spark>", out.Serve.Port, out.Serve.Port)
		pw, err := AskPassword("console password (≥12 chars)", 12)
		if err != nil {
			return out, "", err
		}
		pwCaptured = pw
	}
	out.Serve.AuthEnabled = true

	// Final validation loop
	for {
		if err := out.Validate(); err != nil {
			badf("%v — fix the offending answer (rerun or adjust)", err)
			out.Engine.GpuMem, err = AskFloat("gpu-memory-utilization", out.Engine.GpuMem, 0.5, 0.875, "must be ≤ 0.875")
			if err != nil {
				return out, "", err
			}
			continue
		}
		break
	}

	// Save
	if !edit {
		saveDefault := "yes — write it and I'm ready to `qfn up`"
		choiceOpts := []string{saveDefault, "no — print the config to stdout instead"}
		sel, err = AskChoice("save", choiceOpts, 0, "")
		if err != nil {
			return out, "", err
		}
		if sel == 1 {
			fmt.Print(config.Render(out))
			return out, pwCaptured, errors.New("not saved")
		}
	}
	return out, pwCaptured, nil
}

// Persist writes config + the wizard-captured password to credentials.age.
func Persist(out config.Config, pw string) error {
	if err := out.Validate(); err != nil {
		return err
	}
	if err := config.Save(out); err != nil {
		return err
	}
	if pw != "" {
		app, err := Load()
		if err != nil {
			return err
		}
		return app.Store.EnsureFirstRun(pw)
	}
	return nil
}

// EditConfig reopens the wizard over the on-disk config (config edit).
func EditConfig() error {
	cfg, ok, err := config.Load()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no config yet — run `qfn init`")
	}
	out, pw, err := Wizard(cfg, true)
	if err != nil {
		return err
	}
	if err := config.Save(out); err != nil {
		return err
	}
	if pw != "" {
		app, err := Load()
		if err != nil {
			return err
		}
		return app.Store.SetPassword(pw)
	}
	return nil
}

// OfferProfileAfterSetup offers to snapshot the resolved config as a profile.
func OfferProfileAfterSetup(app *App, cfg config.Config) error {
	yes, err := AskBool("save these launch values as a named profile?", true, "profiles layer over config.toml; `qfn up <name>` picks one")
	if err != nil || !yes {
		return nil
	}
	name, err := Ask("profile name", "daily", "lowercase, e.g. daily / long-ctx / fast")
	if err != nil {
		return err
	}
	if err := profiles.ValidateName(name); err != nil {
		return err
	}
	p := profiles.FromEngine(name, "created by qfn init", cfg.Engine)
	if err := profiles.Save(p); err != nil {
		return err
	}
	cfg.Meta.DefaultProfile = name
	if err := config.Save(cfg); err != nil {
		return err
	}
	okf("profile %q saved and set as default (profiles dir: %s)", name, profiles.Dir())
	return nil
}
