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
	"strconv"
	"strings"
	"sync"
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

	mu      sync.Mutex // one fetch at a time: the poll loop and "check" can overlap
	lastErr string
}

// subState is what is remembered about a feed between polls and restarts.
type subState struct {
	Seen     []string `json:"seen"` // hashed item keys, newest first
	ETag     string   `json:"etag,omitempty"`
	Modified string   `json:"modified,omitempty"`
}

type watcher struct {
	subs         []*sub
	interval     time.Duration
	secret       string // RSSHub access key, kept out of logs
	emitExisting bool
	client       *http.Client
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

// lines splits a multi-line setting, dropping blanks, comments and repeats.
func lines(text string, each func(line string) (*sub, error)) ([]*sub, error) {
	var out []*sub
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := each(line)
		if err != nil {
			return nil, err
		}
		if u, err := url.Parse(s.URL); err != nil || u.Host == "" {
			return nil, fmt.Errorf("地址无效：%s", line)
		}
		if !seen[s.Spec] {
			seen[s.Spec] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// parseFeeds turns the RSS plugin's "feeds" setting into subscriptions.
func parseFeeds(feeds string) ([]*sub, error) {
	out, err := lines(feeds, func(line string) (*sub, error) {
		if _, ok := route(line); ok {
			return nil, fmt.Errorf("「%s」是 RSSHub 路由，请用 RSSHub 插件订阅", line)
		}
		if !isURL(line) {
			return nil, fmt.Errorf("看不懂的订阅源「%s」：请填完整的 http(s) 地址", line)
		}
		return &sub{Spec: line, URL: line}, nil
	})
	if err == nil && len(out) == 0 {
		err = errors.New("还没有填订阅源")
	}
	return out, err
}

// parseRoutes turns the RSSHub plugin's "routes" setting into subscriptions
// on the instance at hub.
func parseRoutes(routes, hub, key string) ([]*sub, error) {
	hub = strings.TrimRight(strings.TrimSpace(hub), "/")
	if hub == "" {
		hub = defaultHub
	}
	if u, err := url.Parse(hub); err != nil || !isURL(hub) || u.Host == "" {
		return nil, errors.New("RSSHub 实例地址无效，应以 http:// 或 https:// 开头")
	}
	out, err := lines(routes, func(line string) (*sub, error) {
		spec, ok := route(line)
		switch {
		case isURL(line):
			return nil, fmt.Errorf("「%s」是完整地址，不是路由：普通 RSS / Atom 源请用 RSS 插件订阅", line)
		case !ok:
			return nil, fmt.Errorf("看不懂的路由「%s」：路由以 / 开头，如 /github/issue/DIYgod/RSSHub", line)
		}
		s := &sub{Spec: spec, URL: hub + spec, Hub: true}
		if u, err := url.Parse(s.URL); err == nil && key != "" {
			q := u.Query()
			q.Set("key", key)
			u.RawQuery = q.Encode()
			s.URL = u.String()
		}
		return s, nil
	})
	if err == nil && len(out) == 0 {
		err = errors.New("还没有填路由")
	}
	return out, err
}

func newWatcher(kind Kind, cfg map[string]any) (*watcher, error) {
	str := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }
	var (
		subs   []*sub
		secret string
		err    error
	)
	if kind == RSSHub {
		secret = str("access_key")
		subs, err = parseRoutes(str("routes"), str("base_url"), secret)
	} else {
		subs, err = parseFeeds(str("feeds"))
	}
	if err != nil {
		return nil, err
	}
	minutes := float64(defaultInterval)
	switch v := cfg["interval"].(type) {
	case float64:
		minutes = v
	case string:
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			minutes = n
		}
	}
	if minutes < 1 {
		minutes = 1
	}
	return &watcher{
		subs: subs, interval: time.Duration(minutes * float64(time.Minute)),
		secret: secret, emitExisting: cfg["emit_existing"] == true,
		client: &http.Client{Timeout: 30 * time.Second},
	}, nil
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
	announce := known || w.emitExisting
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

func (w *watcher) poll(ctx context.Context, h *cat.Host) {
	status := ""
	tick := time.NewTicker(w.interval)
	defer tick.Stop()
	for {
		res := w.round(ctx, h, w.subs)
		if ctx.Err() != nil {
			return
		}
		// The instance is only "in error" when nothing works; a single broken
		// feed is reported in the log and shouldn't hide the healthy ones.
		next := ""
		if len(res.Errors) == len(w.subs) {
			next = "全部获取失败，将继续重试：" + res.Errors[0]
		}
		if next != status {
			if next != "" {
				h.SetStatus("error", next)
			} else {
				h.SetStatus("running", "")
			}
			status = next
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Kind is what a plugin's subscriptions are.
type Kind int

const (
	RSS    Kind = iota // feed URLs, in the "feeds" setting
	RSSHub             // routes on one RSSHub instance, in the "routes" setting
)

// New builds a plugin of the given kind. Its "check" action takes the
// subscription to check in "feed" (RSS) or "route" (RSSHub).
func New(kind Kind) cat.Plugin {
	var w *watcher
	param, noun := "feed", "订阅源"
	if kind == RSSHub {
		param, noun = "route", "路由"
	}
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			var err error
			if w, err = newWatcher(kind, h.Config); err != nil {
				return err
			}
			h.Log("info", "订阅 %d 个%s，每 %s 检查一次", len(w.subs), noun, w.interval)
			go w.poll(ctx, h)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"check": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				if w == nil {
					return nil, errors.New("实例尚未启动")
				}
				subs := w.subs
				if want, _ := p[param].(string); strings.TrimSpace(want) != "" {
					want = strings.TrimSpace(want)
					if kind == RSSHub {
						want, _ = route(want)
					}
					subs = nil
					for _, s := range w.subs {
						if s.Spec == want {
							subs = []*sub{s}
						}
					}
					if subs == nil {
						return nil, fmt.Errorf("配置里没有这个%s：%s", noun, want)
					}
				}
				res := w.round(ctx, h, subs)
				out := map[string]any{"checked": res.Checked, "new": res.New, "failed": len(res.Errors), "errors": res.Errors}
				if res.Errors == nil {
					out["errors"] = []string{}
				}
				if len(res.Errors) == res.Checked {
					return out, errors.New(strings.Join(res.Errors, "；"))
				}
				return out, nil
			},
		},
	}
}
