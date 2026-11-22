package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/market"
	"github.com/dounx/cat/core/mcp"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/update"
)

// extend wires optional subsystems (external plugins, setup wizards, the
// plugin market) into the engine and API.
func (a *App) extend() error {
	dirs := append([]string{}, a.opts.PluginDirs...)
	dirs = append(dirs, a.PluginDir())
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Join(filepath.Dir(exe), "plugins"))
	}
	if err := os.MkdirAll(a.PluginDir(), 0o755); err != nil {
		return err
	}
	a.Plugins = pluginhost.New(a.Engine, dirs...)
	for _, err := range a.Plugins.Scan() {
		slog.Warn("plugin skipped", "err", err)
	}
	a.Server.Extend(a.pluginRoutes)
	mk := market.New(a.opts.MarketURL, a.PluginDir(), a.Plugins, a.Engine)
	mk.URLFunc = func() string {
		if v := strings.TrimSpace(a.opts.MarketURL); v != "" {
			if strings.EqualFold(v, "off") {
				return ""
			}
			return v
		}
		if v, ok := a.Engine.Store.KVGet("settings", "market_url"); ok && strings.TrimSpace(v) != "" {
			if strings.EqualFold(strings.TrimSpace(v), "off") {
				return ""
			}
			return strings.TrimSpace(v)
		}
		return market.DefaultURL
	}
	a.Server.Extend(func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/market", func(w http.ResponseWriter, r *http.Request) {
			eff := mk.EffectiveURL()
			locked := strings.TrimSpace(a.opts.MarketURL) != ""
			list, err := mk.List(r.Context())
			if err != nil {
				api.WriteJSON(w, 200, map[string]any{
					"configured":  eff != "",
					"url":         eff,
					"default_url": market.DefaultURL,
					"locked":      locked,
					"error":       err.Error(),
					"plugins":     []any{},
				})
				return
			}
			api.WriteJSON(w, 200, map[string]any{
				"configured":  true,
				"url":         eff,
				"default_url": market.DefaultURL,
				"locked":      locked,
				"plugins":     list,
			})
		})
		mux.HandleFunc("POST /api/market/{id}/install", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			var last time.Time
			err := mk.Install(r.Context(), id, func(done, total int64) {
				if time.Since(last) > 200*time.Millisecond || done == total {
					last = time.Now()
					a.Engine.Publish("download", map[string]any{"plugin": id, "done": done, "total": total})
				}
			})
			if err != nil {
				api.WriteErr(w, 400, err.Error())
				return
			}
			api.WriteJSON(w, 200, map[string]bool{"ok": true})
		})
		mux.HandleFunc("DELETE /api/market/{id}", func(w http.ResponseWriter, r *http.Request) {
			if err := mk.Uninstall(r.PathValue("id")); err != nil {
				api.WriteErr(w, 400, err.Error())
				return
			}
			api.WriteJSON(w, 200, map[string]bool{"ok": true})
		})
	})
	a.Server.Extend(a.mcpRoutes)
	kind := update.Catd
	if a.opts.Desktop {
		kind = update.Desktop
	}
	a.Updater = update.New(a.opts.UpdateURL, Version, kind, filepath.Join(a.DataDir, "update"))
	var told string
	a.Updater.Changed = func(s update.Status) {
		// Once per state: a download reports its progress many times.
		if state := s.State + s.Latest; state != told {
			told = state
			slog.Info("update", "state", s.State, "running", s.Version, "latest", s.Latest, "reason", s.Reason)
		}
		a.Engine.Publish("update", s)
	}
	a.Server.Extend(a.updateRoutes)
	a.Server.Desktop = a.opts.Desktop
	if open := a.opts.OpenURL; open != nil {
		a.Server.Extend(func(mux *http.ServeMux) {
			mux.HandleFunc("POST /api/open-url", func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					URL string `json:"url"`
				}
				if !api.ReadJSON(w, r, &body) {
					return
				}
				if !strings.HasPrefix(body.URL, "https://") && !strings.HasPrefix(body.URL, "http://") {
					api.WriteErr(w, 400, "only http(s) links can be opened")
					return
				}
				if err := open(body.URL); err != nil {
					api.WriteErr(w, 500, err.Error())
					return
				}
				api.WriteJSON(w, 200, map[string]bool{"ok": true})
			})
		})
	}
	if openLogs := a.opts.OpenLogs; openLogs != nil {
		a.Server.Extend(func(mux *http.ServeMux) {
			mux.HandleFunc("POST /api/open-logs", func(w http.ResponseWriter, r *http.Request) {
				if err := openLogs(); err != nil {
					api.WriteErr(w, 500, err.Error())
					return
				}
				api.WriteJSON(w, 200, map[string]bool{"ok": true})
			})
		})
	}
	return nil
}

// MCPURL is where an AI on this machine reaches the hub's MCP server.
func (a *App) MCPURL() string {
	u := a.URL() + "/api/mcp"
	if a.opts.Token != "" {
		u += "?token=" + url.QueryEscape(a.opts.Token)
	}
	return u
}

