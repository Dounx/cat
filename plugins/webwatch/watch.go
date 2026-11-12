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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andybalholm/cascadia"
	"github.com/dounx/cat/plugins/internal/watchlist"
	cat "github.com/dounx/cat/sdk/go"
)

const (
	defaultInterval = 10 // minutes
	maxBody         = 8 << 20
	maxKeep         = 256 << 10 // bytes of page text remembered for the next diff
	diffLinesShown  = 10        // per side, in payload.text
	lineRunes       = 200
	parallel        = 4
	userAgent       = "Mozilla/5.0 (compatible; Cat-WebWatch/0.1; +https://github.com/dounx/cat)"
)

// page is one watched page.
type page struct {
	Spec     string // "URL" or "URL selector": the conversation id and state key
	URL      string
	Selector string
	sel      cascadia.Matcher // nil: the whole page

	// How it is watched, as said beside it: by the rule that asks for it, or
	// by the instance's settings.
	ignore []*regexp.Regexp
	rules  string       // identifies the ignore patterns the stored hashes were made with
	key    string       // Spec and rules: the same page compared differently is watched apart
	every  atomic.Int64 // time between checks: the shortest anyone asks for

	mu      sync.Mutex // one fetch at a time: the poll loop and "check" can overlap
	lastErr string
}

// pageState is what is remembered about a page between checks and restarts.
type pageState struct {
	Hash     string `json:"hash"`
	Text     string `json:"text,omitempty"` // empty when the page is too large to keep
	Rules    string `json:"rules,omitempty"`
	Title    string `json:"title,omitempty"`
	ETag     string `json:"etag,omitempty"`
	Modified string `json:"modified,omitempty"`
}

type watcher struct {
	// the pages in the settings and those rules ask for in their triggers
	pages  watchlist.List[*page]
	client *http.Client
	wake   chan struct{} // what is watched has changed
}

func (p *page) interval() time.Duration { return time.Duration(p.every.Load()) }

// parsePages reads the pages a rule's trigger (or the instance's settings)
// lists under "pages", with what it says beside them about how to watch.
// Each line is a URL, optionally followed by a CSS selector.
func parsePages(params map[string]any) ([]*page, error) {
	str := func(k string) string { s, _ := params[k].(string); return strings.TrimSpace(s) }
	var ignore []*regexp.Regexp
	var patterns []string
	for _, line := range strings.Split(str("ignore"), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		re, err := regexp.Compile(line)
		if err != nil {
			return nil, fmt.Errorf("忽略规则不是有效的正则表达式「%s」：%v", line, err)
		}
		ignore = append(ignore, re)
		patterns = append(patterns, line)
	}
	rules := ""
	if len(patterns) > 0 {
		rules = hash(strings.Join(patterns, "\n"))
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

	text := str("pages")
	var out []*page
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		raw, selector := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			raw, selector = line[:i], strings.TrimSpace(line[i:])
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("看不懂的网页地址「%s」：应填完整的 http(s) 地址", raw)
		}
		p := &page{Spec: raw, URL: raw, Selector: selector}
		if selector != "" {
			p.Spec = raw + " " + selector
			if p.sel, err = cascadia.ParseGroup(selector); err != nil {
				return nil, fmt.Errorf("CSS 选择器无效「%s」：%v", selector, err)
			}
		}
		if seen[p.Spec] {
			continue
		}
		seen[p.Spec] = true
		p.ignore, p.rules, p.key = ignore, rules, p.Spec
		if rules != "" {
			p.key += "\x00" + rules
		}
		p.every.Store(int64(every))
		out = append(out, p)
	}
	return out, nil
}

func pageList() watchlist.List[*page] {
	return watchlist.List[*page]{
		Field: "pages", Parse: parsePages, Key: func(p *page) string { return p.key },
		// Rules that want a page looked at at different paces share one watch
		// of it, at the quickest of them.
		Merge: func(kept *page, wanted []*page) {
			every := wanted[0].interval()
			for _, p := range wanted[1:] {
				every = min(every, p.interval())
			}
			kept.every.Store(int64(every))
		},
	}
}

func newWatcher(cfg map[string]any) (*watcher, error) {
	w := &watcher{pages: pageList(), client: &http.Client{Timeout: 30 * time.Second}, wake: make(chan struct{}, 1)}
	return w, w.pages.Init(cfg)
}

