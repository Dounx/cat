package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// request is what the fake server remembers about a call.
type request struct {
	Method, Path, Body string
	Header             http.Header
}

// fakeAPI serves bodies by path and records the requests it gets.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   map[string]string // path -> body
	status   map[string]int    // path -> status, default 200
	requests []request
}

func newAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{bodies: map[string]string{}, status: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, request{r.Method, r.URL.Path, string(sent), r.Header})
		body, ok := f.bodies[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if code := f.status[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			fmt.Fprint(w, body)
			return
		}
		etag := fmt.Sprintf(`"%s"`, hash(body))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies[path] = body
}

func (f *fakeAPI) last() request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// start starts the plugin and waits for the round it runs on start to have
// asked for everything it watches. A test that changes the API only after that
// knows the change is found by its own check, not by that round.
func start(t *testing.T, srv *fakeAPI, cfg map[string]any, state map[string]string) *cattest.Harness {
	t.Helper()
	watched := 0
	list, _ := cfg["apis"].(string)
	for _, line := range strings.Split(list, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			watched++
		}
	}
	count := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.requests)
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

// changes renders an event's changes compactly: "type path old new".
func changes(t *testing.T, payload map[string]any) []string {
	t.Helper()
	list, ok := payload["changes"].([]any)
	if !ok {
		t.Fatalf("no changes in %v", payload)
	}
	var out []string
	for _, raw := range list {
		c := raw.(map[string]any)
		out = append(out, fmt.Sprint(c["type"], " ", c["path"], " ", canonical(c["old"]), " ", canonical(c["new"])))
	}
	return out
}

func TestChange(t *testing.T) {
	srv := newAPI(t)
	srv.set("/app", `{"version": "1.0", "size": 9007199254740993, "tags": ["a"], "beta": true}`)
	h := start(t, srv, map[string]any{"apis": "# watched\n" + srv.URL + "/app?key=secret\n"}, nil)
	check(t, h, 0) // the first look only sets the baseline

	// Key order and whitespace are not changes.
	srv.set("/app", `{"beta":true,"tags":["a"],"size":9007199254740993,"version":"1.0"}`)
	check(t, h, 0)

	srv.set("/app", `{"version": "1.1", "size": 9007199254740994, "tags": ["a"], "notes": "hi"}`)
	check(t, h, 1)
	ev := h.NextEvent()
	p := ev.Payload
	if ev.Type != "api.changed" || p["url"] != srv.URL+"/app?key=secret" || p["method"] != "GET" || p["diff"] != true {
		t.Fatalf("event: %+v", ev)
	}
	want := []string{
		`removed beta true null`,
		`added notes null "hi"`,
		// The test host reads numbers as float64 and rounds this one; the
		// text below, made by the plugin, has the exact digits.
		`changed size 9007199254740992 9007199254740994`,
		`changed version "1.0" "1.1"`,
	}
	if got := changes(t, p); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes: %q", got)
	}
	name := strings.TrimPrefix(srv.URL, "http://") + "/app"
	text := "接口返回有变动：" + name + "\n\n- beta：true\n+ notes：\"hi\"\n~ size：9007199254740993 → 9007199254740994\n~ version：\"1.0\" → \"1.1\""
	if p["text"] != text {
		t.Fatalf("text: %q", p["text"])
	}
	if v := p["value"].(map[string]any); v["version"] != "1.1" || p["previous"].(map[string]any)["version"] != "1.0" {
		t.Fatalf("value: %v previous: %v", p["value"], p["previous"])
	}
	// The query string may hold a key: it stays out of what people are shown.
	if ev.Conversation == nil || ev.Conversation.ID != srv.URL+"/app?key=secret" || ev.Conversation.Name != name {
		t.Fatalf("conversation: %+v", ev.Conversation)
	}

	// Unchanged: the server answers 304 and nothing is emitted again.
	check(t, h, 0)
	if srv.last().Header.Get("If-None-Match") == "" {
		t.Fatal("expected a conditional request")
	}
}

