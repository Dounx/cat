package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dounx/cat/plugins/internal/watchlist"
	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultInterval = 10 // minutes
	maxBody         = 8 << 20
	maxKeep         = 256 << 10 // bytes of the watched value remembered for the next diff
	changesShown    = 10        // in payload.text
	lineRunes       = 200
	parallel        = 4
	userAgent       = "Cat-APIWatch/0.1 (+https://github.com/dounx/cat)"
)

var methods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// api is one watched endpoint.
type api struct {
	Spec   string // "[METHOD ]URL[ path]": the conversation id and state key
	Name   string // for people: no scheme and no query string, which may hold a key
	Method string
	URL    string
	Path   string
	path   []string // nil: the whole response

	// How it is asked and compared, as said beside it: by the rule that asks
	// for it, or by the instance's settings.
	headers http.Header
	body    string
	ignore  [][]string
	rules   string       // identifies the ignore paths the stored hashes were made with
	key     string       // Spec and the above: the same API asked differently is watched apart
	every   atomic.Int64 // time between checks: the shortest anyone asks for

	mu      sync.Mutex // one request at a time: the poll loop and "check" can overlap
	lastErr string
}

// apiState is what is remembered about an API between checks and restarts.
type apiState struct {
	Hash     string `json:"hash"`
	Value    string `json:"value,omitempty"` // canonical JSON; empty when too large to keep
	Rules    string `json:"rules,omitempty"`
	ETag     string `json:"etag,omitempty"`
	Modified string `json:"modified,omitempty"`
}

type watcher struct {
	// the APIs in the settings and those rules ask for in their triggers
	apis   watchlist.List[*api]
	client *http.Client
	wake   chan struct{} // what is watched has changed
}

func (a *api) interval() time.Duration { return time.Duration(a.every.Load()) }

