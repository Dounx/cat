package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// fakeSite serves pages by path and records the requests it gets.
type fakeSite struct {
	*httptest.Server
	mu       sync.Mutex
	pages    map[string]string // path -> body
	types    map[string]string // path -> Content-Type
	requests []*http.Request
}

func newSite(t *testing.T) *fakeSite {
	f := &fakeSite{pages: map[string]string{}, types: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r)
		body, ok := f.pages[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		etag := fmt.Sprintf(`"%s"`, hash(body))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		ct := f.types[r.URL.Path]
		if ct == "" {
			ct = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSite) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pages[path] = body
}

func doc(body string) string {
	return `<!doctype html><html><head><title> Pricing
 page </title><style>p{color:red}</style></head><body>` + body + `<script>var t = Date.now()</script></body></html>`
}

// start starts the plugin and waits for the round it runs on start to have
// asked for everything it watches. A test that changes the site only after that
// knows the change is found by its own check, not by that round.
func start(t *testing.T, site *fakeSite, cfg map[string]any, state map[string]string) *cattest.Harness {
	t.Helper()
	watched := 0
	list, _ := cfg["pages"].(string)
	for _, line := range strings.Split(list, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			watched++
		}
	}
	count := func() int {
		site.mu.Lock()
		defer site.mu.Unlock()
		return len(site.requests)
	}
	want := count() + watched
	h := cattest.New(t, New(), cattest.Options{Config: cfg, State: state})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); count() < want; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the round on start made %d of %d requests", count()-want+watched, watched)
		}
	}
	return h
}

func check(t *testing.T, h *cattest.Harness, changed int) {
	t.Helper()
	out, err := h.Invoke("check", nil, nil)
	if err != nil || out["changed"] != float64(changed) || out["failed"] != float64(0) {
		t.Fatalf("check: out=%v err=%v, want changed=%d", out, err, changed)
	}
}

func TestChange(t *testing.T) {
	site := newSite(t)
	site.set("/p", doc(`<h1>Plans</h1><p>Basic   $5</p><p>Pro $10</p>`))
	h := start(t, site, map[string]any{"pages": "# watched\n" + site.URL + "/p\n"}, nil)
	check(t, h, 0) // the first look only sets the baseline

	// Markup and whitespace changes are not content changes.
	site.set("/p", doc(`<h1 class="x">Plans</h1><div>Basic $5</div>
<p>Pro   $10</p><!-- note -->`))
	check(t, h, 0)

	site.set("/p", doc(`<h1>Plans</h1><p>Basic $5</p><p>Pro $12</p><p>Team $30</p>`))
	check(t, h, 1)
	ev := h.NextEvent()
	p := ev.Payload
	if ev.Type != "page.changed" || p["url"] != site.URL+"/p" || p["title"] != "Pricing page" || p["diff"] != true {
		t.Fatalf("event: %+v", ev)
	}
	if !reflect.DeepEqual(p["added"], []any{"Pro $12", "Team $30"}) || !reflect.DeepEqual(p["removed"], []any{"Pro $10"}) {
		t.Fatalf("diff: added=%v removed=%v", p["added"], p["removed"])
	}
	if p["content"] != "Plans\nBasic $5\nPro $12\nTeam $30" || p["previous"] != "Plans\nBasic $5\nPro $10" {
		t.Fatalf("content: %q / %q", p["content"], p["previous"])
	}
	want := "网页有变动：Pricing page\n\n新增：\n+ Pro $12\n+ Team $30\n\n删除：\n- Pro $10\n\n" + site.URL + "/p"
	if p["text"] != want {
		t.Fatalf("text: %q", p["text"])
	}
	if ev.Conversation == nil || ev.Conversation.ID != site.URL+"/p" || ev.Conversation.Name != "Pricing page" {
		t.Fatalf("conversation: %+v", ev.Conversation)
	}

	// Unchanged: the server answers 304 and nothing is emitted again.
	check(t, h, 0)
	site.mu.Lock()
	last := site.requests[len(site.requests)-1]
	site.mu.Unlock()
	if last.Header.Get("If-None-Match") == "" {
		t.Fatal("expected a conditional request")
	}
}

