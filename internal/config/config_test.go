package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func useTempDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestDefaultsAreValid(t *testing.T) {
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("defaults must validate: %v", err)
	}
	d := Defaults()
	if d.Engine.Port == d.Serve.Port {
		t.Fatal("default engine and serve ports must differ")
	}
	if d.Engine.Port != 18300 || d.Serve.Port != 8799 {
		t.Fatalf("expected 18300/8799, got %d/%d", d.Engine.Port, d.Serve.Port)
	}
	if !d.Engine.Lockdown || d.Engine.Bind != "127.0.0.1" {
		t.Fatal("lockdown + loopback bind must be the default posture")
	}
}

func TestLoadMissingReturnsNotOK(t *testing.T) {
	useTempDir(t)
	cfg, ok, err := Load()
	if err != nil || ok {
		t.Fatalf("missing file must be (defaults,false,nil), got ok=%v err=%v", ok, err)
	}
	if cfg.Serve.Port != 8799 {
		t.Fatal("defaults expected")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	useTempDir(t)
	cfg := Defaults()
	cfg.Engine.Mode = "hybrid"
	cfg.Engine.Ctx = 500000
	cfg.Engine.Yarn = true
	cfg.Engine.GpuMem = 0.80
	cfg.Engine.Prewarm = true
	cfg.Meta.FirstRunDone = true
	cfg.Meta.DefaultProfile = "daily"
	if err := Save(cfg); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if got.Engine.Mode != "hybrid" || got.Engine.Ctx != 500000 || !got.Engine.Yarn ||
		got.Engine.GpuMem != 0.80 || !got.Engine.Prewarm || !got.Meta.FirstRunDone ||
		got.Meta.DefaultProfile != "daily" {
		t.Fatalf("round trip lost values: %+v", got)
	}
}

func TestPartialFileLayersOverDefaults(t *testing.T) {
	useTempDir(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// Only one key in the file; everything else must keep defaults.
	if err := os.WriteFile(Path(), []byte("[engine]\nmode = \"hybrid\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, ok, err := Load()
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if cfg.Engine.Mode != "hybrid" {
		t.Fatal("override not applied")
	}
	if cfg.Engine.MTP != 2 || cfg.Serve.Port != 8799 {
		t.Fatalf("defaults clobbered: %+v", cfg)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	useTempDir(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("[engine]\nmode = \"hybrid\"\nmispell = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load()
	if err == nil || !strings.Contains(err.Error(), "mispell") {
		t.Fatalf("expected unknown-key error naming 'mispell', got %v", err)
	}
}

func TestValidateTable(t *testing.T) {
	type tc struct {
		name  string
		mut   func(*Config)
		valid bool
	}
	cases := []tc{
		{"defaults", func(*Config) {}, true},
		{"bad mode", func(c *Config) { c.Engine.Mode = "int4" }, false},
		{"mtp 5", func(c *Config) { c.Engine.MTP = 5 }, false},
		{"mtp 0 ok", func(c *Config) { c.Engine.MTP = 0 }, true},
		{"ctx over native without yarn", func(c *Config) { c.Engine.Ctx = 300000 }, false},
		{"ctx over native with yarn", func(c *Config) { c.Engine.Ctx = 500000; c.Engine.Yarn = true }, true},
		{"gpu_mem 0.88", func(c *Config) { c.Engine.GpuMem = 0.88 }, false},
		{"gpu_mem 0.875 boundary ok", func(c *Config) { c.Engine.GpuMem = 0.875 }, true},
		{"kv fp8 ok", func(c *Config) { c.Engine.KVDtype = "fp8" }, true},
		{"kv bogus", func(c *Config) { c.Engine.KVDtype = "int8" }, false},
		{"bind hostname", func(c *Config) { c.Engine.Bind = "example.com" }, false},
		{"bind 0.0.0.0 ok", func(c *Config) { c.Engine.Bind = "0.0.0.0" }, true},
		{"same ports", func(c *Config) { c.Serve.Port = 18300 }, false},
		{"mem cap shape ok", func(c *Config) { c.Engine.ContainerMem = "100g" }, true},
		{"mem cap shape bad", func(c *Config) { c.Engine.ContainerMem = "100 gigs" }, false},
		{"require key w/o keys", func(c *Config) { c.Serve.RequireAPIKey = true }, false},
		{"require key w/ keys", func(c *Config) {
			c.Serve.RequireAPIKey = true
			c.Serve.APIKeys = []APIKey{{Name: "ci", KeyHash: "sha256:x"}}
		}, true},
		{"exposed bind forces auth on load", func(c *Config) { c.Serve.Bind = "0.0.0.0"; c.Serve.AuthEnabled = false }, false},
	}
	for _, c := range cases {
		cfg := Defaults()
		c.mut(&cfg)
		err := cfg.Validate()
		if (err == nil) != c.valid {
			t.Errorf("%s: valid=%v, err=%v", c.name, c.valid, err)
		}
	}
}

func TestExposedBindForcesAuthOnLoad(t *testing.T) {
	useTempDir(t)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "[serve]\nbind = \"0.0.0.0\"\nauth_enabled = false\n"
	if err := os.WriteFile(Path(), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	// Load must reject the invalid combo rather than silently flipping auth,
	// so the user sees the rule (never launch on invalid config).
	if _, _, err := Load(); err == nil {
		t.Fatal("expected validation error for off-loopback bind with auth off")
	}
}

func TestRenderCommentsPresent(t *testing.T) {
	out := Render(Defaults())
	for _, want := range []string{"[engine]", "[serve]", "lockdown", "8799", "18300", "# "} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered template missing %q", want)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip()
	}
	if got := ExpandHome("~/.cache/x"); got != filepath.Join(home, ".cache/x") {
		t.Fatalf("got %q", got)
	}
	if got := ExpandHome("/abs/path"); got != "/abs/path" {
		t.Fatalf("got %q", got)
	}
}
