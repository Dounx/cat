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
	"os"
	"path/filepath"
	"time"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/store"
	"github.com/dounx/cat/frontend"
)

type Options struct {
	Addr    string // listen address; ":0" style picks a free port
	DataDir string // default ~/.cat
	Token   string
}

type App struct {
	Engine  *engine.Engine
	Store   *store.Store
	Server  *api.Server
	DataDir string

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
	for _, b := range builtin.All() {
		eng.RegisterPlugin(b.Manifest, b.Factory)
	}
	ui, _ := fs.Sub(frontend.Dist, "dist")
	srv := &api.Server{Engine: eng, Token: opts.Token, UI: ui}
	a := &App{Engine: eng, Store: st, Server: srv, DataDir: opts.DataDir, listener: ln}
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