func TestSelector(t *testing.T) {
	site := newSite(t)
	page := func(price, clock string) string {
		return doc(`<div id="clock">` + clock + `</div><ul><li class="price">` + price + `</li><li class="price"><b>B</b> 2</li></ul>`)
	}
	site.set("/p", page("A 1", "10:00"))
	spec := site.URL + "/p   li.price"
	h := start(t, site, map[string]any{"pages": spec}, nil)
	check(t, h, 0)
	site.set("/p", page("A 1", "10:01")) // outside the selector
	check(t, h, 0)
	site.set("/p", page("A 3", "10:02"))
	check(t, h, 1)
	ev := h.NextEvent()
	if ev.Payload["selector"] != "li.price" || ev.Payload["content"] != "A 3\nB 2" || ev.Conversation.ID != site.URL+"/p li.price" {
		t.Fatalf("event: %+v", ev)
	}

	// A page that lost the element is an error, not a change.
	site.set("/p", doc(`<p>Please log in</p>`))
	out, err := h.Invoke("check", map[string]any{"page": site.URL + "/p"}, nil)
	if err == nil || !strings.Contains(err.Error(), "选择器没有匹配到任何内容") || out != nil && out["changed"] == float64(1) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	site.set("/p", page("A 3", "10:03"))
	check(t, h, 0)
}

func TestIgnore(t *testing.T) {
	site := newSite(t)
	site.set("/p", doc(`<p>Stock: 3</p><p>Updated 10:00:01</p><p>阅读 120</p>`))
	cfg := map[string]any{"pages": site.URL + "/p", "ignore": "\\d{2}:\\d{2}:\\d{2}\n阅读 \\d+\n"}
	h := start(t, site, cfg, nil)
	check(t, h, 0)
	site.set("/p", doc(`<p>Stock: 3</p><p>Updated 10:05:09</p><p>阅读 135</p>`))
	check(t, h, 0)
	site.set("/p", doc(`<p>Stock: 0</p><p>Updated 10:06:00</p><p>阅读 140</p>`))
	check(t, h, 1)
	if ev := h.NextEvent(); !reflect.DeepEqual(ev.Payload["added"], []any{"Stock: 0"}) {
		t.Fatalf("added: %v", ev.Payload["added"])
	}

	// Changing the patterns starts over instead of reporting a change.
	key := stateKey(site.URL + "/p")
	h2 := start(t, site, map[string]any{"pages": site.URL + "/p"}, map[string]string{key: h.State(key)})
	check(t, h2, 0)
	site.set("/p", doc(`<p>Stock: 0</p><p>Updated 10:07:00</p><p>阅读 140</p>`))
	check(t, h2, 1)
}

func TestSurvivesRestart(t *testing.T) {
	site := newSite(t)
	site.set("/p", doc(`<p>one</p>`))
	cfg := map[string]any{"pages": site.URL + "/p"}
	h := start(t, site, cfg, nil)
	check(t, h, 0)
	key := stateKey(site.URL + "/p")
	site.set("/p", doc(`<p>two</p>`))
	h2 := start(t, site, cfg, map[string]string{key: h.State(key)})
	if ev := h2.NextEvent(); !reflect.DeepEqual(ev.Payload["removed"], []any{"one"}) {
		t.Fatalf("event: %v", ev.Payload)
	}
}

