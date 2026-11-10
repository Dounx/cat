package main

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

// parseSubs turns the "feeds" setting into subscriptions.
func parseSubs(feeds, hub, key string) ([]*sub, error) {
	hub = strings.TrimRight(strings.TrimSpace(hub), "/")
	if hub == "" {
		hub = defaultHub
	}
	if u, err := url.Parse(hub); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("RSSHub 实例地址无效，应以 http:// 或 https:// 开头")
	}
	var out []*sub
	seen := map[string]bool{}
	for _, line := range strings.Split(feeds, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s := &sub{Spec: line}
		switch lower := strings.ToLower(line); {
		case strings.HasPrefix(lower, "rsshub://"):
			s.Spec, s.Hub = "/"+strings.TrimLeft(line[len("rsshub://"):], "/"), true
		case strings.HasPrefix(line, "/"):
			s.Hub = true
		case strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "https://"):
		default:
			return nil, fmt.Errorf("看不懂的订阅源「%s」：RSSHub 路由以 / 开头，普通源填完整的 http(s) 地址", line)
		}
		s.URL = s.Spec
		if s.Hub {
			s.URL = hub + s.Spec
		}
		u, err := url.Parse(s.URL)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("订阅源地址无效：%s", line)
		}
		if s.Hub && key != "" {
			q := u.Query()
			q.Set("key", key)
			u.RawQuery = q.Encode()
			s.URL = u.String()
		}
		if seen[s.Spec] {
			continue
		}
		seen[s.Spec] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("还没有填订阅源")
	}
	return out, nil
}

func newWatcher(cfg map[string]any) (*watcher, error) {
	str := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }
	subs, err := parseSubs(str("feeds"), str("rsshub_base"), str("access_key"))
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
		secret: str("access_key"), emitExisting: cfg["emit_existing"] == true,
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
		"rsshub":       s.Hub,
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
			next = "订阅源全部获取失败，将继续重试：" + res.Errors[0]
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

// New builds the plugin.
func New() cat.Plugin {
	var w *watcher
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			var err error
			if w, err = newWatcher(h.Config); err != nil {
				return err
			}
			h.Log("info", "订阅 %d 个源，每 %s 检查一次", len(w.subs), w.interval)
			go w.poll(ctx, h)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"check": func(ctx context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				if w == nil {
					return nil, errors.New("实例尚未启动")
				}
				subs := w.subs
				if want, _ := p["feed"].(string); strings.TrimSpace(want) != "" {
					want = strings.TrimSpace(want)
					if strings.HasPrefix(strings.ToLower(want), "rsshub://") {
						want = "/" + strings.TrimLeft(want[len("rsshub://"):], "/")
					}
					subs = nil
					for _, s := range w.subs {
						if s.Spec == want {
							subs = []*sub{s}
						}
					}
					if subs == nil {
						return nil, fmt.Errorf("配置里没有这个订阅源：%s", want)
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
