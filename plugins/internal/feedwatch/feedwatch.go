// Package feedwatch is the engine behind the RSS and RSSHub plugins: it polls
// feeds, remembers which items it has seen and emits an event for each new
// one. The two plugins differ only in what a subscription is — a feed URL, or
// a route on an RSSHub instance.
package feedwatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultHub      = "https://rsshub.app"
	defaultInterval = 10 // minutes
	maxBody         = 16 << 20
	maxSeen         = 2000 // remembered item keys per feed
	summaryRunes    = 500
	parallel        = 4
	userAgent       = "Cat-RSS/0.1 (+https://github.com/dounx/cat)"
)

// sub is one subscribed feed.
type sub struct {
	Spec string // as the user wrote it (normalised); also the conversation id
	URL  string // what is actually fetched; may carry the access key
	Hub  bool   // an RSSHub route rather than a plain feed URL

	// How it is watched, as the rules that ask for it (or the instance's
	// settings) say: the shortest time between checks anyone asks for, and
	// whether anyone wants the items already there the first time.
	every    atomic.Int64
	existing atomic.Bool

	mu      sync.Mutex // one fetch at a time: the poll loop and "check" can overlap
	lastErr string
}

// subState is what is remembered about a feed between polls and restarts.
type subState struct {
	Seen     []string `json:"seen"` // hashed item keys, newest first
	ETag     string   `json:"etag,omitempty"`
	Modified string   `json:"modified,omitempty"`
}

// watcher polls the subscriptions: those in the instance's own settings and
// those the rules ask for in their trigger nodes.
type watcher struct {
	kind   Kind
	hub    string         // RSSHub: where the routes are fetched from
	base   map[string]any // the settings: what a rule leaves unsaid is as here
	secret string         // RSSHub access key, kept out of logs
	client *http.Client
	wake   chan struct{} // what is watched has changed

	mu    sync.Mutex
	fixed []string            // from the settings
	subs  []*sub              // everything watched, the settings' first
	asked map[string][]string // spec → the triggers that ask for it
}

func isURL(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// route normalises an RSSHub route: "rsshub://a/b" and "/a/b" are the same.
// It reports false for anything that isn't one.
func route(s string) (string, bool) {
	if strings.HasPrefix(strings.ToLower(s), "rsshub://") {
		return "/" + strings.TrimLeft(s[len("rsshub://"):], "/"), true
	}
	return s, strings.HasPrefix(s, "/")
}

// parseSpec reads one subscription as a person writes it: a feed's address
// for RSS, a route for RSSHub. Each kind points at the other's plugin.
func parseSpec(kind Kind, line string) (string, error) {
	spec, isRoute := route(line)
	if kind == RSSHub {
		switch {
		case isURL(line):
			return "", fmt.Errorf("「%s」是完整地址，不是路由：普通 RSS / Atom 源请用 RSS 插件订阅", line)
		case !isRoute:
			return "", fmt.Errorf("看不懂的路由「%s」：路由以 / 开头，如 /github/issue/DIYgod/RSSHub", line)
		}
		if _, err := url.Parse("http://hub" + spec); err != nil {
			return "", fmt.Errorf("路由无效：%s", line)
		}
		return spec, nil
	}
	switch {
	case isRoute:
		return "", fmt.Errorf("「%s」是 RSSHub 路由，请用 RSSHub 插件订阅", line)
	case !isURL(line):
		return "", fmt.Errorf("看不懂的订阅源「%s」：请填完整的 http(s) 地址", line)
	}
	if u, err := url.Parse(line); err != nil || u.Host == "" {
		return "", fmt.Errorf("地址无效：%s", line)
	}
	return line, nil
}

// parseSpecs reads a multi-line list of subscriptions, dropping blanks,
// comments and repeats.
func parseSpecs(kind Kind, text string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		spec, err := parseSpec(kind, line)
		if err != nil {
			return nil, err
		}
		if !seen[spec] {
			seen[spec] = true
			out = append(out, spec)
		}
	}
	return out, nil
}

// field is the setting, and the trigger parameter, that lists subscriptions.
func (k Kind) field() string {
	if k == RSSHub {
		return "routes"
	}
	return "feeds"
}

