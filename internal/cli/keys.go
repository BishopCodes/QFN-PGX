package cli

// `qfn keys`: named machine API keys for anything that talks to the front
// door from OUTSIDE this box's launch command — opencode on a laptop, a
// harness on another machine, a script. Each key is revocable on its own and
// shows up BY NAME in the console's request feed, so "who is hammering me"
// always has an answer. The plaintext is printed exactly once; only its
// SHA-256 lives in credentials.age.

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func addKeys(root *cobra.Command, app *App) {
	k := &cobra.Command{
		Use:   "keys",
		Short: "Manage named API keys for the front door",
		Long: `Named machine keys let external tools (opencode, other harnesses, scripts)
authenticate against the console's /v1 front door.

  qfn keys add opencode      # print the one-time plaintext
  qfn keys list              # names, previews, creation dates
  qfn keys rm opencode       # revoke immediately (no restart)

Point the tool at http://<spark>:8799 with Authorization: Bearer <key>.
Unlike the shared front key, these are individually revocable and each
request is attributed to the key's name in the dashboard.`,
	}
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a named key (plaintext shown once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			tok, err := app.Store.AddAPIKey(args[0])
			if err != nil {
				return err
			}
			okf("key %q created", args[0])
			dimf("  copy now — it is never shown again:\n")
			fmt.Printf("  %s\n", tok)
			dimf("usage: base_url http://<this-box>:%d  ·  Authorization: Bearer <key>\n", app.Cfg.Serve.Port)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List named keys (never the values)",
		RunE: func(c *cobra.Command, _ []string) error {
			keys, err := app.Store.ListAPIKeys()
			if err != nil {
				return err
			}
			if len(keys) == 0 {
				dimf("no named keys yet — `qfn keys add <name>` creates one\n")
				return nil
			}
			for _, k := range keys {
				fmt.Printf("  %-16s %s  created %s\n", k.Name, k.Preview,
					time.Unix(k.Created, 0).Format("2006-01-02"))
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"revoke", "remove"},
		Short:   "Revoke a named key (effective immediately)",
		Args:    cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := app.Store.RevokeAPIKey(args[0]); err != nil {
				return err
			}
			okf("key %q revoked — next request with it gets 401", args[0])
			return nil
		},
	}
	k.AddCommand(add, list, rm)
	root.AddCommand(k)
}
