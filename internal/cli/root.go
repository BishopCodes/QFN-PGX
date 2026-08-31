package cli

// Root command: `qfn` with no completed setup launches the wizard (TTY) or
// points at `qfn init` (scripts). Version/build stamp via -ldflags.

import (
	"context"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Execute is main()'s entry point.
func Execute(build string) int { return RunWithContext(context.Background(), build) }

// RunWithContext wires the signal-scoped ctx into every command.
func RunWithContext(ctx context.Context, build string) int {
	root := &cobra.Command{
		Use:           "qfn",
		Short:         "Qwen3.8-Flash-Next on one DGX Spark",
		Long:          "QFN-PGX — serve, watch, chat, and benchmark Qwen3.8-Flash-Next on a single GB10 via PLE-mmap.",
		Version:       build,
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	app, err := Load()
	if err != nil {
		badf("%v", err)
		return 1
	}
	root.SetHelpTemplate(root.HelpTemplate() + "\n" +
		"Get started:  qfn init   →  qfn pull   →  qfn up   →  qfn serve (or open http://localhost:8799)\n")

	addLifecycle(root, app)
	addServe(root, app)
	addSetup(root, app)  // init/config/profiles/passwd/lockdown/service
	addTools(root, app)  // doctor/bench/chat/stats/build/pull/prepare-hybrid
	addLaunch(root, app)
	addKeys(root, app) // wire coding agents to the front door

	root.SetContext(ctx)
	if err := root.ExecuteContext(ctx); err != nil {
		return 1
	}
	return 0
}

// firstRunGuard is consulted by commands that need a completed setup.
func firstRunGuard(app *App) error { return app.requireConfig() }

// isTTY reports whether we can prompt.
func isTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
