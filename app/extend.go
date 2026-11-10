package app

import (
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/dounx/cat/core/api"
	"github.com/dounx/cat/core/pluginhost"
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
	return nil
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