func stateKey(key string) string { return "page:" + hash(key) }

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// snapshot is a page as it looks right now.
type snapshot struct {
	Title string
	Lines []string
}

// fetch downloads a page and extracts the text to compare. It returns nil
// without an error when the server says nothing changed since st was recorded.
func (w *watcher) fetch(ctx context.Context, p *page, st *pageState) (*snapshot, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", p.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
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
		hint := ""
		switch resp.StatusCode {
		case 401, 403:
			hint = "（网站拒绝访问：可能需要登录，或拦截了自动访问）"
		case 404, 410:
			hint = "（网页不存在）"
		case 429, 503:
			hint = "（被限流或暂时不可用）"
		}
		return nil, fmt.Errorf("HTTP %d%s", resp.StatusCode, hint)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, errors.New("网页内容过大")
	}
	title, lines, err := extract(body, resp.Header.Get("Content-Type"), p.sel, p.ignore)
	if err != nil {
		return nil, err
	}
	st.ETag, st.Modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return &snapshot{Title: title, Lines: lines}, nil
}

// check looks at one page and emits an event if it changed since last time.
func (w *watcher) check(ctx context.Context, h *cat.Host, p *page) (changed bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var st pageState
	raw, known := h.StateGet(stateKey(p.key))
	if known && json.Unmarshal([]byte(raw), &st) != nil {
		known = false
	}
	// Hashes made with other ignore patterns can't be compared: start over.
	if known && st.Rules != p.rules {
		known = false
	}
	if !known {
		st = pageState{}
	}
	snap, err := w.fetch(ctx, p, &st)
	if err != nil {
		return false, err
	}
	if snap == nil { // not modified
		return false, nil
	}
	text := strings.Join(snap.Lines, "\n")
	sum := hash(text)
	if known && sum != st.Hash {
		var old []string
		if st.Text != "" {
			old = strings.Split(st.Text, "\n")
		}
		// Without the old text (page too large to keep) there is no diff to show.
		if _, err := h.Emit(w.event(p, snap, old, st.Text != "" || st.Hash == hash(""), sum)); err != nil {
			// State is left alone, so the next check finds the change again.
			return false, fmt.Errorf("发出事件失败：%v", err)
		}
		changed = true
	}
	st.Hash, st.Rules, st.Title, st.Text = sum, p.rules, snap.Title, ""
	if len(text) <= maxKeep {
		st.Text = text
	}
	if data, err := json.Marshal(st); err == nil {
		if err := h.StateSet(stateKey(p.key), string(data)); err != nil {
			h.Log("error", "%s：保存状态失败：%v", p.Spec, err)
		}
	}
	if !known {
		h.Log("info", "开始监控 %s（%s，%d 行文字）", p.Spec, snap.Title, len(snap.Lines))
	}
	return changed, nil
}

func (w *watcher) event(p *page, snap *snapshot, old []string, haveOld bool, sum string) cat.Event {
	name := snap.Title
	if name == "" {
		name = p.URL
	}
	payload := map[string]any{
		"message_id": hash(p.Spec) + "-" + sum,
		"url":        p.URL,
		"title":      snap.Title,
		"selector":   p.Selector,
		"content":    strings.Join(snap.Lines, "\n"),
		"diff":       haveOld,
	}
	parts := []string{"网页有变动：" + name}
	if haveOld {
		added, removed := diffLines(old, snap.Lines)
		if added == nil {
			added = []string{}
		}
		if removed == nil {
			removed = []string{}
		}
		payload["added"], payload["removed"] = added, removed
		payload["previous"] = strings.Join(old, "\n")
		if s := section("+ ", added); s != "" {
			parts = append(parts, "新增：\n"+s)
		}
		if s := section("- ", removed); s != "" {
			parts = append(parts, "删除：\n"+s)
		}
		if len(added)+len(removed) == 0 {
			parts = append(parts, "内容的顺序变了。")
		}
	}
	parts = append(parts, p.URL)
	payload["text"] = strings.Join(parts, "\n\n")
	return cat.Event{
		Type:         "page.changed",
		Conversation: &cat.Conversation{ID: p.Spec, Kind: "channel", Name: name},
		Payload:      payload,
		Triggers:     w.pages.AskedBy(p.key), // the rules that asked for this page take it as theirs
	}
}