// mcpRoutes serves the hub's tools to AIs, and installs that server into the
// harnesses on this machine.
func (a *App) mcpRoutes(mux *http.ServeMux) {
	mux.Handle("/api/mcp", &mcp.Server{Engine: a.Engine, Version: Version})
	list := func() []mcp.Status {
		out := []mcp.Status{}
		for _, h := range mcp.Harnesses() {
			out = append(out, mcp.StatusOf(h, a.MCPURL()))
		}
		return out
	}
	payload := func() map[string]any {
		return map[string]any{
			"url":       a.MCPURL(),
			"harnesses": list(),
			"policy":    mcp.LoadPolicy(a.Engine.Store),
		}
	}
	mux.HandleFunc("GET /api/mcp/harnesses", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, payload())
	})
	mux.HandleFunc("POST /api/mcp/harnesses/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := mcp.Install(r.Context(), r.PathValue("id"), a.MCPURL()); err != nil {
			api.WriteErr(w, 400, err.Error())
			return
		}
		api.WriteJSON(w, 200, payload())
	})
	mux.HandleFunc("DELETE /api/mcp/harnesses/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := mcp.Uninstall(r.Context(), r.PathValue("id")); err != nil {
			api.WriteErr(w, 400, err.Error())
			return
		}
		api.WriteJSON(w, 200, payload())
	})
	mux.HandleFunc("GET /api/mcp/policy", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, mcp.LoadPolicy(a.Engine.Store))
	})
	mux.HandleFunc("PUT /api/mcp/policy", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AllowWriteRules    *bool `json:"allow_write_rules"`
			AllowInvokeActions *bool `json:"allow_invoke_actions"`
			AllowShell         *bool `json:"allow_shell"`
		}
		if !api.ReadJSON(w, r, &in) {
			return
		}
		p := mcp.LoadPolicy(a.Engine.Store)
		if in.AllowWriteRules != nil {
			p.AllowWriteRules = *in.AllowWriteRules
		}
		if in.AllowInvokeActions != nil {
			p.AllowInvokeActions = *in.AllowInvokeActions
		}
		if in.AllowShell != nil {
			p.AllowShell = *in.AllowShell
		}
		if err := mcp.SavePolicy(a.Engine.Store, p); err != nil {
			api.WriteErr(w, 500, err.Error())
			return
		}
		api.WriteJSON(w, 200, p)
	})
}

// updateRoutes tell what version is running and whether a newer one is
// there, look for one when asked, and install the one that was downloaded.
func (a *App) updateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/update", func(w http.ResponseWriter, r *http.Request) {
		api.WriteJSON(w, 200, a.Updater.Status())
	})
	mux.HandleFunc("POST /api/update/check", func(w http.ResponseWriter, r *http.Request) {
		// Not the request's context: a download goes on when the page is closed.
		api.WriteJSON(w, 200, a.Updater.Check(context.Background()))
	})
	mux.HandleFunc("POST /api/update/apply", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Updater.Apply(); err != nil {
			api.WriteErr(w, 400, err.Error())
			return
		}
		api.WriteJSON(w, 200, a.Updater.Status())
	})
}

// PluginDir is where installed plugins live.
func (a *App) PluginDir() string { return filepath.Join(a.DataDir, "plugins") }

func (a *App) pluginRoutes(mux *http.ServeMux) {
	ph := a.Plugins
	mux.HandleFunc("POST /api/plugins/rescan", func(w http.ResponseWriter, r *http.Request) {
		var problems []string
		for _, err := range ph.Scan() {
			problems = append(problems, err.Error())
		}
		a.Engine.Publish("plugins", map[string]any{"changed": true})
		api.WriteJSON(w, 200, map[string]any{"plugins": a.Engine.Plugins(), "problems": problems})
	})
	mux.HandleFunc("POST /api/setup", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Plugin     string `json:"plugin"`
			InstanceID string `json:"instance_id"`
			Name       string `json:"name"`
		}
		if !api.ReadJSON(w, r, &body) {
			return
		}
		view, err := ph.StartSetup(body.Plugin, body.InstanceID, body.Name)
		if err != nil {
			api.WriteErr(w, 400, err.Error())
			return
		}
		api.WriteJSON(w, 200, view)
	})
	mux.HandleFunc("GET /api/setup/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := ph.Setup(r.PathValue("id"))
		if !ok {
			api.WriteErr(w, 404, "setup session not found")
			return
		}
		api.WriteJSON(w, 200, view)
	})
	mux.HandleFunc("POST /api/setup/{id}/answer", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Values map[string]any `json:"values"`
			Cancel bool           `json:"cancel"`
		}
		if !api.ReadJSON(w, r, &body) {
			return
		}
		if body.Cancel {
			ph.CancelSetup(r.PathValue("id"))
		} else if err := ph.AnswerSetup(r.PathValue("id"), body.Values); err != nil {
			api.WriteErr(w, 409, err.Error())
			return
		}
		view, _ := ph.Setup(r.PathValue("id"))
		api.WriteJSON(w, 200, view)
	})
}
