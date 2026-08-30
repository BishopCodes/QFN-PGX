package cli

// `qfn launch <agent>` — point a coding agent at the console front door.
// The FreeToken trick worth stealing: after injecting our env, CLEAR every
// cloud provider key from the child environment, so the agent physically
// cannot fall back to a paid endpoint mid-task.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/BishopCodes/qfn-pgx/internal/engine"
)

// cloudKeys are cleared from the child env for every launch.
var cloudKeys = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "OPENAI_API_BASE",
	"GEMINI_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY", "XAI_API_KEY", "OPENROUTER_API_KEY",
	"DEEPSEEK_API_KEY", "AZURE_OPENAI_API_KEY", "WATSONX_APIKEY",
}

// LaunchPlan is the resolved launch (pure function result → unit-testable).
type LaunchPlan struct {
	Agent   string
	Binary  string
	Args    []string
	EnvSet  map[string]string
	EnvClear []string
	Note    string
}

// ChildEnv applies clear+set onto a base environment: cleared names are
// removed, set names are removed from base too (no duplicates — a key twice
// in an env block is undefined behavior) and appended last.
func (pl LaunchPlan) ChildEnv(base []string) []string {
	drop := map[string]bool{}
	for _, k := range pl.EnvClear {
		drop[k] = true
	}
	for k := range pl.EnvSet {
		drop[k] = true
	}
	out := make([]string, 0, len(base)+len(pl.EnvSet))
	for _, kv := range base {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if !drop[name] {
			out = append(out, kv)
		}
	}
	for _, k := range sortedKeys(pl.EnvSet) {
		out = append(out, k+"="+pl.EnvSet[k])
	}
	return out
}

// buildLaunch resolves the per-agent env/argv. server is the console origin
// (http://127.0.0.1:8799), key the front bearer.
func buildLaunch(agent, server, key, model string, ctxWindow int) (LaunchPlan, error) {
	pl := LaunchPlan{Agent: agent, EnvSet: map[string]string{}, EnvClear: cloudKeys}
	openaiBase := strings.TrimRight(server, "/") + "/v1"
	switch agent {
	case "claude", "claude-code":
		pl.Binary = "claude"
		pl.EnvSet = map[string]string{
			"ANTHROPIC_BASE_URL":      strings.TrimRight(server, "/"),
			"ANTHROPIC_AUTH_TOKEN":    key,
			"ANTHROPIC_API_KEY":       "",
			"ANTHROPIC_MODEL":         model,
			"ANTHROPIC_DEFAULT_OPUS_MODEL": model,
			"ANTHROPIC_DEFAULT_SONNET_MODEL": model,
			"ANTHROPIC_DEFAULT_HAIKU_MODEL": model,
			"CLAUDE_CODE_SUBAGENT_MODEL": model,
			"CLAUDE_CODE_MAX_CONTEXT_TOKENS": strconv.Itoa(ctxWindow),
			"CLAUDE_CODE_MAX_OUTPUT_TOKENS":  "16384",
			"CLAUDE_CODE_ATTRIBUTION_HEADER": "0",
		}
		// ANTHROPIC_API_KEY is cleared from the base then re-set to ""
		// explicitly (ChildEnv drops EnvSet names from base, so no dupes).
	case "codex":
		pl.Binary = "codex"
		pl.Args = []string{"--model", model}
		pl.EnvSet = map[string]string{"OPENAI_BASE_URL": openaiBase, "OPENAI_API_KEY": key}
	case "opencode":
		pl.Binary = "opencode"
		pl.EnvSet = map[string]string{"OPENAI_BASE_URL": openaiBase, "OPENAI_API_KEY": key}
		pl.Note = "opencode may also want a provider entry in its config pointing at " + openaiBase
	case "dsh":
		pl.Binary = "dsh"
		pl.EnvSet = map[string]string{"DEEPSEEK_API_KEY": key}
		pl.Note = "settings yaml written separately; run: dsh --settings <the printed path>"
	case "generic":
		pl.Binary = ""
		pl.EnvSet = map[string]string{"OPENAI_BASE_URL": openaiBase, "OPENAI_API_KEY": key}
		pl.Note = "generic mode: env is exported for the child command after `--`"
	default:
		return pl, fmt.Errorf("unknown agent %q (claude|codex|opencode|dsh|generic)", agent)
	}
	return pl, nil
}

