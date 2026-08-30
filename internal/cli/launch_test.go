package cli

import (
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