// parseAPIs reads the endpoints a rule's trigger (or the instance's settings)
// lists under "apis", with what it says beside them about how to ask and
// compare. Each line is "[METHOD] URL [field path]".
func parseAPIs(params map[string]any) ([]*api, error) {
	str := func(k string) string { s, _ := params[k].(string); return strings.TrimSpace(s) }
	headers, body := http.Header{}, str("body")
	for _, line := range strings.Split(str("headers"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if name = strings.TrimSpace(name); !ok || name == "" || strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("请求头格式应为「名称: 值」：%s", line)
		}
		headers.Set(name, strings.TrimSpace(value))
	}
	if auth := str("auth"); auth != "" {
		headers.Set("Authorization", auth)
	}
	var ignore [][]string
	var patterns []string
	for _, line := range strings.Split(str("ignore"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		p, err := parsePath(line, true)
		if err != nil {
			return nil, fmt.Errorf("忽略的字段：%v", err)
		}
		ignore = append(ignore, p)
		patterns = append(patterns, strings.Join(p, "\x00"))
	}
	rules := ""
	if len(patterns) > 0 {
		rules = hash(strings.Join(patterns, "\n"))
	}
	// What tells two ways of asking the same API apart.
	how := rules
	if len(headers) > 0 || body != "" {
		names := make([]string, 0, len(headers))
		for name := range headers {
			names = append(names, name+": "+headers.Get(name))
		}
		sort.Strings(names)
		how = hash(rules + "\x00" + strings.Join(names, "\n") + "\x00" + body)
	}
	minutes := float64(defaultInterval)
	switch v := params["interval"].(type) {
	case float64:
		minutes = v
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			minutes = n
		}
	}
	every := time.Duration(max(minutes, 1) * float64(time.Minute))

	var out []*api
	seen := map[string]bool{}
	for _, line := range strings.Split(str("apis"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		a := &api{Method: "GET"}
		if !strings.Contains(fields[0], "://") && len(fields) > 1 {
			a.Method = strings.ToUpper(fields[0])
			if !methods[a.Method] {
				return nil, fmt.Errorf("不支持的请求方法「%s」：可用 GET、POST、PUT、PATCH、DELETE", fields[0])
			}
			fields = fields[1:]
		}
		a.URL, a.Path = fields[0], strings.Join(fields[1:], "")
		u, err := url.Parse(a.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("看不懂的接口地址「%s」：应填完整的 http(s) 地址", a.URL)
		}
		a.Spec, a.Name = a.URL, u.Host+u.EscapedPath()
		if a.Method != "GET" {
			a.Spec, a.Name = a.Method+" "+a.Spec, a.Method+" "+a.Name
		}
		if a.Path != "" {
			if a.path, err = parsePath(a.Path, false); err != nil {
				return nil, err
			}
			a.Spec, a.Name = a.Spec+" "+a.Path, a.Name+" "+a.Path
		}
		if seen[a.Spec] {
			continue
		}
		seen[a.Spec] = true
		a.headers, a.body, a.ignore, a.rules, a.key = headers, body, ignore, rules, a.Spec
		if how != "" {
			a.key += "\x00" + how
		}
		a.every.Store(int64(every))
		out = append(out, a)
	}
	return out, nil
}

func apiList() watchlist.List[*api] {
	return watchlist.List[*api]{
		Field: "apis", Parse: parseAPIs, Key: func(a *api) string { return a.key },
		// Rules that want an API asked at different paces share one watch of
		// it, at the quickest of them.
		Merge: func(kept *api, wanted []*api) {
			every := wanted[0].interval()
			for _, a := range wanted[1:] {
				every = min(every, a.interval())
			}
			kept.every.Store(int64(every))
		},
	}
}

func newWatcher(cfg map[string]any) (*watcher, error) {
	w := &watcher{apis: apiList(), client: &http.Client{Timeout: 30 * time.Second}, wake: make(chan struct{}, 1)}
	return w, w.apis.Init(cfg)
}

func stateKey(key string) string { return "api:" + hash(key) }

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// fetch calls an API and returns the value to compare. ok is false, without an
// error, when the server says nothing changed since st was recorded.
func (w *watcher) fetch(ctx context.Context, a *api, st *apiState) (value any, ok bool, err error) {
	var body io.Reader
	sendBody := a.Method != "GET" && a.body != ""
	if sendBody {
		body = strings.NewReader(a.body)
	}
	req, err := http.NewRequestWithContext(ctx, a.Method, a.URL, body)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, */*;q=0.8")
	if sendBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, values := range a.headers {
		req.Header[name] = values
	}
	if a.Method == "GET" {
		if st.ETag != "" {
			req.Header.Set("If-None-Match", st.ETag)
		}
		if st.Modified != "" {
			req.Header.Set("If-Modified-Since", st.Modified)
		}
	}
	resp, err := w.client.Do(req)
	if err != nil {
		// The URL may carry a key in its query string: keep it out of logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, false, nil
	}
	// An error response is not a change: it would bury the real value under
	// an error body and report it back as new when the API recovers.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		hint := ""
		switch resp.StatusCode {
		case 401, 403:
			hint = "（接口拒绝访问：检查认证信息）"
		case 404, 410:
			hint = "（接口不存在）"
		case 429, 503:
			hint = "（被限流或暂时不可用）"
		}
		return nil, false, fmt.Errorf("HTTP %d%s", resp.StatusCode, hint)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > maxBody {
		return nil, false, errors.New("返回内容过大")
	}
	value, isJSON := decode(raw)
	if !isJSON && a.path != nil {
		return nil, false, errors.New("返回的不是 JSON，不能使用字段路径")
	}
	for _, p := range a.ignore {
		value = drop(value, p)
	}
	if a.path != nil {
		// Same reasoning as for error statuses: an API that answers 200 with
		// {"error": …} must not look like the field went away.
		if value, ok = lookup(value, a.path); !ok {
			return nil, false, fmt.Errorf("返回里没有字段 %s", a.Path)
		}
	}
	st.ETag, st.Modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return value, true, nil
}

// check calls one API and emits an event if its value changed since last time.
func (w *watcher) check(ctx context.Context, h *cat.Host, a *api) (changed bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	var st apiState
	raw, known := h.StateGet(stateKey(a.key))
	if known && json.Unmarshal([]byte(raw), &st) != nil {
		known = false
	}
	// Hashes made with other ignore paths can't be compared: start over.
	if known && st.Rules != a.rules {
		known = false
	}
	if !known {
		st = apiState{}
	}
	value, ok, err := w.fetch(ctx, a, &st)
	if err != nil {
		return false, err
	}
	if !ok { // not modified
		return false, nil
	}
	text := canonical(value)
	sum := hash(text)
	if known && sum != st.Hash {
		var old any
		haveOld := false
		if st.Value != "" {
			old, haveOld = decode([]byte(st.Value))
		}
		if _, err := h.Emit(w.event(a, value, old, haveOld, sum)); err != nil {
			// State is left alone, so the next check finds the change again.
			return false, fmt.Errorf("发出事件失败：%v", err)
		}
		changed = true
	}
	st.Hash, st.Rules, st.Value = sum, a.rules, ""
	if len(text) <= maxKeep {
		st.Value = text
	}
	if data, err := json.Marshal(st); err == nil {
		if err := h.StateSet(stateKey(a.key), string(data)); err != nil {
			h.Log("error", "%s：保存状态失败：%v", a.Name, err)
		}
	}
	if !known {
		h.Log("info", "开始监控 %s", a.Name)
	}
	return changed, nil
}

// event describes a change. Without the old value (too large to keep) there
// is no list of changed fields to give.
func (w *watcher) event(a *api, value, old any, haveOld bool, sum string) cat.Event {
	payload := map[string]any{
		"message_id": hash(a.Spec) + "-" + sum,
		"url":        a.URL,
		"method":     a.Method,
		"path":       a.Path,
		"name":       a.Name,
		"value":      value,
		"diff":       haveOld,
	}
	parts := []string{"接口返回有变动：" + a.Name}
	if haveOld {
		changes := []change{}
		diff(a.Path, old, value, &changes)
		payload["previous"], payload["changes"] = old, changes
		var b strings.Builder
		for i, c := range changes {
			if i == changesShown {
				fmt.Fprintf(&b, "… 还有 %d 处", len(changes)-i)
				break
			}
			b.WriteString(describe(c) + "\n")
		}
		if s := strings.TrimRight(b.String(), "\n"); s != "" {
			parts = append(parts, s)
		}
	}
	payload["text"] = strings.Join(parts, "\n\n")
	return cat.Event{
		Type:         "api.changed",
		Conversation: &cat.Conversation{ID: a.Spec, Kind: "channel", Name: a.Name},
		Payload:      payload,
		Triggers:     w.apis.AskedBy(a.key), // the rules that asked for this API take it as theirs
	}
}

// describe formats one change as a line of a chat message.
func describe(c change) string {
	var line string
	switch c.Type {
	case "added":
		line = "+ " + c.Path + "：" + canonical(c.New)
	case "removed":
		line = "- " + c.Path + "：" + canonical(c.Old)
	default:
		line = canonical(c.Old) + " → " + canonical(c.New)
		if c.Path != "" {
			line = c.Path + "：" + line
		}
		line = "~ " + line
	}
	if r := []rune(line); len(r) > lineRunes {
		line = string(r[:lineRunes]) + "…"
	}
	return line
}

type result struct {
	Checked, Changed int
	Errors           []string // "name：reason", in configuration order
}

// round checks the given APIs, a few at a time.
func (w *watcher) round(ctx context.Context, h *cat.Host, apis []*api) result {
	errs := make([]string, len(apis))
	changed := make([]bool, len(apis))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, a := range apis {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ok, err := w.check(ctx, h, a)
			changed[i] = ok
			msg := ""
			if err != nil && ctx.Err() == nil {
				msg = err.Error()
				errs[i] = a.Name + "：" + msg
			}
			// Log transitions only, so an API that stays down doesn't flood the log.
			a.mu.Lock()
			flipped := msg != a.lastErr && ctx.Err() == nil
			if flipped {
				a.lastErr = msg
			}
			a.mu.Unlock()
			if flipped && msg != "" {
				h.Log("warn", "%s 检查失败：%s", a.Name, msg)
			} else if flipped {
				h.Log("info", "%s 已恢复", a.Name)
			}
		}()
	}
	wg.Wait()
	res := result{Checked: len(apis)}
	for i := range apis {
		if changed[i] {
			res.Changed++
		}
		if errs[i] != "" {
			res.Errors = append(res.Errors, errs[i])
		}
	}
	return res
}

// poll asks each API as often as it is asked to, starting with a call at
// once: on start, and to an API a rule has just asked for.
func (w *watcher) poll(ctx context.Context, h *cat.Host) {
	status := ""
	last := map[*api]time.Time{}
	for {
		apis := w.apis.Items()
		now := time.Now()
		var due []*api
		seen := make(map[*api]time.Time, len(apis))
		for _, a := range apis {
			if t, ok := last[a]; ok && now.Sub(t) < a.interval() {
				seen[a] = t
				continue
			}
			seen[a] = now
			due = append(due, a)
		}
		last = seen // APIs no longer watched are forgotten
		if len(due) > 0 {
			w.round(ctx, h, due)
		}
		if ctx.Err() != nil {
			return
		}
		// The instance is only "in error" when nothing works; a single broken
		// API is reported in the log and shouldn't hide the healthy ones.
		next, failing := "", 0
		for _, a := range apis {
			a.mu.Lock()
			if a.lastErr != "" {
				if failing++; next == "" {
					next = "接口全部检查失败，将继续重试：" + a.Name + "：" + a.lastErr
				}
			}
			a.mu.Unlock()
		}
		if failing < len(apis) {
			next = ""
		}
		if next != status {
			if next != "" {
				h.SetStatus("error", next)
			} else {
				h.SetStatus("running", "")
			}
			status = next
		}
		wait := time.Hour
		for _, a := range apis {
			wait = min(wait, time.Until(last[a].Add(a.interval())))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-time.After(max(wait, time.Second)):
		}
	}
}

// New builds the plugin. What it watches, and how, is said in the trigger
// nodes of rules; an instance with settings of its own may list APIs there
// too, under the same names.
func New() cat.Plugin {
	var (
		mu sync.Mutex
		w  *watcher // set once the instance has started
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			nw, err := newWatcher(h.Config)
			if err != nil {
				return err
			}
			mu.Lock()
			w = nw
			nw.apis.Ask(h.Triggers()) // under mu: a change of triggers comes after, not in between
			mu.Unlock()
			if n := len(nw.apis.Items()); n > 0 {
				h.Log("info", "监控 %d 个接口", n)
			} else {
				h.Log("info", "还没有要监控的接口：在规则的触发节点里选「接口变动」后填写")
			}
			go nw.poll(ctx, h)
			return nil
		},
		// The rules changed what they ask for. Before the instance has started
		// there is only the reading of it to check; Start picks up the rest.
		Triggers: func(_ context.Context, h *cat.Host, list []cat.Trigger) map[string]string {
			mu.Lock()
			defer mu.Unlock()
			if w == nil {
				probe := apiList()
				if probe.Init(h.Config) != nil {
					return nil // Start says so
				}
				return probe.Check(list)
			}
			before := len(w.apis.Items())
			added, errs := w.apis.Ask(list)
			if n := len(w.apis.Items()); len(added) > 0 || n != before {
				h.Log("info", "规则要监控的接口有变化：现在监控 %d 个", n)
			}
			select { // a new API is asked now, and a new pace is kept from now
			case w.wake <- struct{}{}:
			default:
			}
			return errs
		},
		Actions: map[string]cat.ActionFunc{
			"check": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				mu.Lock()
				w := w
				mu.Unlock()
				if w == nil {
					return nil, errors.New("实例尚未启动")
				}
				apis := w.apis.Items()
				if want, _ := p["api"].(string); strings.TrimSpace(want) != "" {
					want = strings.Join(strings.Fields(want), " ")
					all := apis
					apis = nil
					for _, a := range all {
						if a.Spec == want || a.URL == want {
							apis = append(apis, a)
						}
					}
					if apis == nil {
						return nil, fmt.Errorf("没有监控这个接口：%s", want)
					}
				}
				res := w.round(ctx, h, apis)
				out := map[string]any{"checked": res.Checked, "changed": res.Changed, "failed": len(res.Errors), "errors": res.Errors}
				if res.Errors == nil {
					out["errors"] = []string{}
				}
				if res.Checked > 0 && len(res.Errors) == res.Checked {
					return out, errors.New(strings.Join(res.Errors, "；"))
				}
				return out, nil
			},
		},
	}
}