func (k Kind) noun() string {
	if k == RSSHub {
		return "路由"
	}
	return "订阅源"
}

// how is the way a subscription is watched.
type how struct {
	every    time.Duration
	existing bool
}

// join settles two ways the same subscription is wanted: it is checked as
// often as the more impatient asks, and what is already there is announced
// if either wants it.
func (a how) join(b how) how {
	return how{every: min(a.every, b.every), existing: a.existing || b.existing}
}

// settings reads how to watch from a trigger's parameters, falling back on
// the instance's settings (base) for what it leaves unsaid.
func settings(params, base map[string]any) how {
	get := func(k string) any {
		if v, ok := params[k]; ok {
			return v
		}
		return base[k]
	}
	minutes := float64(defaultInterval)
	switch v := get("interval").(type) {
	case float64:
		minutes = v
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			minutes = n
		}
	}
	return how{every: time.Duration(max(minutes, 1) * float64(time.Minute)), existing: get("emit_existing") == true}
}

// asked reads what the rules' triggers ask for: for each subscription, the
// triggers that want it and how, and for each trigger that cannot be read,
// why.
func (k Kind) asked(base map[string]any, list []cat.Trigger) (specs []string, by map[string][]string, ways map[string]how, errs map[string]string) {
	by, ways, errs = map[string][]string{}, map[string]how{}, map[string]string{}
	for _, t := range list {
		text, _ := t.Params[k.field()].(string)
		found, err := parseSpecs(k, text)
		if err != nil {
			errs[t.ID] = err.Error()
			continue
		}
		way := settings(t.Params, base)
		for _, spec := range found {
			if by[spec] == nil {
				specs = append(specs, spec)
				ways[spec] = way
			}
			ways[spec] = ways[spec].join(way)
			by[spec] = append(by[spec], t.ID)
		}
	}
	return specs, by, ways, errs
}

func newWatcher(kind Kind, cfg map[string]any) (*watcher, error) {
	str := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }
	w := &watcher{kind: kind, base: cfg, client: &http.Client{Timeout: 30 * time.Second}, wake: make(chan struct{}, 1)}
	if kind == RSSHub {
		w.secret = str("access_key")
		w.hub = strings.TrimRight(str("base_url"), "/")
		if w.hub == "" {
			w.hub = defaultHub
		}
		if u, err := url.Parse(w.hub); err != nil || !isURL(w.hub) || u.Host == "" {
			return nil, errors.New("RSSHub 实例地址无效，应以 http:// 或 https:// 开头")
		}
	}
	var err error
	if w.fixed, err = parseSpecs(kind, str(kind.field())); err != nil {
		return nil, err
	}
	w.watch(nil)
	return w, nil
}

// newSub works out what to fetch for a subscription.
func (w *watcher) newSub(spec string) *sub {
	if w.kind != RSSHub {
		return &sub{Spec: spec, URL: spec}
	}
	s := &sub{Spec: spec, URL: w.hub + spec, Hub: true}
	if u, err := url.Parse(s.URL); err == nil && w.secret != "" {
		q := u.Query()
		q.Set("key", w.secret)
		u.RawQuery = q.Encode()
		s.URL = u.String()
	}
	return s
}

// watch sets what the rules ask for beside the settings' own subscriptions.
// It returns the subscriptions that are new, and what is wrong with the
// triggers that could not be read.
func (w *watcher) watch(list []cat.Trigger) (added []*sub, errs map[string]string) {
	specs, by, ways, errs := w.kind.asked(w.base, list)
	for _, spec := range w.fixed { // those in the settings go by the settings
		way := settings(nil, w.base)
		if other, ok := ways[spec]; ok {
			way = way.join(other)
		}
		ways[spec] = way
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	have := map[string]*sub{}
	for _, s := range w.subs {
		have[s.Spec] = s
	}
	var subs []*sub
	for _, spec := range append(slices.Clone(w.fixed), specs...) {
		s := have[spec]
		if s == nil {
			s = w.newSub(spec)
			added = append(added, s)
		} else if slices.Contains(subs, s) {
			continue // in the settings and asked for by a rule
		}
		have[spec] = s
		s.every.Store(int64(ways[spec].every))
		s.existing.Store(ways[spec].existing)
		subs = append(subs, s)
	}
	w.subs, w.asked = subs, by
	return added, errs
}

// watched lists the subscriptions as they are now.
func (w *watcher) watched() []*sub {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.subs)
}

