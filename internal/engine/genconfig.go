package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// GenerationDefaults reads the checkpoint's generation_config.json from the
// local HF snapshot and returns the recommended sampling params, so the proxy
// can fill omitted fields with the values the checkpoint was tuned for
// (FreeToken's --sampling-defaults idea). Only well-known sampling keys are
// returned; anything exotic in the file is deliberately dropped.
//
// Cached per (cache, model): the file is static once the snapshot exists.
func GenerationDefaults(hfCacheHost, model string) map[string]any {
	genKey := hfCacheHost + "|" + model
	genMu.Lock()
	defer genMu.Unlock()
	if v, ok := genCache[genKey]; ok {
		return v
	}
	out := readGenerationConfig(hfCacheHost, model)
	genCache[genKey] = out
	return out
}

var (
	genMu   sync.Mutex
	genCache = map[string]map[string]any{}
)

func readGenerationConfig(hfCacheHost, model string) map[string]any {
	repo := filepath.Join(hfCacheHost, "hub", "models--"+strings.ReplaceAll(model, "/", "--"), "snapshots")
	entries, err := os.ReadDir(repo)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), "-fp8hybrid") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(repo, e.Name(), "generation_config.json"))
		if err != nil {
			continue
		}
		var raw map[string]any
		if json.Unmarshal(b, &raw) != nil {
			continue
		}
		out := map[string]any{}
		for _, key := range []string{"temperature", "top_p", "top_k", "min_p", "repetition_penalty", "frequency_penalty", "presence_penalty"} {
			if v, ok := raw[key]; ok {
				out[key] = v
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	return nil
}
