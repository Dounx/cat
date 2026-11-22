// Package api exposes the engine over HTTP: a JSON API, a server-sent event
// stream, the ingress endpoint for inbound webhooks and the embedded web UI.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dounx/cat/core/blob"
	"github.com/dounx/cat/core/builtin"
	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

const maxIngressBody = 4 << 20

type Server struct {
	Engine *engine.Engine
	// Token, when set, is required for every /api request (Bearer header or
	// the cat_token cookie set by POST /api/login). Ingress is never gated by
	// it; instances authenticate their own callers.
	Token string
	// UI is the built web frontend; nil serves the API only.
	UI fs.FS
	// UIDev, when set, replaces UI: everything that is not the API or ingress
	// is proxied to the frontend dev server, so its hot reload works at the
	// hub's own address.
	UIDev *url.URL
	// Desktop is reported by /api/status so the UI can adapt to the desktop shell.
	Desktop bool

	mux    *http.ServeMux
	extras []func(mux *http.ServeMux)
}

// Extend registers additional API routes (used by optional subsystems).
func (s *Server) Extend(fn func(mux *http.ServeMux)) { s.extras = append(s.extras, fn) }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.mux = mux
	e := s.Engine

	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "auth": s.Token != "", "desktop": s.Desktop})
	})
	mux.HandleFunc("GET /api/stream", s.stream)

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		public, _ := e.Store.KVGet(settingsScope, "public_url")
		mkt, _ := e.Store.KVGet(settingsScope, "market_url")
		writeJSON(w, 200, Settings{PublicURL: public, MarketURL: mkt})
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			PublicURL *string `json:"public_url"`
			MarketURL *string `json:"market_url"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if in.PublicURL != nil {
			public, err := normalizePublicURL(*in.PublicURL)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			if public == "" {
				err = e.Store.KVDelete(settingsScope, "public_url")
			} else {
				err = e.Store.KVSet(settingsScope, "public_url", public)
			}
			if err != nil {
				respond(w, nil, err)
				return
			}
		}
		if in.MarketURL != nil {
			mkt, err := normalizeMarketURL(*in.MarketURL)
			if err != nil {
				writeErr(w, 400, err.Error())
				return
			}
			if mkt == "" {
				err = e.Store.KVDelete(settingsScope, "market_url")
			} else {
				err = e.Store.KVSet(settingsScope, "market_url", mkt)
			}
			if err != nil {
				respond(w, nil, err)
				return
			}
		}
		public, _ := e.Store.KVGet(settingsScope, "public_url")
		mkt, _ := e.Store.KVGet(settingsScope, "market_url")
		writeJSON(w, 200, Settings{PublicURL: public, MarketURL: mkt})
	})

	mux.HandleFunc("GET /api/plugins", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, e.Plugins())
	})

	mux.HandleFunc("GET /api/instances", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, e.Instances())
	})
	mux.HandleFunc("POST /api/instances", func(w http.ResponseWriter, r *http.Request) {
		var rec store.Instance
		if !readJSON(w, r, &rec) {
			return
		}
		rec.ID = ""
		saved, err := e.SaveInstance(rec)
		respond(w, saved, err)
	})
	mux.HandleFunc("PUT /api/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		var rec store.Instance
		if !readJSON(w, r, &rec) {
			return
		}
		cur, ok := e.Instance(r.PathValue("id"))
		if !ok {
			writeErr(w, 404, "instance not found")
			return
		}
		rec.ID, rec.Plugin = cur.ID, cur.Plugin
		saved, err := e.SaveInstance(rec)
		respond(w, saved, err)
	})
	mux.HandleFunc("DELETE /api/instances/{id}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]bool{"ok": true}, e.DeleteInstance(r.PathValue("id")))
	})
	mux.HandleFunc("GET /api/instances/{id}/options", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		opts, err := e.Options(ctx, r.PathValue("id"), r.URL.Query().Get("action"), r.URL.Query().Get("field"))
		if opts == nil {
			opts = []plugin.Option{}
		}
		respond(w, opts, err)
	})
	mux.HandleFunc("POST /api/instances/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		cur, ok := e.Instance(r.PathValue("id"))
		if !ok {
			writeErr(w, 404, "instance not found")
			return
		}
		saved, err := e.SaveInstance(cur.Instance)
		respond(w, saved, err)
	})
	mux.HandleFunc("GET /api/instances/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := e.Instance(id); !ok {
			writeErr(w, 404, "instance not found")
			return
		}
		writeJSON(w, 200, e.InstanceLogs(id))
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		failed := q.Get("failed") == "1" || strings.EqualFold(q.Get("failed"), "true")
		withRuns := q.Get("with_runs") == "1" || strings.EqualFold(q.Get("with_runs"), "true")
		f := store.EventFilter{
			Source:     q.Get("source"),
			Type:       q.Get("type"),
			Before:     q.Get("before"),
			RuleID:     q.Get("rule_id"),
			OnlyFailed: failed,
			Query:      q.Get("q"),
			Limit:      limit,
		}
		if !withRuns {
			list, err := e.Store.ListEvents(f)
			respond(w, list, err)
			return
		}
		pageSize := limit
		if pageSize <= 0 || pageSize > 200 {
			pageSize = 80
		}
		f.Limit = pageSize + 1
		list, err := e.Store.ListEvents(f)
		if err != nil {
			respond(w, nil, err)
			return
		}
		hasMore := len(list) > pageSize
		if hasMore {
			list = list[:pageSize]
		}
		ids := make([]string, len(list))
		for i := range list {
			ids[i] = list[i].ID
		}
		runs, err := e.Store.ListRunsForEvents(ids)
		if err != nil {
			respond(w, nil, err)
			return
		}
		for i := range runs {
			runs[i].Rule = nil
		}
		writeJSON(w, 200, map[string]any{"events": list, "runs": runs, "has_more": hasMore})
	})
	mux.HandleFunc("GET /api/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		ev, err := e.Store.GetEvent(r.PathValue("id"))
		respond(w, ev, err)
	})
	mux.HandleFunc("POST /api/events/{id}/replay", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RuleID string `json:"rule_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n, err := e.Replay(r.PathValue("id"), body.RuleID)
		respond(w, map[string]int{"matched": n}, err)
	})

	mux.HandleFunc("GET /api/rules", func(w http.ResponseWriter, r *http.Request) {
		list, err := e.Rules()
		respond(w, list, err)
	})
	mux.HandleFunc("POST /api/rules", func(w http.ResponseWriter, r *http.Request) {
		var rule rules.Rule
		if !readJSON(w, r, &rule) {
			return
		}
		rule.ID = ""
		saved, err := e.SaveRule(rule)
		respondBad(w, saved, err)
	})
	mux.HandleFunc("PUT /api/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		var rule rules.Rule
		if !readJSON(w, r, &rule) {
			return
		}
		rule.ID = r.PathValue("id")
		saved, err := e.SaveRule(rule)
		respondBad(w, saved, err)
	})
	mux.HandleFunc("DELETE /api/rules/{id}", func(w http.ResponseWriter, r *http.Request) {
		respond(w, map[string]bool{"ok": true}, e.DeleteRule(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/rules/test", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Condition string `json:"condition"`
			EventID   string `json:"event_id"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		ok, err := e.TestCondition(body.Condition, body.EventID)
		if err != nil {
			writeJSON(w, 200, map[string]any{"matched": false, "error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"matched": ok})
	})

	mux.HandleFunc("POST /api/rules/try", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Rule    rules.Rule `json:"rule"`
			EventID string     `json:"event_id"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		run, err := e.TryRule(body.Rule, body.EventID)
		if err != nil {
			writeJSON(w, 200, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"run": run})
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		list, err := e.Store.ListRuns(q.Get("rule_id"), q.Get("event_id"), limit)
		// the rule each ran as comes with a single run only
		for i := range list {
			list[i].Rule = nil
		}
		respond(w, list, err)
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		run, err := e.Store.GetRun(r.PathValue("id"))
		respond(w, run, err)
	})
	mux.HandleFunc("POST /api/runs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if !e.CancelRun(r.PathValue("id")) {
			writeErr(w, 404, "这次运行已经结束或不存在")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/approvals", func(w http.ResponseWriter, r *http.Request) {
		list := e.ListApprovals()
		if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
			filtered := make([]builtin.ApprovalRequest, 0, len(list))
			for _, item := range list {
				if string(item.Status) == status {
					filtered = append(filtered, item)
				}
			}
			list = filtered
		}
		writeJSON(w, 200, list)
	})
	mux.HandleFunc("POST /api/approvals/{id}/decide", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Decision string `json:"decision"`
			Comment  string `json:"comment"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		req, err := e.DecideApproval(r.PathValue("id"), body.Decision, body.Comment)
		if err != nil {
			switch {
			case errors.Is(err, builtin.ErrInvalidDecision):
				writeErr(w, 400, err.Error())
			case errors.Is(err, builtin.ErrApprovalNotFound):
				writeErr(w, 404, err.Error())
			case errors.Is(err, builtin.ErrApprovalAlreadyDecided), errors.Is(err, plugin.ErrGone):
				writeErr(w, 409, err.Error())
			default:
				writeErr(w, 409, err.Error())
			}
			return
		}
		writeJSON(w, 200, req)
	})
	mux.HandleFunc("GET /api/blobs/{rel...}", s.serveBlob)

	for _, fn := range s.extras {
		fn(mux)
	}

	mux.HandleFunc("/ingress/{id}", s.ingress)
	mux.HandleFunc("/ingress/{id}/{rest...}", s.ingress)

	if s.UIDev != nil {
		mux.Handle("/", devUI(s.UIDev))
	} else if s.UI != nil {
		mux.Handle("/", spa(s.UI))
	}
	return s.guard(mux)
}

const settingsScope = "settings"

// Settings are what the hub is told about itself, as opposed to about a
// connection.
type Settings struct {
	// PublicURL is where the hub is reached from outside, when that is not
	// where the UI is opened: the address a reverse proxy or a tunnel gives
	// it. The UI shows ingress addresses under it. Empty means not set.
	PublicURL string `json:"public_url"`
	// MarketURL overrides the plugin market index URL ("off" disables it).
	MarketURL string `json:"market_url"`
}

// normalizePublicURL checks an address typed in by hand and returns it as it
// is kept: scheme and host, with any path it is served under, and no trailing
// slash. Empty stays empty.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("对外地址要以 http:// 或 https:// 开头，后面跟域名或 IP")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", errors.New("对外地址里不要带 ?、# 或用户名密码")
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

func normalizeMarketURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if strings.EqualFold(raw, "off") {
		return "off", nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("插件市场地址格式不对")
	}
	if u.Scheme == "file" && u.Path != "" {
		return raw, nil
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil {
		return raw, nil
	}
	return "", errors.New("插件市场地址要以 http://、https:// 或 file:// 开头，或者填 off 关闭")
}

// guard enforces the API token and rejects cross-site API calls.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// A page on another origin must not be able to drive the local API.
			if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
				writeErr(w, 403, "cross-site request refused")
				return
			}
			open := r.URL.Path == "/api/login" || r.URL.Path == "/api/status"
			if s.Token != "" && !open && !s.authed(r) {
				writeErr(w, 401, "unauthorized")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) authed(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if c, err := r.Cookie("cat_token"); err == nil && got == "" {
		got = c.Value
	}
	// An MCP client is given one address and nothing else, so for that
	// endpoint the token may ride in it.
	if got == "" && r.URL.Path == "/api/mcp" {
		got = r.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if s.Token != "" && subtle.ConstantTimeCompare([]byte(body.Token), []byte(s.Token)) != 1 {
		writeErr(w, 401, "令牌不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "cat_token", Value: body.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400 * 365})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, cancel := s.Engine.Subscribe()
	defer cancel()
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case m := <-ch:
			b, err := json.Marshal(m.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.Kind, b)
			fl.Flush()
		}
	}
}

func (s *Server) ingress(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIngressBody))
	if err != nil {
		writeErr(w, 413, "body too large")
		return
	}
	headers := map[string]string{}
	for k, v := range r.Header {
		headers[k] = strings.Join(v, ", ")
	}
	resp, err := s.Engine.Ingress(r.Context(), r.PathValue("id"), plugin.IngressRequest{
		Method: r.Method, Path: "/" + r.PathValue("rest"), Query: r.URL.Query(), Headers: headers, Body: body,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for k, v := range resp.Headers {
		w.Header().Set(k, v)
	}
	if resp.Status == 0 {
		resp.Status = 200
	}
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	path, err := blob.Resolve(s.Engine.BlobsDir(), r.PathValue("rel"))
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, 404, "not found")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeErr(w, 404, "not found")
		return
	}
	var head [512]byte
	n, _ := f.Read(head[:])
	_, _ = f.Seek(0, io.SeekStart)
	ct := mime.TypeByExtension(filepath.Ext(path))
	if ct == "" {
		ct = http.DetectContentType(head[:n])
	}
	baseCT, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(ct)), ";")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; sandbox")
	w.Header().Set("Content-Type", ct)
	switch baseCT {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		w.Header().Set("Content-Disposition", "inline")
	default:
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(path)}))
	}
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

// spa serves static files and falls back to index.html for client routes.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(ui, p); err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				writeErr(w, 404, "not found")
				return
			}
			http.ServeFileFS(w, r, ui, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}

// devUI proxies to the frontend dev server, WebSocket upgrades included (the
// hot-reload channel).
func devUI(target *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(target) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "frontend dev server at "+target.String()+" is not reachable: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeErr(w, 404, "not found")
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// WriteJSON and WriteErr are exported for subsystems that add routes.
func WriteJSON(w http.ResponseWriter, status int, v any)     { writeJSON(w, status, v) }
func WriteErr(w http.ResponseWriter, status int, msg string) { writeErr(w, status, msg) }

// ReadJSON decodes the request body, replying 400 and returning false on failure.
func ReadJSON(w http.ResponseWriter, r *http.Request, v any) bool { return readJSON(w, r, v) }

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(v); err != nil {
		writeErr(w, 400, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any, err error) {
	switch {
	case err == nil:
		writeJSON(w, 200, v)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, 500, err.Error())
	}
}

// respondBad reports errors as 400: the caller supplied something invalid.
func respondBad(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