func TestNotHTMLAndCharset(t *testing.T) {
	site := newSite(t)
	site.types["/api"] = "application/json"
	site.set("/api", `{"version": "1.0"}`)
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(`<html><head><meta charset="gbk"><title>公告</title></head><body><p>你好</p></body></html>`)
	if err != nil {
		t.Fatal(err)
	}
	site.types["/gbk"] = "text/html"
	site.set("/gbk", gbk)
	h := start(t, site, map[string]any{"pages": site.URL + "/api\n" + site.URL + "/gbk"}, nil)
	check(t, h, 0)
	site.set("/api", `{"version": "1.1"}`)
	gbk2, _ := simplifiedchinese.GBK.NewEncoder().String(`<html><head><meta charset="gbk"><title>公告</title></head><body><p>再见</p></body></html>`)
	site.set("/gbk", gbk2)
	check(t, h, 2)
	got := map[string]string{}
	for range 2 {
		ev := h.NextEvent()
		got[strings.TrimPrefix(ev.Conversation.ID, site.URL)] = fmt.Sprint(ev.Conversation.Name, "|", ev.Payload["added"])
	}
	if got["/api"] != site.URL+`/api|[{"version": "1.1"}]` || got["/gbk"] != "公告|[再见]" {
		t.Fatalf("events: %v", got)
	}
}

func TestErrors(t *testing.T) {
	site := newSite(t)
	site.set("/ok", doc(`<p>fine</p>`))
	h := start(t, site, map[string]any{"pages": site.URL + "/ok\n" + site.URL + "/missing"}, nil)
	// One page works, one doesn't: reported, but not a failure of the action.
	out, err := h.Invoke("check", nil, nil)
	if err != nil || out["failed"] != float64(1) || !strings.Contains(fmt.Sprint(out["errors"]), "/missing：HTTP 404") {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if _, err := h.Invoke("check", map[string]any{"page": site.URL + "/missing"}, nil); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := h.Invoke("check", map[string]any{"page": "https://example.com/nope"}, nil); err == nil {
		t.Fatal("expected an error for an unknown page")
	}
}

// Rules say which pages to watch in their trigger nodes: the instance's own
// settings may be empty, and each event names the triggers that asked.
func TestPagesFromRules(t *testing.T) {
	site := newSite(t)
	site.set("/p", doc(`<p>Basic $5</p>`))
	requests := func() int {
		site.mu.Lock()
		defer site.mu.Unlock()
		return len(site.requests)
	}
	h := cattest.New(t, New(), cattest.Options{})
	// Cat hands the triggers over before the instance starts.
	errs, err := h.SetTriggers(
		cat.Trigger{ID: "t1", Params: map[string]any{"pages": site.URL + "/p"}},
		cat.Trigger{ID: "t2", Params: map[string]any{"pages": "# the same page\n" + site.URL + "/p"}},
		cat.Trigger{ID: "t-bad", Params: map[string]any{"pages": "example.com/page"}},
	)
	if err != nil || len(errs) != 1 || !strings.Contains(errs["t-bad"], "看不懂的网页地址") {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); requests() < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page was not looked at on start")
		}
	}
	check(t, h, 0)
	site.set("/p", doc(`<p>Basic $6</p>`))
	check(t, h, 1)
	if ev := h.NextEvent(); ev.Type != "page.changed" || strings.Join(ev.Triggers, ",") != "t1,t2" {
		t.Fatalf("event: %+v", ev)
	}

	// A rule is added while the instance runs: its page is looked at right
	// away. The others are gone, and so is their page.
	site.set("/q", doc(`<p>Q</p>`))
	before := requests()
	if errs, err := h.SetTriggers(cat.Trigger{ID: "t3", Params: map[string]any{"pages": site.URL + "/q"}}); err != nil || len(errs) != 0 {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	for deadline := time.Now().Add(10 * time.Second); requests() <= before; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the new page was not looked at")
		}
	}
	if out, err := h.Invoke("check", nil, nil); err != nil || out["checked"] != float64(1) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	// With no rule asking there is nothing to watch, which is not a failure.
	if _, err := h.SetTriggers(); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Invoke("check", nil, nil); err != nil || out["checked"] != float64(0) {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

func TestBadConfig(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"pages": "example.com/page"},
		{"pages": "https://example.com/ div["},
		{"pages": "https://example.com/", "ignore": "("},
	} {
		h := cattest.New(t, New(), cattest.Options{Config: cfg})
		if err := h.Start(); err == nil {
			t.Fatalf("config %v: expected an error", cfg)
		}
	}
}

