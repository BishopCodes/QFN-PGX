package cli

// Setup surface: init wizard, config get/set/edit, profiles, passwd,
// lockdown keys, systemd service management.

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/doctor"
	"github.com/BishopCodes/qfn-pgx/internal/profiles"
	"github.com/BishopCodes/qfn-pgx/internal/service"
)

func addSetup(root *cobra.Command, app *App) {
	// ---- init ----
	root.AddCommand(&cobra.Command{
		Use:   "init",
		Short: "First-run setup wizard (walks defaults, sets the console password)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if app.Cfg.Meta.FirstRunDone {
				return errors.New("already configured — use `qfn config` to edit (or `qfn config reset` first)")
			}
			if !isTTY() {
				return errors.New("init needs a terminal (or edit " + config.Path() + " by hand)")
			}
			out, pw, err := Wizard(app.Cfg, false)
			if err != nil {
				if err.Error() == "not saved" {
					return nil
				}
				return err
			}
			out.Meta.FirstRunDone = true
			if err := Persist(out, pw); err != nil {
				return err
			}
			okf("config written: %s", config.Path())
			okf("console password stored (age-encrypted): %s", config.Dir())
			if err := OfferProfileAfterSetup(app, out); err != nil {
				return err
			}
			dimf("next: `qfn pull` (fetch weights) or `qfn up` if they're already on disk")
			dimf("always-on console: `qfn service install` — then start/stop the engine from the browser")
			return nil
		},
	})

	// ---- config ----
	cfgCmd := &cobra.Command{
		Use:   "config",
		Short: "Show/edit configuration (no args = edit wizard)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !isTTY() {
				return errors.New("editing needs a terminal — use `qfn config get|set`")
			}
			return EditConfig()
		},
	}
	cfgGet := &cobra.Command{
		Use: "get [dotted.path]", Short: "print one value (or the whole file with no path)",
		Args: cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				b, err := os.ReadFile(config.Path())
				if err != nil {
					return err
				}
				fmt.Print(string(b))
				return nil
			}
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			v, err := configGetPath(cfg, args[0])
			if err != nil {
				return err
			}
			fmt.Println(v)
			return nil
		},
	}
	cfgSet := &cobra.Command{
		Use: "set <dotted.path> <value>", Short: "update one value (validated before saving)",
		Args: cobra.ExactArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			cfg, _, err := config.Load()
			if err != nil {
				return err
			}
			if err := configSetPath(&cfg, args[0], args[1]); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			if err := config.Save(cfg); err != nil {
				return err
			}
			okf("%s = %s (a running `qfn serve` hot-reloads)", args[0], args[1])
			return nil
		},
	}
	cfgReset := &cobra.Command{
		Use:   "reset",
		Short: "delete config.toml (credentials kept; --all removes those too)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			all, _ := c.Flags().GetBool("all")
			if err := os.Remove(config.Path()); err != nil && !os.IsNotExist(err) {
				return err
			}
			if all {
				for _, f := range []string{"credentials.age", "age.key"} {
					_ = os.Remove(config.Dir() + "/" + f)
				}
				okf("config + credentials cleared")
			} else {
				okf("config cleared (password kept)")
			}
			return nil
		},
	}
	cfgReset.Flags().Bool("all", false, "also remove age key + credentials")
	cfgPath := &cobra.Command{
		Use: "path", Short: "config file location", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { fmt.Println(config.Path()); return nil },
	}
	cfgCmd.AddCommand(cfgGet, cfgSet, cfgReset, cfgPath)
	root.AddCommand(cfgCmd)

	// ---- profiles ----
	pro := &cobra.Command{Use: "profiles", Short: "Named launch profiles (config.toml overlays)"}
	proList := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		names, err := profiles.List()
		if err != nil {
			return err
		}
		def := app.Cfg.Meta.DefaultProfile
		for _, n := range names {
			mark := "  "
			if n == def {
				mark = "› "
			}
			fmt.Printf("%s%s\n", mark, n)
		}
		if len(names) == 0 {
			dimf("none yet — `qfn profiles new <name> --from-current`")
		}
		return nil
	}}
	proShow := &cobra.Command{Use: "show <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		b, err := os.ReadFile(profiles.Dir() + "/" + args[0] + ".toml")
		if err != nil {
			return err
		}
		fmt.Print(string(b))
		return nil
	}}
	proNew := &cobra.Command{Use: "new <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		name := args[0]
		if err := profiles.ValidateName(name); err != nil {
			return err
		}
		from, _ := c.Flags().GetBool("from-current")
		if !from {
			return errors.New("interactive templates aren't built — snapshot with `--from-current`, then edit the file")
		}
		desc, _ := c.Flags().GetString("desc")
		p := profiles.FromEngine(name, desc, app.Cfg.Engine)
		if err := profiles.Save(p); err != nil {
			return err
		}
		okf("profile %q saved → %s/%s.toml", name, profiles.Dir(), name)
		return nil
	}}
	proNew.Flags().Bool("from-current", false, "snapshot the current engine config")
	proNew.Flags().String("desc", "created by qfn", "profile description")
	proUse := &cobra.Command{Use: "use <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if _, err := profiles.Load(args[0]); err != nil {
			return err
		}
		app.Cfg.Meta.DefaultProfile = args[0]
		if err := config.Save(app.Cfg); err != nil {
			return err
		}
		okf("default profile: %s", args[0])
		return nil
	}}
	proDel := &cobra.Command{Use: "delete <name>", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		if err := profiles.Delete(args[0]); err != nil {
			return err
		}
		okf("deleted")
		return nil
	}}
	pro.AddCommand(proList, proShow, proNew, proUse, proDel)
	root.AddCommand(pro)

	// ---- passwd ----
	root.AddCommand(&cobra.Command{
		Use:   "passwd",
		Short: "Change the console password (active sessions die)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if !isTTY() {
				return errors.New("passwd needs a terminal")
			}
			pw, err := AskPassword("new console password (≥12 chars)", 12)
			if err != nil {
				return err
			}
			if err := app.Store.SetPassword(pw); err != nil {
				return err
			}
			okf("password updated — browser sessions invalidated")
			return nil
		},
	})

	// ---- lockdown ----
	lock := &cobra.Command{Use: "lockdown", Short: "Engine lockdown: loopback bind + proxy-only API key"}
	lockOn := &cobra.Command{Use: "on", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		k, err := app.Store.EngineKey()
		if err != nil {
			return err
		}
		if k == "" {
			if _, err := app.Store.RotateEngineKey(); err != nil {
				return err
			}
		}
		app.Cfg.Engine.Lockdown = true
		app.Cfg.Engine.Bind = "127.0.0.1"
		if err := config.Save(app.Cfg); err != nil {
			return err
		}
		okf("lockdown on — engine binds loopback, only qfn holds the key. Restart the engine to apply.")
		return nil
	}}
	lockOff := &cobra.Command{Use: "off", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		app.Cfg.Engine.Lockdown = false
		app.Cfg.Engine.Bind = "0.0.0.0"
		if err := config.Save(app.Cfg); err != nil {
			return err
		}
		warnf("lockdown off — anyone on the LAN can hit the engine directly. Restart the engine to apply.")
		return nil
	}}
	lockStatus := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		fmt.Printf("config:      lockdown=%v bind=%s\n", app.Cfg.Engine.Lockdown, app.Cfg.Engine.Bind)
		k, _ := app.Store.EngineKey()
		fmt.Printf("engine key:  %s\n", yesNo(k != "", "present", "missing — `qfn lockdown on` will generate one"))
		fk, _ := app.Store.FrontKey()
		fmt.Printf("front key:   %s\n", yesNo(fk != "", "present", "not generated (cookie auth only; `qfn lockdown front-key` creates one)"))
		fmt.Printf("api keys:    require=%v named=%d (named keys are schema-ready, enforced later)\n",
			app.Cfg.Serve.RequireAPIKey, len(app.Cfg.Serve.APIKeys))
		return nil
	}}
	lockRotate := &cobra.Command{Use: "rotate-engine-key", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if _, err := app.Store.RotateEngineKey(); err != nil {
			return err
		}
		warnf("engine key rotated — restart the engine so it comes up with the new key")
		return nil
	}}
	lockFront := &cobra.Command{Use: "front-key", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		rotate, _ := c.Flags().GetBool("rotate")
		var (
			k   string
			err error
		)
		if rotate {
			k, err = app.Store.RotateFrontKey()
		} else if k, err = app.Store.FrontKey(); err == nil && k == "" {
			k, err = app.Store.RotateFrontKey() // first print generates it
		}
		if err != nil {
			return err
		}
		fmt.Println(k)
		dimf("use as `Authorization: Bearer <key>` against the console /v1 — agents never need the password")
		return nil
	}}
	lockFront.Flags().Bool("rotate", false, "generate a new key (the old bearer stops working)")
	lock.AddCommand(lockOn, lockOff, lockStatus, lockRotate, lockFront)
	root.AddCommand(lock)

	// ---- service ----
	svc := &cobra.Command{Use: "service", Short: "systemd units: reboot-persistent console + optional engine autostart"}
	svcInstall := &cobra.Command{Use: "install", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if err := firstRunGuard(app); err != nil {
			return err
		}
		withEngine, _ := c.Flags().GetBool("engine-autostart")
		bin, err := os.Executable()
		if err != nil {
			return err
		}
		m := service.New(app.StateDir(), service.Deps{})
		done, err := m.Install(c.Context(), bin, withEngine)
		if err != nil {
			return err
		}
		for _, u := range done {
			okf("installed + enabled: %s.service", u)
		}
		return nil
	}}
	svcInstall.Flags().Bool("engine-autostart", false, "also boot the engine at startup (adds ~10 min boot to every reboot)")
	svcUninstall := &cobra.Command{Use: "uninstall", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		list, _ := c.Flags().GetBool("list")
		m := service.New(app.StateDir(), service.Deps{})
		if list {
			owned, err := m.Owned(c.Context())
			if err != nil {
				return err
			}
			if len(owned.Names) == 0 {
				dimf("nothing qfn-owned installed")
				return nil
			}
			for _, n := range owned.Names {
				fmt.Printf("%s (%s)\n", n, owned.EnabledState[n])
			}
			return nil
		}
		removed, err := m.Uninstall(c.Context(), false)
		if err != nil {
			return err
		}
		for _, u := range removed {
			okf("disabled + removed: %s.service", u)
		}
		return nil
	}}
	svcUninstall.Flags().Bool("list", false, "just show what qfn owns")
	svcStatus := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		for _, ch := range doctor.ServiceState(c.Context()) {
			color := ansiOK
			if ch.Status != "ok" {
				color = "\x1b[33m"
			}
			fmt.Println(styled(fmt.Sprintf("  %-4s %s — %s", ch.Status, ch.ID, ch.Msg), color))
		}
		return nil
	}}
	svc.AddCommand(svcInstall, svcUninstall, svcStatus)
	root.AddCommand(svc)
}