func TestPath(t *testing.T) {
	srv := newAPI(t)
	body := func(price int, clock string) string {
		return fmt.Sprintf(`{"now": %q, "data": {"items": [{"id": 1, "price": %d}, {"id": 2, "price": 7}]}}`, clock, price)
	}
	srv.set("/list", body(5, "10:00"))
	h := start(t, srv, map[string]any{"apis": srv.URL + "/list   data.items[0].price"}, nil)
	check(t, h, 0)
	srv.set("/list", body(5, "10:01")) // outside the path
	check(t, h, 0)
	srv.set("/list", body(6, "10:02"))
	check(t, h, 1)
	ev := h.NextEvent()
	if got := changes(t, ev.Payload); !reflect.DeepEqual(got, []string{"changed data.items[0].price 5 6"}) {
		t.Fatalf("changes: %q", got)
	}
	if ev.Payload["path"] != "data.items[0].price" || ev.Payload["value"] != float64(6) || ev.Conversation.ID != srv.URL+"/list data.items[0].price" {
		t.Fatalf("event: %+v", ev)
	}
	if !strings.HasSuffix(ev.Payload["text"].(string), "\n\n~ data.items[0].price：5 → 6") {
		t.Fatalf("text: %q", ev.Payload["text"])
	}

	// A response that lost the field is an error, not a change.
	srv.set("/list", `{"error": "rate limited"}`)
	out, err := h.Invoke("check", map[string]any{"api": srv.URL + "/list"}, nil)
	if err == nil || !strings.Contains(err.Error(), "返回里没有字段 data.items[0].price") || out != nil && out["changed"] == float64(1) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	srv.set("/list", body(6, "10:03"))
	check(t, h, 0)
}

func TestIgnore(t *testing.T) {
	srv := newAPI(t)
	body := func(stock int, ts string) string {
		return fmt.Sprintf(`{"ts": %q, "items": [{"stock": %d, "seen": %q}, {"stock": 1, "seen": %q}]}`, ts, stock, ts, ts)
	}
	srv.set("/s", body(3, "a"))
	cfg := map[string]any{"apis": srv.URL + "/s", "ignore": "ts\nitems[*].seen\nno.such.field\n"}
	h := start(t, srv, cfg, nil)
	check(t, h, 0)
	srv.set("/s", body(3, "b"))
	check(t, h, 0)
	srv.set("/s", body(0, "c"))
	check(t, h, 1)
	if got := changes(t, h.NextEvent().Payload); !reflect.DeepEqual(got, []string{"changed items[0].stock 3 0"}) {
		t.Fatalf("changes: %q", got)
	}

	// Changing the ignored fields starts over instead of reporting a change.
	key := stateKey(srv.URL + "/s")
	h2 := start(t, srv, map[string]any{"apis": srv.URL + "/s"}, map[string]string{key: h.State(key)})
	check(t, h2, 0)
	srv.set("/s", body(0, "d"))
	check(t, h2, 1)
}