// askedBy lists the triggers that ask for a subscription.
func (w *watcher) askedBy(spec string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.asked[spec])
}

func stateKey(spec string) string { return "feed:" + hash(spec) }

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// itemKey identifies an item across polls.
func itemKey(it item) string {
	switch {
	case it.ID != "":
		return hash(it.ID)
	case it.Link != "":
		return hash(it.Link)
	}
	return hash(it.Title + "\x00" + it.RawDate)
}

// scrub keeps the access key (part of the URL) out of logs and the UI.
func (w *watcher) scrub(err error) error {
	if err == nil || w.secret == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), url.QueryEscape(w.secret), "<key>")
	return errors.New(strings.ReplaceAll(msg, w.secret, "<key>"))
}

// fetch downloads and parses a feed. It returns nil without an error when the
// server says nothing changed since st was recorded.
func (w *watcher) fetch(ctx context.Context, s *sub, st *subState) (*feed, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if st.ETag != "" {
		req.Header.Set("If-None-Match", st.ETag)
	}
	if st.Modified != "" {
		req.Header.Set("If-Modified-Since", st.Modified)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		page, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		reason := ""
		if s.Hub {
			reason = hubError(page)
		}
		hint := ""
		switch {
		case s.Hub && (resp.StatusCode == 401 || resp.StatusCode == 403 || strings.Contains(reason, "Authentication failed")):
			hint = "（RSSHub 拒绝访问：检查访问密钥，或换一个实例）"
		case s.Hub && (resp.StatusCode == 404 || strings.Contains(reason, "NotFoundError")):
			hint = "（路由不存在，或这个实例没有启用它）"
		case reason != "":
			hint = "（RSSHub：" + reason + "）"
		case resp.StatusCode == 429 || resp.StatusCode == 503:
			hint = "（被限流或暂时不可用）"
		}
		return nil, fmt.Errorf("HTTP %d%s", resp.StatusCode, hint)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("订阅源内容过大")
	}
	f, err := parseFeed(body, resp.Request.URL.String())
	if err != nil {
		return nil, err
	}
	st.ETag, st.Modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return f, nil
}

// hubError pulls the reason out of RSSHub's HTML error page. RSSHub answers
// 503 for most failures (a wrong access key, an upstream site refusing it),
// so the status code alone says little.
func hubError(page []byte) string {
	text := html.UnescapeString(oneLine(reTag.ReplaceAllString(string(page), " ")))
	m := reHubError.FindStringSubmatch(text)
	if m == nil {
		return ""
	}
	return truncate(m[1], 200)
}

var reHubError = regexp.MustCompile(`Error Message:\s*(.+?)\s+Route:`)

// check fetches one feed and emits its new items, oldest first. It returns
// how many were emitted.
func (w *watcher) check(ctx context.Context, h *cat.Host, s *sub) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var st subState
	raw, known := h.StateGet(stateKey(s.Spec))
	if known && json.Unmarshal([]byte(raw), &st) != nil {
		st, known = subState{}, false
	}
	f, err := w.fetch(ctx, s, &st)
	if err != nil {
		return 0, w.scrub(err)
	}
	if f == nil { // not modified
		return 0, nil
	}

	old := make(map[string]bool, len(st.Seen))
	for _, k := range st.Seen {
		old[k] = true
	}
	// A feed seen for the first time only sets the baseline, unless asked otherwise.
	announce := known || s.existing.Load()
	current := make([]string, 0, len(f.Items))
	inDoc := map[string]bool{}
	var fresh []item
	for _, it := range f.Items {
		k := itemKey(it)
		if inDoc[k] {
			continue
		}
		inDoc[k] = true
		current = append(current, k)
		if !old[k] && announce {
			fresh = append(fresh, it)
		}
	}

	emitted, failed := 0, map[string]bool{}
	for i := len(fresh) - 1; i >= 0; i-- { // feeds list newest first
		if _, err := h.Emit(w.event(s, f, fresh[i])); err != nil {
			h.Log("error", "%s：发出事件失败：%v", s.Spec, err)
			failed[itemKey(fresh[i])] = true
			continue
		}
		emitted++
	}

	seen := make([]string, 0, len(current)+len(st.Seen))
	for _, k := range current {
		if !failed[k] {
			seen = append(seen, k)
		}
	}
	for _, k := range st.Seen {
		if !inDoc[k] {
			seen = append(seen, k)
		}
	}
	if len(seen) > maxSeen && len(seen) > 2*len(current) {
		seen = seen[:max(maxSeen, 2*len(current))]
	}
	st.Seen = seen
	if len(failed) > 0 { // make sure the next poll downloads the feed again
		st.ETag, st.Modified = "", ""
	}
	if data, err := json.Marshal(st); err == nil {
		if err := h.StateSet(stateKey(s.Spec), string(data)); err != nil {
			h.Log("error", "%s：保存状态失败：%v", s.Spec, err)
		}
	}
	if !known {
		h.Log("info", "已订阅 %s（%s，现有 %d 条）", s.Spec, f.Title, len(current))
	}
	return emitted, nil
}

