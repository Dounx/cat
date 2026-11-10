// Command catd runs the event hub headless: the engine, its HTTP API and the
// embedded web UI, with no desktop window.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/dounx/cat/app"
)

func main() {
	opts := app.Options{}
	flag.StringVar(&opts.Addr, "addr", envOr("CAT_ADDR", "127.0.0.1:7788"), "listen address")
	flag.StringVar(&opts.DataDir, "data", os.Getenv("CAT_DATA"), "data directory (default ~/.cat)")
	flag.StringVar(&opts.Token, "token", os.Getenv("CAT_TOKEN"), "API token; required when listening on a non-loopback address")
	plugins := flag.String("plugins", os.Getenv("CAT_PLUGINS"), "extra plugin directories, separated by "+string(os.PathListSeparator))
	flag.Parse()
	opts.PluginDirs = filepath.SplitList(*plugins)

	a, err := app.New(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catd:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("cat listening", "url", a.URL(), "data", a.DataDir)
	if err := a.Run(ctx); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, "catd:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
