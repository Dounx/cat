// Package app assembles the engine, storage, plugins and HTTP server. Both
// shells (headless catd and the desktop app) start from here.
package app

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/logfile"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/store"
	"github.com/dounx/cat/frontend"
)

type Options struct {
	Addr    string // listen address; ":0" style picks a free port
	DataDir string // default ~/.cat
	Token   string
	// UIDev is the URL of a running frontend dev server. When set the hub
	// proxies the UI to it instead of serving the embedded build.
	UIDev string
	// PluginDirs are extra directories searched for plugins, ahead of
	// <data>/plugins and the plugins directory beside the executable.
	PluginDirs []string
	// MarketURL is the plugin market index (http(s):// or file://).
	MarketURL string
	// Secrets overrides where credentials are kept (default: a private file
	// in the data directory).
	Secrets engine.Secrets
	// Desktop marks the desktop shell; OpenURL opens a link in the browser.
	Desktop bool
	OpenURL func(url string) error
}

type App struct {
	Engine  *engine.Engine
	Store   *store.Store
	Server  *api.Server
	Plugins *pluginhost.Host
	DataDir string
	// LogPath is the file the log is also written to; empty when it could
	// not be opened.
	LogPath string
	opts    Options

	listener net.Listener
	http     *http.Server
}

func New(opts Options) (*App, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:7788"
	}
	if opts.DataDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		opts.DataDir = filepath.Join(home, ".cat")
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, err
	}
	// Before anything else logs: the engine and the plugins take the default
	// logger as it is when they are created.
	logPath := logToFile(opts.DataDir)
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, err
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok && !tcp.IP.IsLoopback() && opts.Token == "" {
		ln.Close()
		return nil, fmt.Errorf("listening on %s exposes the hub beyond this machine; set --token", opts.Addr)
	}
	st, err := store.Open(filepath.Join(opts.DataDir, "cat.db"))
	if err != nil {
		ln.Close()
		return nil, err
	}
	eng := engine.New(st, opts.DataDir, slog.Default())
	eng.Secrets = opts.Secrets
	if eng.Secrets == nil {
		eng.Secrets = &engine.FileSecrets{Path: filepath.Join(opts.DataDir, "secrets.json")}
	}
	for _, b := range builtin.All() {
		eng.RegisterPlugin(b.Manifest, b.Factory)
	}
	ui, _ := fs.Sub(frontend.Dist, "dist")
	srv := &api.Server{Engine: eng, Token: opts.Token, UI: ui}
	if opts.UIDev != "" {
		dev, err := url.Parse(opts.UIDev)
		if err != nil || dev.Host == "" {
			ln.Close()
			st.Close()
			return nil, fmt.Errorf("ui dev server %q: not a URL", opts.UIDev)
		}
		srv.UIDev = dev
	}
	a := &App{Engine: eng, Store: st, Server: srv, DataDir: opts.DataDir, LogPath: logPath, listener: ln, opts: opts}
	if err := a.extend(); err != nil {
		ln.Close()
		st.Close()
		return nil, err
	}
	return a, nil
}

// logToFile makes the default logger write to <data>/logs/cat.log as well as
// stderr, and returns the file's path. The desktop app has no terminal to
// show stderr, so the file is the only place its log can be read afterwards.
// The file also takes debug lines, which stderr leaves out. A log file that
// cannot be opened is reported and otherwise ignored: the hub still runs.
func logToFile(dataDir string) string {
	path := filepath.Join(dataDir, "logs", "cat.log")
	w, err := logfile.Open(path)
	if err != nil {
		slog.Warn("log file", "err", err)
		return ""
	}
	slog.SetDefault(slog.New(slog.NewMultiHandler(
		slog.NewTextHandler(os.Stderr, nil),
		slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}),
	)))
	keepCrashes(filepath.Dir(path))
	return path
}

// keepCrashes has the Go runtime write the stack of a panic or fatal error to
// <logs>/crash.log as well as stderr. That report never goes through the
// logger, so without this a crash of the desktop app leaves nothing behind.
// A report left by the previous run is set aside under the time it was
// written and announced in the log, so it is noticed and not overwritten.
func keepCrashes(dir string) {
	path := filepath.Join(dir, "crash.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 0 {
		kept := filepath.Join(dir, "crash-"+st.ModTime().Format("20060102-150405")+".log")
		if err := os.Rename(path, kept); err == nil {
			slog.Warn("the previous run crashed", "at", st.ModTime().Format(time.RFC3339), "report", kept)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		slog.Warn("crash file", "err", err)
		return
	}
	// The runtime keeps its own handle on the file.
	defer f.Close()
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		slog.Warn("crash file", "err", err)
	}
}

// URL is where the UI and API are reachable locally.
func (a *App) URL() string {
	addr := a.listener.Addr().(*net.TCPAddr)
	host := addr.IP.String()
	if addr.IP.IsUnspecified() {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s", net.JoinHostPort(host, fmt.Sprint(addr.Port)))
}

// Run starts the engine and serves until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	if err := a.Engine.Start(); err != nil {
		return err
	}
	slog.Info("cat listening", "url", a.URL(), "data", a.DataDir, "log", a.LogPath)
	a.http = &http.Server{Handler: a.Server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- a.http.Serve(a.listener) }()
	select {
	case err := <-errc:
		a.shutdown()
		return err
	case <-ctx.Done():
		a.shutdown()
		return nil
	}
}

func (a *App) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = a.http.Shutdown(ctx)
	a.Engine.Stop()
	_ = a.Store.Close()
}
