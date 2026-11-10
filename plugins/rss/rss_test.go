package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dounx/cat/sdk/go/cattest"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// fakeSite serves feeds by path and records the requests it gets.
type fakeSite struct {
	*httptest.Server
	mu       sync.Mutex
	feeds    map[string]string // path -> body
	requests []*http.Request
	key      string // when set, requests must carry ?key=
}

func newSite(t *testing.T) *fakeSite {
	f := &fakeSite{feeds: map[string]string{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r)
		if f.key != "" && r.URL.Query().Get("key") != f.key {
			w.WriteHeader(403)
			return
		}
		body, ok := f.feeds[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			return
		}
		etag := fmt.Sprintf(`"%d-%d"`, len(body), strings.Count(body, "<item>"))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeSite) set(path, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.feeds[path] = body
}

func rss(items ...string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>Cat News</title><link>https://example.com/</link>
<atom:link href="https://example.com/feed.xml" rel="self"/>` + strings.Join(items, "") + `</channel></rss>`
}

func feedItem(n int) string {
	return fmt.Sprintf(`<item><title>Post %d</title><link>/posts/%d</link><guid>id-%d</guid>
<media:title>ignored</media:title><dc:creator>Ann</dc:creator><category>ops</category>
<pubDate>Tue, 10 Nov 2026 09:0%d:00 +0800</pubDate>
<description><![CDATA[<p>Hello &amp; <b>world</b> %d</p><img src="https://example.com/a.png?x=1&amp;y=2"><script>x()</script>]]></description>
<enclosure url="https://example.com/a.mp3" type="audio/mpeg"/></item>`, n, n, n, n, n)
}

func TestNewItemsOnly(t *testing.T) {
	site := newSite(t)
	site.set("/feed.xml", rss(feedItem(2), feedItem(1)))
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"feeds": "# my feeds\n" + site.URL + "/feed.xml\n"}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// The first look at a feed only sets the baseline.
	out, err := h.Invoke("check", nil, nil)
	if err != nil || out["new"] != float64(0) {
		t.Fatalf("baseline: out=%v err=%v", out, err)
	}
	site.set("/feed.xml", rss(feedItem(4), feedItem(3), feedItem(2), feedItem(1)))
	out, err = h.Invoke("check", nil, nil)
	if err != nil || out["new"] != float64(2) || out["failed"] != float64(0) {
		t.Fatalf("out=%v err=%v", out, err)
	}
	ev := h.NextEvent() // oldest first
	p := ev.Payload
	if ev.Type != "feed.item" || p["title"] != "Post 3" || p["message_id"] != "id-3" {
		t.Fatalf("event: %+v", ev)
	}
	if p["link"] != site.URL+"/posts/3" { // relative links resolve against the feed's URL
		t.Fatalf("link: %v", p["link"])
	}
	if p["content"] != "Hello & world 3" || p["author"] != "Ann" || p["published"] != "2026-11-10T01:03:00Z" {
		t.Fatalf("payload: %v", p)
	}
	if imgs, _ := p["images"].([]any); len(imgs) != 1 || imgs[0] != "https://example.com/a.png?x=1&y=2" {
		t.Fatalf("images: %v", p["images"])
	}
	if enc, _ := p["enclosures"].([]any); len(enc) != 1 || enc[0] != "https://example.com/a.mp3" {
		t.Fatalf("enclosures: %v", p["enclosures"])
	}
	want := "Post 3\n\nHello & world 3\n\n" + site.URL + "/posts/3"
	if p["text"] != want {
		t.Fatalf("text: %q", p["text"])
	}
	if ev.Conversation == nil || ev.Conversation.ID != site.URL+"/feed.xml" || ev.Conversation.Name != "Cat News" || ev.Conversation.Kind != "channel" {
		t.Fatalf("conversation: %+v", ev.Conversation)
	}
	if ev.Actor == nil || ev.Actor.Name != "Ann" {
		t.Fatalf("actor: %+v", ev.Actor)
	}
	if ev := h.NextEvent(); ev.Payload["title"] != "Post 4" {
		t.Fatalf("second event: %v", ev.Payload["title"])
	}
	// Nothing new: the server answers 304 and nothing is emitted again.
	if out, err = h.Invoke("check", nil, nil); err != nil || out["new"] != float64(0) {
		t.Fatalf("repeat: out=%v err=%v", out, err)
	}
	site.mu.Lock()
	last := site.requests[len(site.requests)-1]
	site.mu.Unlock()
	if last.Header.Get("If-None-Match") == "" {
		t.Fatal("expected a conditional request")
	}
}

func TestSeenSurvivesRestart(t *testing.T) {
	site := newSite(t)
	site.set("/feed.xml", rss(feedItem(1)))
	cfg := map[string]any{"feeds": site.URL + "/feed.xml"}
	h := cattest.New(t, New(), cattest.Options{Config: cfg})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("check", nil, nil); err != nil {
		t.Fatal(err)
	}
	key := stateKey(site.URL + "/feed.xml")
	saved := h.State(key)
	if saved == "" {
		t.Fatal("no state saved")
	}
	site.set("/feed.xml", rss(feedItem(2), feedItem(1)))
	h2 := cattest.New(t, New(), cattest.Options{Config: cfg, State: map[string]string{key: saved}})
	if err := h2.Start(); err != nil {
		t.Fatal(err)
	}
	if ev := h2.NextEvent(); ev.Payload["title"] != "Post 2" {
		t.Fatalf("event: %v", ev.Payload)
	}
}

func TestEmitExisting(t *testing.T) {
	site := newSite(t)
	site.set("/feed.xml", rss(feedItem(2), feedItem(1)))
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"feeds": site.URL + "/feed.xml", "emit_existing": true}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if a, b := h.NextEvent(), h.NextEvent(); a.Payload["title"] != "Post 1" || b.Payload["title"] != "Post 2" {
		t.Fatalf("events: %v, %v", a.Payload["title"], b.Payload["title"])
	}
}

func TestRSSHubRoute(t *testing.T) {
	site := newSite(t)
	site.key = "s3cret"
	site.set("/github/issue/DIYgod/RSSHub", rss(feedItem(1)))
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"feeds":       "rsshub://github/issue/DIYgod/RSSHub?limit=5\n/missing/route",
		"rsshub_base": site.URL + "/", "access_key": "s3cret", "emit_existing": true,
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	ev := h.NextEvent()
	if ev.Conversation.ID != "/github/issue/DIYgod/RSSHub?limit=5" || ev.Payload["rsshub"] != true {
		t.Fatalf("event: %+v", ev)
	}
	site.mu.Lock()
	q := site.requests[0].URL.Query()
	site.mu.Unlock()
	if q.Get("key") != "s3cret" {
		t.Fatalf("query: %v", q)
	}
	// One route works, one doesn't: reported, but not a failure of the action.
	out, err := h.Invoke("check", nil, nil)
	if err != nil || out["failed"] != float64(1) || !strings.Contains(fmt.Sprint(out["errors"]), "/missing/route：HTTP 404") {
		t.Fatalf("out=%v err=%v", out, err)
	}
	if _, err := h.Invoke("check", map[string]any{"feed": "/missing/route"}, nil); err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("err=%v", err)
	}
	if _, err := h.Invoke("check", map[string]any{"feed": "/nope"}, nil); err == nil {
		t.Fatal("expected an error for an unknown feed")
	}
}

func TestBadConfig(t *testing.T) {
	for _, feeds := range []string{"", "# nothing\n", "example.com/feed"} {
		h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"feeds": feeds}})
		if err := h.Start(); err == nil {
			t.Fatalf("feeds %q: expected an error", feeds)
		}
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"feeds": "/a", "rsshub_base": "rsshub.app"}})
	if err := h.Start(); err == nil {
		t.Fatal("expected an error for a base without scheme")
	}
}

func TestParseAtom(t *testing.T) {
	f, err := parseFeed([]byte(`<?xml version="1.0"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:media="http://search.yahoo.com/mrss/">
  <title type="html">Cat &amp;amp; Co</title>
  <link rel="self" href="https://example.com/atom.xml"/><link href="https://example.com/"/>
  <entry>
    <id>tag:example.com,2026:1</id><title>First</title>
    <link rel="enclosure" href="/a.mp3"/><link rel="alternate" href="/first"/>
    <updated>2026-11-10T01:00:00Z</updated>
    <author><name>Bob</name></author><category term="go"/>
    <summary>short</summary>
    <content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Full <i>text</i></p></div></content>
    <media:group><media:title>ignored</media:title></media:group>
  </entry>
</feed>`), "https://example.com/atom.xml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "Cat & Co" || f.Link != "https://example.com/" || len(f.Items) != 1 {
		t.Fatalf("feed: %+v", f)
	}
	it := f.Items[0]
	if it.ID != "tag:example.com,2026:1" || it.Title != "First" || it.Link != "https://example.com/first" || it.Author != "Bob" {
		t.Fatalf("item: %+v", it)
	}
	if htmlToText(it.HTML) != "Full text" || it.Published.IsZero() || len(it.Categories) != 1 || len(it.Enclosures) != 1 {
		t.Fatalf("item: %+v", it)
	}
}

func TestParseRDFAndCharset(t *testing.T) {
	doc := `<?xml version="1.0" encoding="GBK"?>
<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel><title>新闻</title><link>https://example.com/</link></channel>
<item><title>你好，世界</title><link>https://example.com/1</link><dc:date>2026-11-10T09:00:00+08:00</dc:date><description>正文</description></item>
</rdf:RDF>`
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(doc)
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseFeed([]byte(gbk), "https://example.com/rdf")
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "新闻" || len(f.Items) != 1 || f.Items[0].Title != "你好，世界" || f.Items[0].HTML != "正文" || f.Items[0].Published.IsZero() {
		t.Fatalf("feed: %+v", f)
	}
	if _, err := parseFeed([]byte("<html><body>rate limited</body></html>"), ""); err == nil {
		t.Fatal("expected an error for a non-feed document")
	}
}

func TestRSSHubErrorPage(t *testing.T) {
	page := `<html><body><h1>Looks like something went wrong</h1><p>Error Message:<br><code>FetchError: [GET] &quot;https://example.com/best&quot;: 419</code></p><p>Route: <code>/a/:b</code></p></body></html>`
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		if r.URL.Path == "/auth" {
			fmt.Fprint(w, strings.Replace(page, `FetchError: [GET] &quot;https://example.com/best&quot;: 419`, "RejectError: Authentication failed. Access denied.", 1))
			return
		}
		fmt.Fprint(w, page)
	}))
	defer hub.Close()
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"feeds": "/upstream\n/auth", "rsshub_base": hub.URL}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	_, err := h.Invoke("check", map[string]any{"feed": "/upstream"}, nil)
	if err == nil || !strings.Contains(err.Error(), `HTTP 503（RSSHub：FetchError: [GET] "https://example.com/best": 419）`) {
		t.Fatalf("err=%v", err)
	}
	_, err = h.Invoke("check", map[string]any{"feed": "/auth"}, nil)
	if err == nil || !strings.Contains(err.Error(), "检查访问密钥") {
		t.Fatalf("err=%v", err)
	}
}