func (w *watcher) event(s *sub, f *feed, it item) cat.Event {
	content := htmlToText(it.HTML)
	summary := truncate(content, summaryRunes)
	parts := make([]string, 0, 3)
	if it.Title != "" {
		parts = append(parts, it.Title)
	}
	if summary != "" && summary != it.Title {
		parts = append(parts, summary)
	}
	if it.Link != "" {
		parts = append(parts, it.Link)
	}
	id := it.ID
	if id == "" {
		id = it.Link
	}
	if id == "" {
		id = itemKey(it)
	}
	payload := map[string]any{
		"text":         strings.Join(parts, "\n\n"),
		"message_id":   id,
		"title":        it.Title,
		"link":         it.Link,
		"summary":      summary,
		"content":      content,
		"content_html": it.HTML,
		"feed":         s.Spec,
		"feed_title":   f.Title,
		"feed_link":    f.Link,
	}
	if it.Author != "" {
		payload["author"] = it.Author
	}
	if len(it.Categories) > 0 {
		payload["categories"] = it.Categories
	}
	if imgs := images(it.HTML); len(imgs) > 0 {
		payload["images"] = imgs
	}
	if len(it.Enclosures) > 0 {
		payload["enclosures"] = it.Enclosures
	}
	if !it.Published.IsZero() {
		payload["published"] = it.Published.UTC().Format(time.RFC3339)
	} else if it.RawDate != "" {
		payload["published"] = it.RawDate
	}
	name := f.Title
	if name == "" {
		name = s.Spec
	}
	ev := cat.Event{
		Type:         "feed.item",
		Conversation: &cat.Conversation{ID: s.Spec, Kind: "channel", Name: name},
		Payload:      payload,
		Triggers:     w.askedBy(s.Spec), // the rules that asked for this feed take it as theirs
	}
	if it.Author != "" {
		ev.Actor = &cat.Actor{ID: it.Author, Name: it.Author}
	}
	return ev
}

type result struct {
	Checked, New int
	Errors       []string // "spec：reason", in subscription order
}

// round checks the given feeds, a few at a time.
func (w *watcher) round(ctx context.Context, h *cat.Host, subs []*sub) result {
	errs := make([]string, len(subs))
	counts := make([]int, len(subs))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, s := range subs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			n, err := w.check(ctx, h, s)
			counts[i] = n
			msg := ""
			if err != nil && ctx.Err() == nil {
				msg = err.Error()
				errs[i] = s.Spec + "：" + msg
			}
			// Log transitions only, so a feed that stays down doesn't flood the log.
			s.mu.Lock()
			changed := msg != s.lastErr && ctx.Err() == nil
			if changed {
				s.lastErr = msg
			}
			s.mu.Unlock()
			if changed && msg != "" {
				h.Log("warn", "%s 获取失败：%s", s.Spec, msg)
			} else if changed {
				h.Log("info", "%s 已恢复", s.Spec)
			}
		}()
	}
	wg.Wait()
	res := result{Checked: len(subs)}
	for i := range subs {
		res.New += counts[i]
		if errs[i] != "" {
			res.Errors = append(res.Errors, errs[i])
		}
	}
	return res
}