func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}

// ---- dotted-path get/set (bounded surface, validated on set) ----

func configGetPath(cfg config.Config, path string) (string, error) {
	e, s := cfg.Engine, cfg.Serve
	switch path {
	case "engine.mode":
		return e.Mode, nil
	case "engine.port":
		return fmt.Sprint(e.Port), nil
	case "engine.bind":
		return e.Bind, nil
	case "engine.ctx":
		return fmt.Sprint(e.Ctx), nil
	case "engine.yarn":
		return fmt.Sprint(e.Yarn), nil
	case "engine.mtp":
		return fmt.Sprint(e.MTP), nil
	case "engine.seqs":
		return fmt.Sprint(e.Seqs), nil
	case "engine.gpu_mem":
		return fmt.Sprint(e.GpuMem), nil
	case "engine.prewarm":
		return fmt.Sprint(e.Prewarm), nil
	case "engine.prefix_cache":
		return fmt.Sprint(e.PrefixCache), nil
	case "engine.exact_topk":
		return fmt.Sprint(e.ExactTopK), nil
	case "engine.lockdown":
		return fmt.Sprint(e.Lockdown), nil
	case "engine.image":
		return e.Image, nil
	case "engine.model":
		return e.Model, nil
	case "serve.port":
		return fmt.Sprint(s.Port), nil
	case "serve.bind":
		return s.Bind, nil
	case "serve.proxy":
		return fmt.Sprint(s.Proxy), nil
	case "serve.sampling_defaults":
		return fmt.Sprint(s.SamplingDefaults), nil
	case "paths.hf_cache":
		return cfg.Paths.HFCache, nil
	case "paths.state_dir":
		return cfg.Paths.StateDir, nil
	case "meta.default_profile":
		return cfg.Meta.DefaultProfile, nil
	}
	return "", fmt.Errorf("unknown path %q (engine.<field> / serve.<field> / paths.* / meta.default_profile)", path)
}

