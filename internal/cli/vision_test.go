package cli

// `qfn up` has to say what the configured snapshot can do with images — the
// three postures (OK / no processor file / no vision tower) plus the two that
// must not scare anyone (text-only lane, checkpoint we can't resolve).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BishopCodes/qfn-pgx/internal/config"
	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

// snapshotAt lays down a pulled checkpoint under a throwaway HF cache.
func snapshotAt(t *testing.T, files map[string]string) engine.SnapshotLocator {
	t.Helper()
	hf := t.TempDir()
	dir := filepath.Join(hf, "hub", "models--acme--visioney", "snapshots", "rev0000")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return engine.NewSnapshotLocator(hf)
}

func TestVisionLinePostures(t *testing.T) {
	const (
		visionJSON = `{"vision_config":{"hidden_size":1280}}`
		plainJSON  = `{"architectures":["ForCausalLM"]}`
		procJSON   = `{"processor_class":"Qwen3VLProcessor"}`
	)
	cases := []struct {
		name   string
		files  map[string]string // nil = nothing pulled
		images int
		want   string // "" = must print nothing; else must be a substring
		warn   bool
	}{
		{"text-only lane says nothing", map[string]string{"config.json": visionJSON, "processor_config.json": procJSON}, 0, "", false},
		{"unresolved snapshot stays neutral", nil, 4, "unknown", false},
		{"full mm support is an ok line", map[string]string{"config.json": visionJSON, "processor_config.json": procJSON}, 4, "snapshot OK", false},
		{"missing processor file warns by name", map[string]string{"config.json": visionJSON}, 4, "processor_config.json", true},
		{"no vision tower warns about the model", map[string]string{"config.json": plainJSON, "processor_config.json": procJSON}, 4, "vision_config", true},
		// A dir with no config.json says nothing about towers — report unknown,
		// never "no vision_config" (that would send the owner to diff a file
		// that isn't there).
		{"unreadable config warns as unknown", map[string]string{"processor_config.json": procJSON}, 4, "posture unknown", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loc := snapshotAt(t, tc.files)
			if tc.files == nil {
				loc = engine.NewSnapshotLocator(t.TempDir())
			}
			eng := config.Engine{Model: "acme/visioney", Mode: "nvfp4", Images: tc.images}
			line, warn := visionLine(loc, eng)
			if tc.want == "" {
				if line != "" {
					t.Fatalf("want silence, got %q", line)
				}
				return
			}
			if !strings.Contains(line, tc.want) {
				t.Fatalf("line %q lacks %q", line, tc.want)
			}
			if warn != tc.warn {
				t.Fatalf("warn=%v want %v (line %q)", warn, tc.warn, line)
			}
		})
	}
}
