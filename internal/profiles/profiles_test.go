package profiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/BishopCodes/qfn-pgx/internal/config"
)

func useTempDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestApplyOnlyOverridesSetFields(t *testing.T) {
	e := config.Defaults().Engine
	on := true
	ctx := 500000
	(&Profile{Yarn: &on, Ctx: &ctx}).Apply(&e)
	if !e.Yarn || e.Ctx != 500000 {
		t.Fatalf("overlay missed: %+v", e)
	}
	if e.Mode != "nvfp4" || e.MTP != 2 {
		t.Fatalf("untouched fields changed: %+v", e)
	}
}

func TestPrecedenceDefaultProfileFlag(t *testing.T) {
	base := config.Defaults().Engine
	profMTP := 3
	mode := "hybrid"
	flagCtx := 128000
	(&Profile{MTP: &profMTP}).Apply(&base)
	(&Profile{Mode: &mode}).Apply(&base) // later overlay (flags) wins for its own fields
	(&Profile{Ctx: &flagCtx}).Apply(&base)
	if base.MTP != 3 || base.Mode != "hybrid" || base.Ctx != 128000 {
		t.Fatalf("precedence wrong: %+v", base)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	useTempDir(t)
	mode := "hybrid"
	pw := true
	p := &Profile{Name: "fast", Description: "hybrid + prewarm", Mode: &mode, Prewarm: &pw}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load("fast")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "fast" || got.Description != "hybrid + prewarm" {
		t.Fatalf("meta lost: %+v", got)
	}
	if got.Mode == nil || *got.Mode != "hybrid" || got.Prewarm == nil || !*got.Prewarm {
		t.Fatalf("values lost: %+v", got)
	}
	if got.Ctx != nil {
		t.Fatal("unset field must stay nil")
	}
}

func TestListAndDelete(t *testing.T) {
	useTempDir(t)
	e := config.Defaults().Engine
	for _, n := range []string{"b", "a"} {
		if err := Save(FromEngine(n, "", e)); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(Dir(), "notes.txt"), []byte("x"), 0o644) // ignored
	names, err := List()
	if err != nil || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Fatalf("names=%v err=%v", names, err)
	}
	if err := Delete("a"); err != nil {
		t.Fatal(err)
	}
	names, _ = List()
	if len(names) != 1 {
		t.Fatalf("delete failed: %v", names)
	}
}

func TestNameValidation(t *testing.T) {
	for _, bad := range []string{"", "A-bad", "up/../down", "spa ce", "a/b", "x~"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("name %q must be rejected", bad)
		}
	}
	for _, ok := range []string{"daily", "a.b_c-1", "max-tokens"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("name %q must be accepted: %v", ok, err)
		}
	}
}
