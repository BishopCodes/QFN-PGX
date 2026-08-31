// Package config holds QFN-PGX's configuration: built-in defaults, the commented
// TOML file at ~/.config/qfn/config.toml, named profiles that layer over it, and
// the precedence chain built-ins < config < profile < flags.
package config

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Config is the full configuration tree, mirroring the TOML sections.
type Config struct {
	Paths   Paths   `toml:"paths"`
	Engine  Engine  `toml:"engine"`
	Serve   Serve   `toml:"serve"`
	Service Service `toml:"service"`
	Meta    Meta    `toml:"meta"`
}

// Paths is the [paths] section.
type Paths struct {
	HFCache  string `toml:"hf_cache"`
	StateDir string `toml:"state_dir"`
}

// Engine is the [engine] section: launch defaults, one field per upstream
// serve.sh env var (see engine/ATTRIBUTION.md), plus the QFN-PGX lockdown deltas.
type Engine struct {
	Mode         string  `toml:"mode"`              // nvfp4 | hybrid
	PrefixCache  bool    `toml:"prefix_cache"`      // PREFIX_CACHE
	ExactTopK    bool    `toml:"exact_topk"`        // EXACT_TOPK
	Ctx          int     `toml:"ctx"`               // CTX
	Yarn         bool    `toml:"yarn"`              // YARN
	MTP          int     `toml:"mtp"`               // MTP (speculative tokens; 0 = off)
	Seqs         int     `toml:"seqs"`              // SEQS
	GpuMem       float64 `toml:"gpu_mem"`           // GPU_MEM (hard cap 0.875)
	KVDtype      string  `toml:"kv_dtype"`          // KV_DTYPE
	Prewarm      bool    `toml:"prewarm"`           // PREWARM
	Workers      int     `toml:"workers"`           // WORKERS (mmap gather threads)
	CPUSet       string  `toml:"cpuset"`            // CPUSET ("" = unpinned)
	Extra        string  `toml:"extra"`             // EXTRA (verbatim extra vllm flags)
	Port         int     `toml:"port"`              // host port published for the API
	Bind         string  `toml:"bind"`              // docker -p bind address (127.0.0.1 = lockdown default)
	Lockdown     bool    `toml:"lockdown"`          // serve engine with --api-key; only qfn holds it
	ContainerMem string  `toml:"container_mem_cap"` // docker --memory opt-in ("" = none; see doctor)
	Image        string  `toml:"image"`
	Model        string  `toml:"model"`
	Name         string  `toml:"name"` // docker container name
}

// Serve is the [serve] section: console + proxy + auth.
type Serve struct {
	Port             int      `toml:"port"`
	Bind             string   `toml:"bind"`
	Proxy            bool     `toml:"proxy"`
	MaxPromptTokens  int      `toml:"max_prompt_tokens"` // 0 = no ceiling
	AuthEnabled      bool     `toml:"auth_enabled"`      // forced true when Bind != loopback
	SamplingDefaults bool     `toml:"sampling_defaults"` // fill omitted sampling params from the checkpoint's generation_config.json
	RequireAPIKey    bool     `toml:"require_api_key"`   // future: reject cookie-only /v1 calls
	APIKeys          []APIKey `toml:"api_keys"`          // future: named machine keys (schema-ready)
}

// APIKey is a named machine key entry. Parsed and persisted today; enforcement is
// a forward-looking flag (serve.require_api_key) landing with named keys.
type APIKey struct {
	Name    string `toml:"name"`
	KeyHash string `toml:"key_hash"`
	Created string `toml:"created"`
}

// Service is the [service] section.
type Service struct {
	ServeAlwaysOn   bool `toml:"serve_always_on"`  // qfn-serve.service (reboot-persistent console)
	EngineAutostart bool `toml:"engine_autostart"` // also enable qfn-engine.service
}

// Meta is the [meta] section.
type Meta struct {
	FirstRunDone   bool   `toml:"first_run_done"`
	DefaultProfile string `toml:"default_profile"`
}

// Defaults returns the built-in configuration: the upstream serve.sh defaults,
// with QFN-PGX's loopback-bind lockdown and :8799 console added.
func Defaults() Config {
	return Config{
		Paths: Paths{
			HFCache:  "~/.cache/huggingface",
			StateDir: "~/.local/state/qfn",
		},
		Engine: Engine{
			Mode:         "nvfp4",
			PrefixCache:  true,
			ExactTopK:    true,
			Ctx:          262144,
			Yarn:         false,
			MTP:          2,
			Seqs:         8,
			GpuMem:       0.85,
			KVDtype:      "auto",
			Prewarm:      false,
			Workers:      32,
			CPUSet:       "",
			Extra:        "",
			Port:         18300,
			Bind:         "127.0.0.1",
			Lockdown:     true,
			ContainerMem: "",
			Image:        "qwen38-flash-dgx",
			Model:        "RadixArk/Qwen3.8-Flash-Next-NVFP4",
			Name:         "qwen38-flash",
		},
		Serve: Serve{
			Port:             8799,
			Bind:             "0.0.0.0",
			Proxy:            true,
			MaxPromptTokens:  0,
			AuthEnabled:      true,
			RequireAPIKey:    false,
			SamplingDefaults: false,
		},
		Service: Service{ServeAlwaysOn: true, EngineAutostart: false},
		Meta:    Meta{},
	}
}

// Dir returns the config directory (~/.config/qfn unless XDG_CONFIG_HOME is set).
func Dir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "qfn")
	}
	// invoking user's home; SUDO_USER fallback avoids the catch-22 where
	// `qfn` can't write /etc/systemd/system and `sudo qfn` can't find config.
	if os.Geteuid() == 0 {
		if su := os.Getenv("SUDO_USER"); su != "" {
			if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
				return filepath.Join(u.HomeDir, ".config", "qfn")
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "qfn")
}

// Path returns the config.toml location.
func Path() string { return filepath.Join(Dir(), "config.toml") }

// ExpandHome resolves a leading ~ in p against the current user's home.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home := ""
		if os.Geteuid() == 0 {
			if su := os.Getenv("SUDO_USER"); su != "" {
				if u, err := user.Lookup(su); err == nil && u.HomeDir != "" {
					home = u.HomeDir
				}
			}
		}
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home != "" {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// Load reads the config file. A missing file is not an error: ok=false with the
// defaults is returned so callers can offer the first-run wizard.
func Load() (cfg Config, ok bool, err error) {
	cfg = Defaults()
	b, err := os.ReadFile(Path())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, false, nil
		}
		return cfg, false, err
	}
	if err := decodeTOML(b, &cfg); err != nil {
		return cfg, false, fmt.Errorf("%s: %w", Path(), err)
	}
	return cfg, true, cfg.Validate()
}

// Save writes the commented template form of cfg to the config file.
func Save(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(Path(), []byte(Render(cfg)), 0o644)
}

// IsLoopbackBind reports whether a bind address is loopback-only.
func IsLoopbackBind(bind string) bool {
	switch bind {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	return false
}
