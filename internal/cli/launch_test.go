package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildLaunchClaude(t *testing.T) {
	pl, err := buildLaunch("claude", "http://127.0.0.1:8799", "fk-1234567890", "qwen3.8-flash-next", 262144)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Binary != "claude" {
		t.Fatalf("binary: %s", pl.Binary)
	}
	if pl.EnvSet["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8799" {
		t.Fatalf("base url: %v", pl.EnvSet["ANTHROPIC_BASE_URL"])
	}
	if pl.EnvSet["ANTHROPIC_AUTH_TOKEN"] != "fk-1234567890" || pl.EnvSet["ANTHROPIC_API_KEY"] != "" {
		t.Fatalf("auth env: %v", pl.EnvSet)
	}
	if pl.EnvSet["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "262144" {
		t.Fatal("ctx window must be advertised (compaction math)")
	}
	for _, m := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL"} {
		if pl.EnvSet[m] != "qwen3.8-flash-next" {
			t.Fatalf("%s: %v", m, pl.EnvSet[m])
		}
	}
}

func TestBuildLaunchCodexAndGeneric(t *testing.T) {
	pl, err := buildLaunch("codex", "http://h:1/", "k", "m1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pl.EnvSet["OPENAI_BASE_URL"] != "http://h:1/v1" { // trailing slash normalized
		t.Fatalf("base: %v", pl.EnvSet["OPENAI_BASE_URL"])
	}
	if pl.Args[0] != "--model" {
		t.Fatalf("args: %v", pl.Args)
	}
	if _, err := buildLaunch("bogus", "x", "k", "m", 1); err == nil {
		t.Fatal("unknown agent must error")
	}
}

func TestBuildLaunchOpencode(t *testing.T) {
	pl, err := buildLaunch("opencode", "http://h:1/", "k", "m1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Binary != "opencode" {
		t.Fatalf("binary: %s", pl.Binary)
	}
	// The harness config must carry the front key, not the built-in openai
	// provider. OPENAI_API_KEY would make opencode's default openai provider
	// (models.dev list, api.openai.com) read the front key — leaked and useless.
	if _, ok := pl.EnvSet["OPENAI_API_KEY"]; ok {
		t.Fatalf("opencode must not get OPENAI_API_KEY: %v", pl.EnvSet)
	}
	if _, ok := pl.EnvSet["OPENCODE_CONFIG"]; ok {
		t.Fatal("OPENCODE_CONFIG is resolved by addLaunch (needs StateDir), not the pure plan")
	}
}

func TestOpencodeConfigJSON(t *testing.T) {
	b, err := opencodeConfigJSON("http://127.0.0.1:8799/", "fk-1234567890", "qwen3.8-flash-next", 262144)
	if err != nil {
		t.Fatal(err)
	}
	var c opencodeConfig
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("generated opencode config must be valid JSON: %v", err)
	}
	if c.Schema != "https://opencode.ai/config.json" {
		t.Fatalf("schema: %s", c.Schema)
	}
	if c.Model != "qfn/qwen3.8-flash-next" {
		t.Fatalf("default model: %s", c.Model)
	}
	p, ok := c.Provider["qfn"]
	if !ok {
		t.Fatal("provider key qfn missing")
	}
	if p.NPM != "@ai-sdk/openai-compatible" {
		t.Fatalf("npm: %s", p.NPM)
	}
	if p.Options["baseURL"] != "http://127.0.0.1:8799/v1" {
		t.Fatalf("baseURL: %v", p.Options["baseURL"])
	}
	if p.Options["apiKey"] != "fk-1234567890" {
		t.Fatalf("apiKey: %v", p.Options["apiKey"])
	}
	m, ok := p.Models["qwen3.8-flash-next"]
	if !ok {
		t.Fatal("served model must be registered under the provider")
	}
	if m.Limit.Context != 262144 || m.Limit.Output != 16384 {
		t.Fatalf("limits: %+v", m.Limit)
	}
}

func TestChildEnvClearsCloudKeysWithoutDuplicates(t *testing.T) {
	pl, err := buildLaunch("codex", "http://h:1", "fronty", "m", 1)
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=/usr/bin",
		"OPENAI_API_KEY=sk-cloud-secret",   // must vanish
		"ANTHROPIC_API_KEY=sk-ant-secret", // must vanish
		"HOME=/home/bishop",
		"OPENAI_BASE_URL=http://evil:1/v1", // must be replaced exactly once
	}
	env := pl.ChildEnv(base)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "sk-cloud-secret") || strings.Contains(joined, "sk-ant-secret") {
		t.Fatalf("cloud key survived: %s", joined)
	}
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "OPENAI_BASE_URL=") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("OPENAI_BASE_URL occurrences: %d (%s)", n, joined)
	}
	if strings.Contains(joined, "OPENAI_BASE_URL=http://evil") {
		t.Fatal("stale base url survived")
	}
	if !strings.Contains(joined, "OPENAI_API_KEY=fronty") {
		t.Fatal("front key not injected")
	}
	if !strings.Contains(joined, "HOME=/home/bishop") {
		t.Fatal("unrelated vars must pass through")
	}
}