// section formats a few lines of a diff for a chat message.
func section(prefix string, lines []string) string {
	var b strings.Builder
	for i, l := range lines {
		if i == diffLinesShown {
			fmt.Fprintf(&b, "… 还有 %d 行", len(lines)-i)
			break
		}
		if r := []rune(l); len(r) > lineRunes {
			l = string(r[:lineRunes]) + "…"
		}
		b.WriteString(prefix + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

type result struct {
	Checked, Changed int
	Errors           []string // "spec：reason", in configuration order
}

// round checks the given pages, a few at a time.
func (w *watcher) round(ctx context.Context, h *cat.Host, pages []*page) result {
	errs := make([]string, len(pages))
	changed := make([]bool, len(pages))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, p := range pages {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			ok, err := w.check(ctx, h, p)
			changed[i] = ok
			msg := ""
			if err != nil && ctx.Err() == nil {
				msg = err.Error()
				errs[i] = p.Spec + "：" + msg
			}
			// Log transitions only, so a page that stays down doesn't flood the log.
			p.mu.Lock()
			flipped := msg != p.lastErr && ctx.Err() == nil
			if flipped {
				p.lastErr = msg
			}
			p.mu.Unlock()
			if flipped && msg != "" {
				h.Log("warn", "%s 检查失败：%s", p.Spec, msg)
			} else if flipped {
				h.Log("info", "%s 已恢复", p.Spec)
			}
		}()
	}
	wg.Wait()
	res := result{Checked: len(pages)}
	for i := range pages {
		if changed[i] {
			res.Changed++
		}
		if errs[i] != "" {
			res.Errors = append(res.Errors, errs[i])
		}
	}
	return res
}

// poll looks at each page as often as it is asked to, starting with a look
// at once: on start, and at a page a rule has just asked for.
func (w *watcher) poll(ctx context.Context, h *cat.Host) {
	status := ""
	last := map[*page]time.Time{}
	for {
		pages := w.pages.Items()
		now := time.Now()
		var due []*page
		seen := make(map[*page]time.Time, len(pages))
		for _, p := range pages {
			if t, ok := last[p]; ok && now.Sub(t) < p.interval() {
				seen[p] = t
				continue
			}
			seen[p] = now
			due = append(due, p)
		}
		last = seen // pages no longer watched are forgotten
		if len(due) > 0 {
			w.round(ctx, h, due)
		}
		if ctx.Err() != nil {
			return
		}
		// The instance is only "in error" when nothing works; a single broken
		// page is reported in the log and shouldn't hide the healthy ones.
		next, failing := "", 0
		for _, p := range pages {
			p.mu.Lock()
			if p.lastErr != "" {
				if failing++; next == "" {
					next = "网页全部检查失败，将继续重试：" + p.Spec + "：" + p.lastErr
				}
			}
			p.mu.Unlock()
		}
		if failing < len(pages) {
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
		for _, p := range pages {
			wait = min(wait, time.Until(last[p].Add(p.interval())))
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
// nodes of rules; an instance with settings of its own may list pages there
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
			nw.pages.Ask(h.Triggers()) // under mu: a change of triggers comes after, not in between
			mu.Unlock()
			if n := len(nw.pages.Items()); n > 0 {
				h.Log("info", "监控 %d 个网页", n)
			} else {
				h.Log("info", "还没有要监控的网页：在规则的触发节点里选「网页变动」后填写")
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
				probe := pageList()
				if probe.Init(h.Config) != nil {
					return nil // Start says so
				}
				return probe.Check(list)
			}
			before := len(w.pages.Items())
			added, errs := w.pages.Ask(list)
			if n := len(w.pages.Items()); len(added) > 0 || n != before {
				h.Log("info", "规则要监控的网页有变化：现在监控 %d 个", n)
			}
			select { // a new page is looked at now, and a new pace is kept from now
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
				pages := w.pages.Items()
				if want, _ := p["page"].(string); strings.TrimSpace(want) != "" {
					want = strings.Join(strings.Fields(want), " ")
					all := pages
					pages = nil
					for _, pg := range all {
						if pg.Spec == want || pg.URL == want {
							pages = append(pages, pg)
						}
					}
					if pages == nil {
						return nil, fmt.Errorf("没有监控这个网页：%s", want)
					}
				}
				res := w.round(ctx, h, pages)
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