func TestRequest(t *testing.T) {
	srv := newAPI(t)
	srv.set("/q", `{"total": 1}`)
	srv.set("/plain", "v1\n")
	cfg := map[string]any{
		"apis":    "post " + srv.URL + "/q total\n" + srv.URL + "/plain",
		"auth":    "Bearer abc",
		"headers": "X-Api-Version: 2\n",
		"body":    `{"q": "open"}`,
	}
	h := start(t, srv, cfg, nil)
	check(t, h, 0)
	seen := map[string]request{}
	srv.mu.Lock()
	for _, r := range srv.requests {
		seen[r.Path] = r
	}
	srv.mu.Unlock()
	q, plain := seen["/q"], seen["/plain"]
	if q.Method != "POST" || q.Body != `{"q": "open"}` || q.Header.Get("Content-Type") != "application/json" ||
		q.Header.Get("Authorization") != "Bearer abc" || q.Header.Get("X-Api-Version") != "2" {
		t.Fatalf("POST request: %+v", q)
	}
	if plain.Method != "GET" || plain.Body != "" || plain.Header.Get("Authorization") != "Bearer abc" {
		t.Fatalf("GET request: %+v", plain)
	}

	// A body that is not JSON is compared as text.
	srv.set("/q", `{"total": 2}`)
	srv.set("/plain", "v2\n")
	check(t, h, 2)
	got := map[string]string{}
	for range 2 {
		ev := h.NextEvent()
		got[strings.TrimPrefix(ev.Conversation.ID, srv.URL)] = strings.Join(changes(t, ev.Payload), ",")
	}
	if got["POST "+srv.URL+"/q total"] != "changed total 1 2" || got["/plain"] != `changed  "v1" "v2"` {
		t.Fatalf("events: %q", got)
	}
}

func TestSurvivesRestart(t *testing.T) {
	srv := newAPI(t)
	srv.set("/v", `{"v": 1}`)
	cfg := map[string]any{"apis": srv.URL + "/v"}
	h := start(t, srv, cfg, nil)
	check(t, h, 0)
	key := stateKey(srv.URL + "/v")
	srv.set("/v", `{"v": 2}`)
	h2 := start(t, srv, cfg, map[string]string{key: h.State(key)})
	if got := changes(t, h2.NextEvent().Payload); !reflect.DeepEqual(got, []string{"changed v 1 2"}) {
		t.Fatalf("changes: %q", got)
	}
}

func TestErrors(t *testing.T) {
	srv := newAPI(t)
	srv.set("/ok", `{"a": 1}`)
	srv.set("/down", `{"a": 1}`)
	h := start(t, srv, map[string]any{"apis": srv.URL + "/ok\n" + srv.URL + "/down\n" + srv.URL + "/missing"}, nil)
	// Some work, one doesn't: reported, but not a failure of the action.
	out, err := h.Invoke("check", nil, nil)
	if err != nil || out["failed"] != float64(1) || !strings.Contains(fmt.Sprint(out["errors"]), "/missing：HTTP 404") {
		t.Fatalf("out=%v err=%v", out, err)
	}
	// An error response is not a change, and neither is the recovery.
	srv.mu.Lock()
	srv.bodies["/down"], srv.status["/down"] = `{"error": "oops"}`, 500
	srv.mu.Unlock()
	if out, err := h.Invoke("check", map[string]any{"api": srv.URL + "/down"}, nil); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("out=%v err=%v", out, err)
	}
	srv.mu.Lock()
	srv.bodies["/down"], srv.status["/down"] = `{"a": 1}`, 0
	srv.mu.Unlock()
	out, err = h.Invoke("check", map[string]any{"api": srv.URL + "/down"}, nil)
	if err != nil || out["changed"] != float64(0) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if _, err := h.Invoke("check", map[string]any{"api": "https://example.com/nope"}, nil); err == nil {
		t.Fatal("expected an error for an unknown API")
	}
	// A path can't be applied to a body that isn't JSON.
	srv.set("/plain", "hello")
	h2 := start(t, srv, map[string]any{"apis": srv.URL + "/plain data"}, nil)
	if _, err := h2.Invoke("check", nil, nil); err == nil || !strings.Contains(err.Error(), "不是 JSON") {
		t.Fatalf("err=%v", err)
	}
}

