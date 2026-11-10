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
	"time"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
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
	a := &App{Engine: eng, Store: st, Server: srv, DataDir: opts.DataDir, listener: ln, opts: opts}
	if err := a.extend(); err != nil {
		ln.Close()
		st.Close()
		return nil, err
	}
	return a, nil
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
