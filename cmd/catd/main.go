// Command catd runs the event hub headless: the engine, its HTTP API and the
// embedded web UI, with no desktop window.
package main

import (
	"context"
	"flag"
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
	flag.StringVar(&opts.MarketURL, "market", os.Getenv("CAT_MARKET"), "plugin market index URL")
	flag.StringVar(&opts.UIDev, "ui-dev", os.Getenv("CAT_UI_DEV"), "frontend dev server URL to proxy the UI to (development)")
	flag.Parse()
	opts.PluginDirs = filepath.SplitList(*plugins)

	a, err := app.New(opts)
	if err != nil {
		// Through the logger, so it reaches the log file when the failure
		// came after that was opened.
		slog.Error("catd cannot start", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil && err != http.ErrServerClosed {
		slog.Error("catd stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