// poll checks each subscription as often as it is asked to, starting with a
// look at once: on start, and at one a rule has just asked for.
func (w *watcher) poll(ctx context.Context, h *cat.Host) {
	status := ""
	last := map[*sub]time.Time{}
	for {
		subs := w.watched()
		now := time.Now()
		var due []*sub
		seen := make(map[*sub]time.Time, len(subs))
		for _, s := range subs {
			if t, ok := last[s]; ok && now.Sub(t) < time.Duration(s.every.Load()) {
				seen[s] = t
				continue
			}
			seen[s] = now
			due = append(due, s)
		}
		last = seen // subscriptions no longer watched are forgotten
		if len(due) > 0 {
			w.round(ctx, h, due)
		}
		if ctx.Err() != nil {
			return
		}
		// The instance is only "in error" when nothing works; a single broken
		// feed is reported in the log and shouldn't hide the healthy ones.
		next, failing := "", 0
		for _, s := range subs {
			s.mu.Lock()
			if s.lastErr != "" {
				if failing++; next == "" {
					next = "全部获取失败，将继续重试：" + s.Spec + "：" + s.lastErr
				}
			}
			s.mu.Unlock()
		}
		if failing < len(subs) {
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
		for _, s := range subs {
			wait = min(wait, time.Until(last[s].Add(time.Duration(s.every.Load()))))
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-time.After(max(wait, time.Second)):
		}
	}
}

// Kind is what a plugin's subscriptions are.
type Kind int

const (
	RSS    Kind = iota // feed URLs, in "feeds"
	RSSHub             // routes on one RSSHub instance, in "routes"
)

// New builds a plugin of the given kind. What it watches is said in the
// trigger nodes of rules, and may be listed in the instance's settings too,
// under the same name: "feeds" (RSS) or "routes" (RSSHub). How often to look,
// and whether to announce what is already there, is said beside it. Its "check" action takes the
// subscription to check in "feed" or "route".
func New(kind Kind) cat.Plugin {
	var (
		mu sync.Mutex
		w  *watcher // set once the instance has started
	)
	started := func() *watcher {
		mu.Lock()
		defer mu.Unlock()
		return w
	}
	param, noun := "feed", kind.noun()
	if kind == RSSHub {
		param = "route"
	}
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			nw, err := newWatcher(kind, h.Config)
			if err != nil {
				return err
			}
			mu.Lock()
			w = nw
			nw.watch(h.Triggers()) // under mu: a change of triggers comes after, not in between
			mu.Unlock()
			if n := len(nw.watched()); n > 0 {
				h.Log("info", "订阅 %d 个%s", n, noun)
			} else {
				h.Log("info", "还没有要订阅的%s：在规则的触发节点里填写", noun)
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
				_, _, _, errs := kind.asked(h.Config, list)
				return errs
			}
			before := len(w.watched())
			added, errs := w.watch(list)
			if n := len(w.watched()); len(added) > 0 || n != before {
				h.Log("info", "规则要订阅的%s有变化：现在订阅 %d 个", noun, n)
			}
			select { // a new subscription is looked at now, and a new pace is kept from now
			case w.wake <- struct{}{}:
			default:
			}
			return errs
		},
		Actions: map[string]cat.ActionFunc{
			"check": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				w := started()
				if w == nil {
					return nil, errors.New("实例尚未启动")
				}
				subs := w.watched()
				if want, _ := p[param].(string); strings.TrimSpace(want) != "" {
					want = strings.TrimSpace(want)
					if kind == RSSHub {
						want, _ = route(want)
					}
					i := slices.IndexFunc(subs, func(s *sub) bool { return s.Spec == want })
					if i < 0 {
						return nil, fmt.Errorf("没有订阅这个%s：%s", noun, want)
					}
					subs = subs[i : i+1]
				}
				res := w.round(ctx, h, subs)
				out := map[string]any{"checked": res.Checked, "new": res.New, "failed": len(res.Errors), "errors": res.Errors}
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