func TestDiffLines(t *testing.T) {
	added, removed := diffLines([]string{"a", "b", "b", "c"}, []string{"c", "b", "d", "a"})
	if !reflect.DeepEqual(added, []string{"d"}) || !reflect.DeepEqual(removed, []string{"b"}) {
		t.Fatalf("added=%v removed=%v", added, removed)
	}
	long := make([]string, 13)
	for i := range long {
		long[i] = fmt.Sprint("line ", i)
	}
	if s := section("+ ", long); !strings.HasSuffix(s, "\n… 还有 3 行") || strings.Count(s, "\n") != 10 {
		t.Fatalf("section: %q", s)
	}
}

// A rule says beside its pages what to leave out; the instance needs no
// settings for it. The same page compared differently is watched apart.
func TestSettingsFromRules(t *testing.T) {
	site := newSite(t)
	site.set("/p", doc(`<p>Stock: 3</p><p>阅读 120</p>`))
	requests := func() int {
		site.mu.Lock()
		defer site.mu.Unlock()
		return len(site.requests)
	}
	h := cattest.New(t, New(), cattest.Options{})
	errs, err := h.SetTriggers(
		cat.Trigger{ID: "t-all", Params: map[string]any{"pages": site.URL + "/p", "interval": float64(1)}},
		cat.Trigger{ID: "t-stock", Params: map[string]any{"pages": site.URL + "/p", "ignore": "阅读 \\d+"}},
		cat.Trigger{ID: "t-stock-too", Params: map[string]any{"pages": site.URL + "/p", "ignore": "阅读 \\d+", "interval": float64(30)}},
		cat.Trigger{ID: "t-bad", Params: map[string]any{"pages": site.URL + "/p", "ignore": "("}},
	)
	if err != nil || len(errs) != 1 || !strings.Contains(errs["t-bad"], "忽略规则") {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); requests() < 2; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the page was not looked at on start")
		}
	}
	check(t, h, 0)
	site.set("/p", doc(`<p>Stock: 3</p><p>阅读 135</p>`))
	check(t, h, 1)
	if ev := h.NextEvent(); strings.Join(ev.Triggers, ",") != "t-all" {
		t.Fatalf("event: %+v", ev)
	}
	site.set("/p", doc(`<p>Stock: 0</p><p>阅读 135</p>`))
	check(t, h, 2)
	got := []string{strings.Join(h.NextEvent().Triggers, ","), strings.Join(h.NextEvent().Triggers, ",")}
	if sort.Strings(got); strings.Join(got, " ") != "t-all t-stock,t-stock-too" {
		t.Fatalf("triggers: %q", got)
	}
}

// Rules that ask for a page at different paces share one watch of it, at the
// quickest of them.
func TestInterval(t *testing.T) {
	list := pageList()
	if err := list.Init(map[string]any{"interval": float64(20)}); err != nil {
		t.Fatal(err)
	}
	ask := func(minutes ...any) time.Duration {
		t.Helper()
		var triggers []cat.Trigger
		for i, m := range minutes {
			params := map[string]any{"pages": "https://example.com/"}
			if m != nil {
				params["interval"] = m
			}
			triggers = append(triggers, cat.Trigger{ID: fmt.Sprint("t", i), Params: params})
		}
		if _, errs := list.Ask(triggers); len(errs) != 0 {
			t.Fatal(errs)
		}
		if n := len(list.Items()); n != 1 {
			t.Fatalf("%d pages", n)
		}
		return list.Items()[0].Interval()
	}
	if got := ask(float64(30), float64(5)); got != 5*time.Minute {
		t.Fatalf("quickest: %s", got)
	}
	if got := ask(float64(30)); got != 30*time.Minute {
		t.Fatalf("after the quick one left: %s", got)
	}
	if got := ask(nil); got != 20*time.Minute { // unsaid: as the instance's settings have it
		t.Fatalf("unsaid: %s", got)
	}
	if got := ask(0.1); got != time.Minute {
		t.Fatalf("floor: %s", got)
	}
}