func configSetPath(cfg *config.Config, path, val string) error {
	e, s := &cfg.Engine, &cfg.Serve
	pi := func(p *int) error { _, err := fmt.Sscanf(val, "%d", p); return err }
	pf := func(p *float64) error { _, err := fmt.Sscanf(val, "%g", p); return err }
	pb := func(p *bool) error {
		b, err := parseBool(val)
		*p = b
		return err
	}
	switch path {
	case "engine.mode":
		e.Mode = val
	case "engine.port":
		return pi(&e.Port)
	case "engine.bind":
		e.Bind = val
	case "engine.ctx":
		return pi(&e.Ctx)
	case "engine.yarn":
		return pb(&e.Yarn)
	case "engine.mtp":
		return pi(&e.MTP)
	case "engine.seqs":
		return pi(&e.Seqs)
	case "engine.gpu_mem":
		return pf(&e.GpuMem)
	case "engine.prewarm":
		return pb(&e.Prewarm)
	case "engine.prefix_cache":
		return pb(&e.PrefixCache)
	case "engine.exact_topk":
		return pb(&e.ExactTopK)
	case "engine.lockdown":
		return pb(&e.Lockdown)
	case "engine.image":
		e.Image = val
	case "engine.model":
		e.Model = val
	case "serve.port":
		return pi(&s.Port)
	case "serve.bind":
		s.Bind = val
	case "serve.proxy":
		return pb(&s.Proxy)
	case "serve.sampling_defaults":
		return pb(&s.SamplingDefaults)
	case "paths.hf_cache":
		cfg.Paths.HFCache = val
	case "paths.state_dir":
		cfg.Paths.StateDir = val
	case "meta.default_profile":
		cfg.Meta.DefaultProfile = val
	default:
		return fmt.Errorf("unknown path %q", path)
	}
	return nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true, nil
	case "false", "0", "no", "off":
		return false, nil
	}
	return false, fmt.Errorf("not a boolean: %q", v)
}
