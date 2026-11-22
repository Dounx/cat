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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/dounx/cat/app"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/update"
)

func main() {
	update.AwaitPredecessor()
	opts := app.Options{
		MasterKey: os.Getenv("CAT_MASTER_KEY"),
	}
	flag.StringVar(&opts.Addr, "addr", envOr("CAT_ADDR", "127.0.0.1:7788"), "listen address")
	flag.StringVar(&opts.DataDir, "data", os.Getenv("CAT_DATA"), "data directory (default ~/.cat)")
	flag.StringVar(&opts.Token, "token", os.Getenv("CAT_TOKEN"), "API token; required when listening on a non-loopback address")
	plugins := flag.String("plugins", os.Getenv("CAT_PLUGINS"), "extra plugin directories, separated by "+string(os.PathListSeparator))
	flag.StringVar(&opts.MarketURL, "market", os.Getenv("CAT_MARKET"), "optional third-party plugin market index URL; \"off\" disables it")
	flag.StringVar(&opts.UpdateURL, "update", os.Getenv("CAT_UPDATE"), "update manifest URL; \"off\" never updates (default: the latest release)")
	flag.StringVar(&opts.UIDev, "ui-dev", os.Getenv("CAT_UI_DEV"), "frontend dev server URL to proxy the UI to (development)")
	flag.IntVar(&opts.KeepEvents, "keep-events", envInt("CAT_KEEP_EVENTS", engine.DefaultKeepEvents), "maximum number of recent events to retain")
	flag.IntVar(&opts.MaxConcurrent, "max-concurrent", envInt("CAT_MAX_CONCURRENT", engine.DefaultMaxConcurrent), "maximum number of concurrent rule executions")
	serializeConv := flag.Bool("serialize-conversations", envBool("CAT_SERIALIZE_CONVERSATIONS", true), "serialize runs per (rule, source, conversation) in arrival order")
	flag.Parse()
	opts.PluginDirs = filepath.SplitList(*plugins)
	opts.SerializeConversations = serializeConv

	a, err := app.New(opts)
	if err != nil {
		// Through the logger, so it reaches the log file when the failure
		// came after that was opened.
		slog.Error("catd cannot start", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// An installed update stops the hub as an interrupt does, and the new
	// version then takes this process's place.
	var restart atomic.Bool
	a.Updater.Restart = func() {
		restart.Store(true)
		stop()
	}
	if err := a.Run(ctx); err != nil && err != http.ErrServerClosed {
		slog.Error("catd stopped", "err", err)
		os.Exit(1)
	}
	if restart.Load() {
		slog.Info("starting the updated catd")
		if err := update.Replace(a.Updater.Exe, os.Args[1:]); err != nil {
			slog.Error("the updated catd did not start", "err", err)
			os.Exit(1)
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
