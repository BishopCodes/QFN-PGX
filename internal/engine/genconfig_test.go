package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationDefaultsFromSnapshot(t *testing.T) {
	hf := t.TempDir()
	snap := filepath.Join(hf, "hub", "models--RadixArk--Qwen3.8-Flash-Next-NVFP4", "snapshots", "abc123")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	gc := `{"temperature":0.6,"top_p":0.95,"top_k":20,"eos_token_id":151645,"transformers_version":"4.55"}`
	if err := os.WriteFile(filepath.Join(snap, "generation_config.json"), []byte(gc), 0o644); err != nil {
		t.Fatal(err)
	}
	got := GenerationDefaults(hf, "RadixArk/Qwen3.8-Flash-Next-NVFP4")
	if got["temperature"].(float64) != 0.6 || got["top_p"].(float64) != 0.95 || got["top_k"].(float64) != 20 {
		t.Fatalf("defaults: %v", got)
	}
	if _, ok := got["eos_token_id"]; ok {
		t.Fatal("non-sampling keys must be dropped")
	}

	// Missing checkpoint → nil (feature degrades silently, never blocks).
	if GenerationDefaults(t.TempDir(), "some/model") != nil {
		t.Fatal("want nil for absent checkpoint")
	}

	// Hybrid sibling must not shadow the real snapshot.
	hf2 := t.TempDir()
	real := filepath.Join(hf2, "hub", "models--o--m", "snapshots", "rev1")
	hyb := real + "-fp8hybrid"
	os.MkdirAll(real, 0o755)
	os.MkdirAll(hyb, 0o755)
	os.WriteFile(filepath.Join(hyb, "generation_config.json"), []byte(`{"temperature":9.9}`), 0o644)
	os.WriteFile(filepath.Join(real, "generation_config.json"), []byte(`{"temperature":0.7}`), 0o644)
	got = GenerationDefaults(hf2, "o/m")
	if got["temperature"].(float64) != 0.7 {
		t.Fatalf("hybrid dir shadowed the real snapshot: %v", got)
	}
}