// Rules say which APIs to watch in their trigger nodes: the instance's own
// settings may be empty, and each event names the triggers that asked.
func TestAPIsFromRules(t *testing.T) {
	srv := newAPI(t)
	srv.set("/v", `{"version":"1.0"}`)
	requests := func() int {
		srv.mu.Lock()
		defer srv.mu.Unlock()
		return len(srv.requests)
	}
	h := cattest.New(t, New(), cattest.Options{})
	// Cat hands the triggers over before the instance starts.
	errs, err := h.SetTriggers(
		cat.Trigger{ID: "t1", Params: map[string]any{"apis": srv.URL + "/v version"}},
		cat.Trigger{ID: "t2", Params: map[string]any{"apis": "GET " + srv.URL + "/v   version"}},
		cat.Trigger{ID: "t-bad", Params: map[string]any{"apis": "FETCH https://example.com/api"}},
	)
	if err != nil || len(errs) != 1 || !strings.Contains(errs["t-bad"], "不支持的请求方法") {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); requests() < 1; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the API was not called on start")
		}
	}
	check(t, h, 0)
	srv.set("/v", `{"version":"1.1"}`)
	check(t, h, 1)
	if ev := h.NextEvent(); ev.Type != "api.changed" || strings.Join(ev.Triggers, ",") != "t1,t2" {
		t.Fatalf("event: %+v", ev)
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
		{"apis": "example.com/api"},
		{"apis": "FETCH https://example.com/api"},
		{"apis": "https://example.com/api data[x]"},
		{"apis": "https://example.com/api data[*]"},
		{"apis": "https://example.com/api data..x"},
		{"apis": "https://example.com/api", "ignore": "a[1"},
		{"apis": "https://example.com/api", "headers": "no colon"},
	} {
		h := cattest.New(t, New(), cattest.Options{Config: cfg})
		if err := h.Start(); err == nil {
			t.Fatalf("config %v: expected an error", cfg)
		}
	}
}

func TestDiffArrays(t *testing.T) {
	parse := func(s string) any {
		v, ok := decode([]byte(s))
		if !ok {
			t.Fatalf("bad JSON: %s", s)
		}
		return v
	}
	for _, c := range []struct{ old, cur, want string }{
		// A new item at the front is one addition, not a change to every item.
		{`[{"id":1},{"id":2}]`, `[{"id":3},{"id":1},{"id":2}]`, `added list[0]  {"id":3}`},
		{`[1,2,3,4]`, `[1,4]`, `removed list[1] 2 ;removed list[2] 3 `},
		// Same length: items are compared field by field.
		{`[{"id":1,"n":1},{"id":2}]`, `[{"id":1,"n":2},{"id":2}]`, `changed list[0].n 1 2`},
		{`[1,2,2,3]`, `[2,9,2,8,7]`, `removed list[0] 1 ;removed list[3] 3 ;added list[1]  9;added list[3]  8;added list[4]  7`},
		{`[1,2]`, `{"a":1}`, `changed list [1,2] {"a":1}`},
		{`[]`, `[]`, ``},
	} {
		var out []change
		diff("list", parse(c.old), parse(c.cur), &out)
		var got []string
		for _, ch := range out {
			old, cur := "", ""
			if ch.Type != "added" {
				old = canonical(ch.Old)
			}
			if ch.Type != "removed" {
				cur = canonical(ch.New)
			}
			got = append(got, fmt.Sprint(ch.Type, " ", ch.Path, " ", old, " ", cur))
		}
		if s := strings.Join(got, ";"); s != c.want {
			t.Errorf("%s -> %s: got %q, want %q", c.old, c.cur, s, c.want)
		}
	}
}

func TestDescribe(t *testing.T) {
	long := strings.Repeat("长", 300)
	if s := describe(change{Path: "a", Type: "added", New: long}); len([]rune(s)) != lineRunes+1 || !strings.HasSuffix(s, "…") {
		t.Fatalf("describe: %d runes", len([]rune(s)))
	}
	w := &watcher{}
	old, cur := map[string]any{}, map[string]any{}
	for i := range 13 {
		cur[fmt.Sprint("k", i)] = json.Number("1")
	}
	text := w.event(&api{Name: "x"}, cur, old, true, "sum").Payload["text"].(string)
	if !strings.HasSuffix(text, "\n… 还有 3 处") || strings.Count(text, "\n+ ") != 10 {
		t.Fatalf("text: %q", text)
	}
}