func addLaunch(root *cobra.Command, app *App) {
	var serverFlag, modelFlag string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "launch <agent> [-- agent-args…]",
		Short: "Wire a coding agent to the console (cloud API keys cleared from its env)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			agent := args[0]
			agentArgs := args[1:]
			if c.ArgsLenAtDash() >= 0 {
				agentArgs = args[1:]
			}
			server := serverFlag
			if server == "" {
				server = app.ServeBaseURL()
				if !app.consoleUp(c.Context()) {
					return errors.New("console not reachable on " + server + " — start `qfn serve` or pass --server")
				}
			}
			key := app.FrontKey()
			if key == "" {
				k, err := app.Store.RotateFrontKey()
				if err != nil {
					return err
				}
				key = k
			}
			model := modelFlag
			if model == "" {
				model = discoverModel(c.Context(), server, key)
			}
			ctxWindow := app.LastUp().Ctx
			if ctxWindow == 0 {
				ctxWindow = app.Cfg.Engine.Ctx
			}

			pl, err := buildLaunch(agent, server, key, model, ctxWindow)
			if err != nil {
				return err
			}
			if len(agentArgs) > 0 {
				pl.Args = append(pl.Args, agentArgs...)
			}

			// dsh needs a settings file, not just env.
			var settingsPath string
			if agent == "dsh" {
				settingsPath, err = writeDshSettings(app, server, key, model, ctxWindow)
				if err != nil {
					return err
				}
			}

			// Show the plan (key masked).
			dimf("launch: %s → %s · model %s · ctx %d", agent, strings.TrimRight(server, "/")+"/v1", model, ctxWindow)
			for _, k := range sortedKeys(pl.EnvSet) {
				v := pl.EnvSet[k]
				if strings.HasSuffix(k, "_KEY") || strings.HasSuffix(k, "_TOKEN") {
					v = mask(v)
				}
				fmt.Printf("  %s=%s\n", k, v)
			}
			dimf("cleared from child env: %s", strings.Join(pl.EnvClear, ", "))
			if pl.Note != "" {
				dimf("note: %s", pl.Note)
			}
			if settingsPath != "" {
				fmt.Printf("  settings: %s\n", settingsPath)
			}
			if dryRun {
				return nil
			}

			bin := pl.Binary
			if agent == "generic" {
				if len(agentArgs) == 0 {
					return errors.New("generic mode needs a command after `--`")
				}
				bin, agentArgs = agentArgs[0], agentArgs[1:]
				pl.Args = agentArgs
			}
			path, err := exec.LookPath(bin)
			if err != nil {
				return fmt.Errorf("%s not found on PATH — install it first (qfn does not manage agent installs)", bin)
			}
			// Generic + dsh: the child command itself was after the dash.
			if agent == "generic" {
				pl.EnvClear = cloudKeys
			}
			cmd := exec.CommandContext(c.Context(), path, pl.Args...)
			cmd.Env = pl.ChildEnv(os.Environ())
			cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
			dimf("exec: %s %s", path, strings.Join(pl.Args, " "))
			if err := cmd.Run(); err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					return fmt.Errorf("%s exited: %s", bin, exit)
				}
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&serverFlag, "server", "", "front-door URL (default: configured console)")
	cmd.Flags().StringVar(&modelFlag, "model", "", "model id (default: discover via /v1/models)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the plan, launch nothing")
	root.AddCommand(cmd)
}

// discoverModel asks /v1/models (console first; engine direct when no proxy).
func discoverModel(ctx context.Context, server, key string) string {
	fallback := engine.ServedModelName
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(server, "/")+"/v1/models", nil)
	if err != nil {
		return fallback
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil || len(out.Data) == 0 {
		return fallback
	}
	return out.Data[0].ID
}

// writeDshSettings drops a ready-to-run DSH settings yaml in the state dir.
func writeDshSettings(app *App, server, key, model string, ctxWindow int) (string, error) {
	if err := os.MkdirAll(app.StateDir(), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(app.StateDir(), "dsh-launch.settings.yaml")
	yaml := fmt.Sprintf(`# generated by qfn launch dsh — points the harness at the local Spark lane
models:
  baseurl: %s
  api_key: %s
  model: %s
  max_context_tokens: %d
`, strings.TrimRight(server, "/")+"/v1", key, model, ctxWindow)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mask(s string) string {
	if s == "" {
		return `""`
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "…" + s[len(s)-4:]
}
