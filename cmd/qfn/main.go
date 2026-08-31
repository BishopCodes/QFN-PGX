// Command qfn is the single static binary: engine launcher, always-on web
// console + proxy, chat REPL, and bench battery for Qwen3.8-Flash-Next on a
// single DGX Spark.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/BishopCodes/qfn-pgx/internal/cli"
)

// version is stamped with -ldflags "-X main.version=…" (see Makefile).
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Second Ctrl+C hard-exits mid-shutdown.
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "\nforced exit")
		os.Exit(130)
	}()
	os.Exit(cli.RunWithContext(ctx, version))
}
