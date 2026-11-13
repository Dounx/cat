package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// The tests don't depend on whether this machine has gh, or is logged in.
func TestMain(m *testing.M) {
	ghInstalled = func() (string, bool) { return "", false }
	os.Exit(m.Run())
}

// fakeGitHub is a tiny REST API: it serves the lists and objects it is given
// by path, answers conditional requests, and records what is posted.
type fakeGitHub struct {
	*httptest.Server
	mu          sync.Mutex
	lists       map[string][]map[string]any
	objects     map[string]map[string]any
	posts       []post
	edited      obj // what the GraphQL API says of acme/app: {"issues": [...], "pullRequests": [...]}
	graphql     int // how often it was asked
	notModified int
	asked       []string // the list requests, in order
}

type post struct {
	Path string
	Body map[string]any
}

type obj = map[string]any

func user(id int, login string) obj { return obj{"id": id, "login": login, "type": "User"} }

const app = "/repos/acme/app"

func newFake(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{
		lists: map[string][]map[string]any{"/user/repos": {
			{"full_name": "acme/app"}, {"full_name": "acme/secret", "private": true}, {"full_name": "acme/attic", "archived": true},
		}, app + "/issues": nil, app + "/issues/comments": nil, app + "/pulls/comments": nil, "/notifications": nil},
		objects: map[string]map[string]any{
			"/user": user(1, "cat-bot"), app: {"full_name": "acme/app"},
			app + "/issues/3": {"number": 3, "title": "Old one"},
		},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good-token" {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		if r.Method == "POST" && path == "/graphql" {
			f.graphql++
			repository := obj{"issues": obj{"nodes": []obj{}}, "pullRequests": obj{"nodes": []obj{}}}
			for k, nodes := range f.edited {
				repository[k] = obj{"nodes": nodes}
			}
			json.NewEncoder(w).Encode(obj{"data": obj{"repository": repository}})
			return
		}
		if r.Method == "POST" {
			var body map[string]any
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &body)
			f.posts = append(f.posts, post{path, body})
			n := len(f.posts)
			w.WriteHeader(201)
			switch {
			case strings.HasSuffix(path, "/comments"):
				json.NewEncoder(w).Encode(obj{"id": 900 + n, "html_url": fmt.Sprintf("https://github.test%s#issuecomment-%d", path, 900+n)})
			case strings.HasSuffix(path, "/issues"):
				json.NewEncoder(w).Encode(obj{"number": 12, "html_url": "https://github.test/acme/app/issues/12"})
			default:
				w.Write([]byte("{}"))
			}
			return
		}
		if items, ok := f.lists[path]; ok {
			f.asked = append(f.asked, path+"?"+r.URL.RawQuery)
			if items == nil {
				items = []map[string]any{}
			}
			data, _ := json.Marshal(items)
			etag := fmt.Sprintf(`"%d-%x"`, len(data), len(r.URL.RawQuery))
			if r.Header.Get("If-None-Match") == etag {
				f.notModified++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			w.Write(data)
			return
		}
		if o, ok := f.objects[path]; ok {
			json.NewEncoder(w).Encode(o)
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitHub) set(path string, items ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists[path] = items
}

func (f *fakeGitHub) postsCopy() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]post(nil), f.posts...)
}

// rounds waits until the lists have been asked n more times.
func (f *fakeGitHub) rounds(t *testing.T, n int) {
	t.Helper()
	f.mu.Lock()
	want := len(f.asked) + n
	f.mu.Unlock()
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		f.mu.Lock()
		got := len(f.asked)
		f.mu.Unlock()
		if got >= want {
			return
		}
	}
	t.Fatal("the lists are not being read")
}

func (f *fakeGitHub) config(more ...any) map[string]any {
	cfg := map[string]any{"token": "good-token", "api_base": f.URL}
	for i := 0; i+1 < len(more); i += 2 {
		cfg[more[i].(string)] = more[i+1]
	}
	return cfg
}

// watchApp is the trigger node of a rule that watches acme/app.
var watchApp = cat.Trigger{ID: "t1", Params: obj{"repos": "acme/app"}}

// begin starts the plugin as Cat does: told first what the rules ask for.
func begin(t *testing.T, h *cattest.Harness, rules ...cat.Trigger) {
	t.Helper()
	if errs, err := h.SetTriggers(rules...); err != nil || len(errs) != 0 {
		t.Fatalf("triggers: %v %v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
}

func fast(t *testing.T) {
	old := tick
	tick = time.Millisecond
	t.Cleanup(func() { tick = old })
}

// started is a state in which everything has been read up to 2026.
func started() map[string]string {
	const since = `{"since":"2026-01-01T00:00:00Z"}`
	return map[string]string{
		"repo:acme/app issues": since, "repo:acme/app issues/comments": since, "repo:acme/app pulls/comments": since, "mentions": since,
	}
}

func comment_(id int, by obj, issue string, created, updated, body string) obj {
	if updated == "" {
		updated = created
	}
	return obj{"id": id, "body": body, "user": by, "created_at": created, "updated_at": updated,
		"issue_url": "https://api.github.test/repos/" + issue, "html_url": fmt.Sprintf("https://github.test/c/%d", id)}
}

func cursorOf(t *testing.T, h *cattest.Harness, key string) cursor {
	t.Helper()
	var c cursor
	if err := json.Unmarshal([]byte(h.State(key)), &c); err != nil {
		t.Fatalf("cursor %s: %q", key, h.State(key))
	}
	return c
}

func none(t *testing.T, h *cattest.Harness) {
	t.Helper()
	if len(h.Events) != 0 {
		ev := <-h.Events
		t.Fatalf("unexpected event: %s %+v", ev.Type, ev.Payload)
	}
}

func TestMessagesAndActions(t *testing.T) {
	fast(t)
	f := newFake(t)
	ann, bob, me := user(42, "ann"), user(43, "bob"), user(1, "cat-bot")
	f.set(app+"/issues",
		obj{"number": 9, "title": "Faster start", "body": "", "user": bob, "created_at": "2025-12-01T00:00:00Z", "updated_at": "2026-01-04T01:00:00Z",
			"pull_request": obj{}, "html_url": "https://github.test/acme/app/pull/9"},
		obj{"number": 7, "title": "Crash on start", "body": "@Cat-Bot please look", "state": "open", "html_url": "https://github.test/acme/app/issues/7",
			"user": ann, "created_at": "2026-01-02T10:00:00Z", "updated_at": "2026-01-03T00:00:02Z", "labels": []obj{{"name": "bug"}}},
		// only changed since, and opened before: not news
		obj{"number": 3, "title": "Old one", "body": "", "user": ann, "created_at": "2025-06-01T00:00:00Z", "updated_at": "2026-01-03T12:00:00Z"},
	)
	f.set(app+"/issues/comments",
		comment_(400, bob, "acme/app/issues/3", "2025-06-02T00:00:00Z", "2026-01-03T12:00:00Z", "now with a fix"),
		comment_(503, me, "acme/app/issues/7", "2026-01-03T00:00:02Z", "", "written by hand, from the bot's account"),
		comment_(502, me, "acme/app/issues/7", "2026-01-03T00:00:01Z", "", "an answer\n\n"+marker),
		comment_(501, bob, "acme/app/issues/3", "2026-01-03T00:00:00Z", "", "me too, @cat-bot-fan"),
	)
	f.set(app+"/pulls/comments", obj{"id": 601, "body": "nit: rename", "user": bob, "created_at": "2026-01-04T00:00:00Z", "updated_at": "2026-01-04T00:00:00Z",
		"pull_request_url": "https://api.github.test/repos/acme/app/pulls/9", "html_url": "https://github.test/acme/app/pull/9#discussion_r601"})
	f.set(app+"/pulls/9/reviews",
		obj{"id": 800, "body": "", "state": "APPROVED", "user": ann, "submitted_at": "2026-01-04T00:30:00Z"},
		obj{"id": 801, "body": "one thing before it goes in", "state": "CHANGES_REQUESTED", "user": bob, "submitted_at": "2026-01-04T01:00:00Z"},
		obj{"id": 700, "body": "from last year", "state": "COMMENTED", "user": bob, "submitted_at": "2025-12-02T00:00:00Z"},
	)
	h := cattest.New(t, New(), cattest.Options{Config: f.config(), State: started()})
	begin(t, h, watchApp)

	// What one look finds comes oldest first.
	opened := h.NextEvent()
	if opened.Type != "message.created" || opened.Conversation.ID != "acme/app#7" || opened.Conversation.Name != "acme/app#7 Crash on start" || opened.Conversation.Kind != "group" {
		t.Fatalf("conversation: %+v %+v", opened, opened.Conversation)
	}
	if opened.Actor.ID != "42" || opened.Actor.Name != "ann" {
		t.Fatalf("actor: %+v", opened.Actor)
	}
	p := opened.Payload
	if p["text"] != "Crash on start\n\n@Cat-Bot please look" || p["message_id"] != "7" || p["mentioned"] != true || p["kind"] != "issue" ||
		p["repo"] != "acme/app" || p["number"] != float64(7) || p["is_pull_request"] != false || !reflect.DeepEqual(p["labels"], []any{"bug"}) {
		t.Fatalf("payload: %+v", p)
	}
	if !opened.Time.Equal(time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("time: %v", opened.Time)
	}
	commented := h.NextEvent()
	if commented.Type != "message.created" || commented.Conversation.Name != "acme/app#3 Old one" || commented.Payload["kind"] != "comment" ||
		commented.Payload["message_id"] != "501" || commented.Payload["mentioned"] != false || commented.Payload["title"] != "Old one" {
		t.Fatalf("comment: %+v %+v", commented.Conversation, commented.Payload)
	}
	// What the plugin wrote itself is skipped; what else the account writes is not.
	if byHand := h.NextEvent(); byHand.Payload["message_id"] != "503" || byHand.Actor.ID != "1" {
		t.Fatalf("own account: %+v", byHand.Payload)
	}
	edited := h.NextEvent()
	if edited.Type != "message.edited" || edited.Payload["message_id"] != "400" || edited.Payload["text"] != "now with a fix" ||
		!edited.Time.Equal(time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("edited: %s %+v %v", edited.Type, edited.Payload, edited.Time)
	}
	onDiff := h.NextEvent()
	if onDiff.Conversation.ID != "acme/app#9" || onDiff.Payload["kind"] != "review_comment" || onDiff.Payload["is_pull_request"] != true {
		t.Fatalf("comment on a diff: %+v %+v", onDiff.Conversation, onDiff.Payload)
	}

	reviewed := h.NextEvent()
	if reviewed.Conversation.ID != "acme/app#9" || reviewed.Conversation.Name != "acme/app#9 Faster start" || reviewed.Payload["kind"] != "review" ||
		reviewed.Payload["message_id"] != "801" || reviewed.Payload["review_state"] != "changes_requested" || reviewed.Payload["is_pull_request"] != true {
		t.Fatalf("review: %+v %+v", reviewed.Conversation, reviewed.Payload)
	}

	out, err := h.Invoke("reply", map[string]any{"text": "on it"}, &opened)
	if err != nil {
		t.Fatal(err)
	}
	if out["message_id"] != "901" || out["conversation_id"] != "acme/app#7" || !strings.HasSuffix(fmt.Sprint(out["url"]), "#issuecomment-901") {
		t.Fatalf("reply output: %+v", out)
	}
	if sent := f.postsCopy()[0]; sent.Path != app+"/issues/7/comments" || sent.Body["body"] != "on it\n\n"+marker {
		t.Fatalf("reply: %+v", sent)
	}
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "https://github.test/acme/app/pull/9#discussion_r601", "text": "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	if sent := f.postsCopy()[1]; sent.Path != app+"/issues/9/comments" {
		t.Fatalf("send by address: %+v", sent)
	}
	// Long comments are split.
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "acme/app#7", "text": strings.Repeat("a", maxText+10)}, nil); err != nil {
		t.Fatal(err)
	}
	if n := len(f.postsCopy()); n != 4 {
		t.Fatalf("want 4 posts after split, got %d", n)
	}
	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Fatal("reply without a triggering message should fail")
	}
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "nowhere", "text": "x"}, nil); err == nil {
		t.Fatal("send to something that is no conversation should fail")
	}

	for _, ev := range []cat.Event{opened, commented, onDiff, reviewed} {
		if _, err := h.Invoke("typing", nil, &ev); err != nil {
			t.Fatal(err)
		}
	}
	posts := f.postsCopy()
	var paths []string
	for _, p := range posts[len(posts)-4:] {
		paths = append(paths, p.Path)
		if p.Body["content"] != "eyes" {
			t.Fatalf("reaction: %+v", p)
		}
	}
	want := []string{app + "/issues/7/reactions", app + "/issues/comments/501/reactions", app + "/pulls/comments/601/reactions", app + "/issues/9/reactions"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("reactions: %v", paths)
	}

	out, err = h.Invoke("create_issue", map[string]any{"repo": "acme/app", "title": "Disk full", "text": "92%", "labels": "ops, urgent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["number"] != float64(12) || out["conversation_id"] != "acme/app#12" {
		t.Fatalf("create_issue output: %+v", out)
	}
	posts = f.postsCopy()
	if made := posts[len(posts)-1]; made.Path != app+"/issues" || made.Body["title"] != "Disk full" ||
		made.Body["body"] != "92%\n\n"+marker || !reflect.DeepEqual(made.Body["labels"], []any{"ops", "urgent"}) {
		t.Fatalf("create_issue: %+v", made)
	}
	// What a rule chooses repositories from: the watched ones, then the rest the account can reach.
	if opts, err := h.Options("@trigger", "repos"); err != nil || len(opts) != 2 || opts[0].Value != "acme/app" || opts[1].Value != "acme/secret" || opts[1].Note != "私有" {
		t.Fatalf("options: %+v %v", opts, err)
	}
	if opts, err := h.Options("create_issue", "repo"); err != nil || len(opts) != 2 || opts[0].Value != "acme/app" {
		t.Fatalf("options: %+v %v", opts, err)
	}

	// The cursors stay a little behind the newest thing seen, and later looks
	// neither repeat anything nor cost a request that counts.
	if c := cursorOf(t, h, "repo:acme/app issues"); !c.Since.Equal(time.Date(2026, 1, 4, 0, 59, 30, 0, time.UTC)) || c.Seen["v801"].IsZero() || len(c.Seen) != 3 {
		t.Fatalf("issues cursor: %+v", c)
	}
	if c := cursorOf(t, h, "repo:acme/app issues/comments"); !c.Since.Equal(time.Date(2026, 1, 3, 11, 59, 30, 0, time.UTC)) || len(c.Seen) != 1 {
		t.Fatalf("comments cursor: %+v", c)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.notModified
		f.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.notModified < 3 {
		t.Fatalf("conditional requests answered 304: %d", f.notModified)
	}
	if !strings.Contains(f.asked[0], "since=2025-12-31T23:59:59Z") || !strings.Contains(f.asked[0], "state=all") {
		t.Fatalf("first list: %s", f.asked[0])
	}
	none(t, h)
}

func TestFirstSightTakesOnlyWhatComesLater(t *testing.T) {
	fast(t)
	f := newFake(t)
	old := comment_(501, user(43, "bob"), "acme/app/issues/3", "2020-01-01T00:00:00Z", "", "before Cat came")
	f.set(app+"/issues/comments", old)
	h := cattest.New(t, New(), cattest.Options{Config: f.config()})
	begin(t, h, watchApp)
	deadline := time.Now().Add(3 * time.Second)
	for h.State("repo:acme/app issues/comments") == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if h.State("repo:acme/app issues/comments") == "" {
		t.Fatal("no cursor after the first look")
	}
	later := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	f.set(app+"/issues/comments", comment_(502, user(43, "bob"), "acme/app/issues/3", later, "", "after"), old)
	if ev := h.NextEvent(); ev.Payload["text"] != "after" {
		t.Fatalf("event: %+v", ev.Payload)
	}
}

func TestEditedTextsAreFoundWithoutAWebhook(t *testing.T) {
	fast(t)
	f := newFake(t)
	ann, bob := user(42, "ann"), user(43, "bob")
	f.set(app+"/issues",
		obj{"number": 9, "title": "Faster start", "body": "now with numbers", "user": bob, "created_at": "2025-12-01T00:00:00Z", "updated_at": "2026-01-04T01:00:00Z", "pull_request": obj{}},
		obj{"number": 3, "title": "Old one", "body": "said better", "user": ann, "created_at": "2025-06-01T00:00:00Z", "updated_at": "2026-01-03T12:00:00Z"},
		// changed, but not in its text: a label, say
		obj{"number": 4, "title": "Another", "body": "", "user": ann, "created_at": "2025-06-01T00:00:00Z", "updated_at": "2026-01-03T12:00:00Z"},
	)
	f.set(app+"/pulls/9/reviews",
		obj{"id": 700, "body": "on second thought, fine", "state": "COMMENTED", "user": bob, "submitted_at": "2025-12-02T00:00:00Z"},
		obj{"id": 701, "body": "untouched", "state": "COMMENTED", "user": bob, "submitted_at": "2025-12-03T00:00:00Z"},
	)
	f.edited = obj{
		"issues": []obj{{"number": 3, "lastEditedAt": "2026-01-03T12:00:00Z"}, {"number": 4, "lastEditedAt": "2025-06-02T00:00:00Z"}},
		"pullRequests": []obj{{"number": 9, "lastEditedAt": "2026-01-04T00:50:00Z", "reviews": obj{"nodes": []obj{
			{"databaseId": 700, "lastEditedAt": "2026-01-04T01:00:00Z"}, {"databaseId": 701, "lastEditedAt": nil},
		}}}},
	}
	h := cattest.New(t, New(), cattest.Options{Config: f.config(), State: started()})
	begin(t, h, watchApp)
	if ev := h.NextEvent(); ev.Type != "message.edited" || ev.Conversation.ID != "acme/app#3" || ev.Payload["kind"] != "issue" ||
		ev.Payload["text"] != "Old one\n\nsaid better" || !ev.Time.Equal(time.Date(2026, 1, 3, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("edited issue: %s %+v %+v", ev.Type, ev.Conversation, ev.Payload)
	}
	if ev := h.NextEvent(); ev.Type != "message.edited" || ev.Conversation.ID != "acme/app#9" || ev.Payload["kind"] != "issue" || ev.Payload["is_pull_request"] != true {
		t.Fatalf("edited pull request: %s %+v %+v", ev.Type, ev.Conversation, ev.Payload)
	}
	if ev := h.NextEvent(); ev.Type != "message.edited" || ev.Payload["kind"] != "review" || ev.Payload["message_id"] != "700" || ev.Payload["text"] != "on second thought, fine" {
		t.Fatalf("edited review: %s %+v", ev.Type, ev.Payload)
	}
	// Asked once for what has changed, and not again while nothing does.
	f.rounds(t, 6)
	none(t, h)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.graphql != 1 {
		t.Fatalf("GraphQL asked %d times", f.graphql)
	}
}

// deliver sends a webhook delivery, signed with key.
func deliver(t *testing.T, h *cattest.Harness, key, event string, body obj) int {
	t.Helper()
	data, _ := json.Marshal(body)
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(data)
	resp, err := h.Ingress(cat.IngressRequest{Method: "POST", Path: "/", Body: data, Headers: map[string]string{
		"X-Github-Event": event, "X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Status
}

func TestWebhookDeliversAtOnceAndTheListsDoNotRepeatIt(t *testing.T) {
	fast(t)
	f := newFake(t)
	ann := user(42, "ann")
	h := cattest.New(t, New(), cattest.Options{Config: f.config("webhook_secret", "s3cret"), State: started()})
	begin(t, h, watchApp)
	repository := obj{"full_name": "acme/app"}
	issue7 := obj{"number": 7, "title": "Crash on start", "body": "it crashes", "user": ann, "created_at": "2025-12-01T00:00:00Z", "updated_at": "2026-02-01T00:00:00Z"}
	c700 := comment_(700, ann, "acme/app/issues/7", "2026-02-01T00:10:00Z", "", "@cat-bot any news?")

	if st := deliver(t, h, "wrong", "issue_comment", obj{"action": "created", "issue": issue7, "comment": c700, "repository": repository}); st != 401 {
		t.Fatalf("a delivery signed with another key: %d", st)
	}
	if st := deliver(t, h, "s3cret", "ping", obj{"zen": "Keep it logically awesome."}); st != 200 {
		t.Fatalf("ping: %d", st)
	}
	none(t, h)

	if st := deliver(t, h, "s3cret", "issue_comment", obj{"action": "created", "issue": issue7, "comment": c700, "repository": repository}); st != 200 {
		t.Fatalf("delivery: %d", st)
	}
	ev := h.NextEvent()
	if ev.Type != "message.created" || ev.Conversation.Name != "acme/app#7 Crash on start" || ev.Payload["message_id"] != "700" ||
		ev.Payload["kind"] != "comment" || ev.Payload["mentioned"] != true {
		t.Fatalf("delivered comment: %+v %+v", ev.Conversation, ev.Payload)
	}
	// The same delivery again, and the same comment in the list: no news.
	deliver(t, h, "s3cret", "issue_comment", obj{"action": "created", "issue": issue7, "comment": c700, "repository": repository})
	f.set(app+"/issues/comments", c700)
	f.rounds(t, 3)
	none(t, h)

	// An edit comes once as well, whichever way it is heard of first.
	c700["body"], c700["updated_at"] = "@cat-bot any news? (edited)", "2026-02-01T00:20:00Z"
	f.set(app+"/issues/comments", c700)
	if ev := h.NextEvent(); ev.Type != "message.edited" || ev.Payload["text"] != "@cat-bot any news? (edited)" {
		t.Fatalf("edit from the list: %s %+v", ev.Type, ev.Payload)
	}
	deliver(t, h, "s3cret", "issue_comment", obj{"action": "edited", "issue": issue7, "comment": c700, "repository": repository})
	none(t, h)

	// Only a delivery tells that an issue's own text has changed.
	issue7["body"], issue7["updated_at"] = "it crashes on start", "2026-02-01T00:30:00Z"
	deliver(t, h, "s3cret", "issues", obj{"action": "edited", "issue": issue7, "repository": repository})
	if ev := h.NextEvent(); ev.Type != "message.edited" || ev.Payload["kind"] != "issue" || ev.Payload["text"] != "Crash on start\n\nit crashes on start" {
		t.Fatalf("edited issue: %s %+v", ev.Type, ev.Payload)
	}
	deliver(t, h, "s3cret", "issues", obj{"action": "labeled", "issue": issue7, "repository": repository})
	f.mu.Lock()
	f.edited = obj{"issues": []obj{{"number": 7, "lastEditedAt": "2026-02-01T00:30:01Z"}}}
	f.mu.Unlock()
	f.set(app+"/issues", issue7)
	f.rounds(t, 3)
	none(t, h)

	pr := obj{"number": 9, "title": "Faster start", "body": "", "user": ann, "created_at": "2026-02-02T00:00:00Z", "updated_at": "2026-02-02T00:00:00Z"}
	deliver(t, h, "s3cret", "pull_request", obj{"action": "opened", "pull_request": pr, "repository": repository})
	if ev := h.NextEvent(); ev.Conversation.ID != "acme/app#9" || ev.Payload["kind"] != "issue" || ev.Payload["is_pull_request"] != true {
		t.Fatalf("opened pull request: %+v", ev.Payload)
	}
	deliver(t, h, "s3cret", "pull_request_review", obj{"action": "submitted", "pull_request": pr, "repository": repository,
		"review": obj{"id": 801, "body": "ship it", "state": "approved", "user": ann, "submitted_at": "2026-02-02T01:00:00Z"}})
	if ev := h.NextEvent(); ev.Payload["kind"] != "review" || ev.Payload["review_state"] != "approved" || ev.Conversation.Name != "acme/app#9 Faster start" {
		t.Fatalf("review: %+v %+v", ev.Conversation, ev.Payload)
	}
	deliver(t, h, "s3cret", "pull_request_review_comment", obj{"action": "created", "pull_request": pr, "repository": repository,
		"comment": obj{"id": 601, "body": "nit", "user": ann, "created_at": "2026-02-02T02:00:00Z", "updated_at": "2026-02-02T02:00:00Z"}})
	if ev := h.NextEvent(); ev.Payload["kind"] != "review_comment" || ev.Conversation.ID != "acme/app#9" {
		t.Fatalf("comment on a diff: %+v", ev.Payload)
	}

	// A repository that isn't in the list is taken from a webhook all the same.
	deliver(t, h, "s3cret", "issues", obj{"action": "opened", "repository": obj{"full_name": "other/lib"},
		"issue": obj{"number": 1, "title": "Hello", "body": "", "user": ann, "created_at": "2026-02-03T00:00:00Z", "updated_at": "2026-02-03T00:00:00Z"}})
	if ev := h.NextEvent(); ev.Conversation.ID != "other/lib#1" {
		t.Fatalf("unwatched repository: %+v", ev.Conversation)
	}
	// What the plugin wrote itself is not news from a webhook either.
	deliver(t, h, "s3cret", "issue_comment", obj{"action": "created", "issue": issue7, "repository": repository,
		"comment": comment_(702, user(1, "cat-bot"), "acme/app/issues/7", "2026-02-03T00:00:00Z", "", "an answer\n\n"+marker)})
	none(t, h)
}

func TestWebhookNeedsASecret(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: f.config(), State: started()})
	begin(t, h, watchApp)
	if st := deliver(t, h, "", "ping", obj{}); st != 403 {
		t.Fatalf("status: %d", st)
	}
}

func TestMentionsAnywhere(t *testing.T) {
	fast(t)
	f := newFake(t)
	ann := user(42, "ann")
	thread := func(repo, typ, url, updated string) obj {
		return obj{"updated_at": updated, "repository": obj{"full_name": repo}, "subject": obj{"type": typ, "url": "https://api.github.test/repos/" + url}}
	}
	f.set("/notifications",
		thread("other/lib", "PullRequest", "other/lib/pulls/6", "2026-01-05T01:00:00Z"),
		thread("other/lib", "Issue", "other/lib/issues/5", "2026-01-05T00:00:00Z"),
		thread("acme/app", "Issue", "acme/app/issues/7", "2026-01-05T00:00:00Z"), // watched: the repository's own lists bring it
		thread("other/lib", "Discussion", "other/lib/discussions/2", "2026-01-05T00:00:00Z"),
	)
	f.objects["/repos/other/lib/issues/5"] = obj{"number": 5, "title": "Docs", "body": "", "user": ann, "created_at": "2025-01-01T00:00:00Z", "updated_at": "2026-01-05T00:00:00Z"}
	f.objects["/repos/other/lib/issues/6"] = obj{"number": 6, "title": "Rework", "body": "", "user": ann, "created_at": "2025-01-01T00:00:00Z", "updated_at": "2026-01-05T01:00:00Z", "pull_request": obj{}}
	f.set("/repos/other/lib/issues/5/comments",
		comment_(900, ann, "other/lib/issues/5", "2026-01-05T00:00:00Z", "", "hey @cat-bot, help"),
		comment_(901, ann, "other/lib/issues/5", "2026-01-05T00:00:00Z", "", "nothing for the bot"),
		comment_(800, ann, "other/lib/issues/5", "2025-01-02T00:00:00Z", "", "@cat-bot from long ago"),
	)
	f.set("/repos/other/lib/issues/6/comments")
	f.set("/repos/other/lib/pulls/6/comments")
	f.set("/repos/other/lib/pulls/6/reviews", obj{"id": 950, "body": "@cat-bot please check", "state": "COMMENTED", "user": ann, "submitted_at": "2026-01-05T01:00:00Z"})
	h := cattest.New(t, New(), cattest.Options{Config: f.config(), State: started()})
	begin(t, h, watchApp, cat.Trigger{ID: "t2", Params: obj{"mentions": true}})
	ev := h.NextEvent()
	if ev.Conversation.ID != "other/lib#5" || ev.Conversation.Name != "other/lib#5 Docs" || ev.Payload["message_id"] != "900" || ev.Payload["mentioned"] != true ||
		!reflect.DeepEqual(ev.Triggers, []string{"t2"}) { // for the rule that wants mentions, not the one that watches acme/app
		t.Fatalf("mention: %+v %+v", ev.Conversation, ev.Payload)
	}
	if ev := h.NextEvent(); ev.Conversation.ID != "other/lib#6" || ev.Payload["kind"] != "review" || ev.Payload["message_id"] != "950" {
		t.Fatalf("mention in a review: %+v %+v", ev.Conversation, ev.Payload)
	}
	want := time.Date(2026, 1, 5, 0, 59, 30, 0, time.UTC)
	for deadline := time.Now().Add(3 * time.Second); !cursorOf(t, h, "mentions").Since.Equal(want) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if c := cursorOf(t, h, "mentions"); !c.Since.Equal(want) {
		t.Fatalf("cursor: %+v", c)
	}
	f.rounds(t, 3)
	none(t, h)
}

// forms are the forms a wizard has asked, in order.
func forms(h *cattest.Harness) (out []cat.Step) {
	for len(h.Steps) > 0 {
		if s := <-h.Steps; s.Kind == cat.StepForm {
			out = append(out, s)
		}
	}
	return out
}

// What a rule asks for while the plugin runs is tried at once, and what
// can't be had is said of that rule.
func TestRulesSayWhatToWatch(t *testing.T) {
	fast(t)
	f := newFake(t)
	delete(f.lists, "/notifications") // a token that may not read them
	bob := user(43, "bob")
	old := comment_(501, bob, "acme/app/issues/3", "2026-01-02T00:00:00Z", "", "while no rule cared")
	f.set(app+"/issues/comments", old)
	// No rule yet: an account that is signed in, and nothing to look at.
	h := cattest.New(t, New(), cattest.Options{Config: f.config(), State: started()})
	begin(t, h)
	time.Sleep(50 * time.Millisecond)
	if f.mu.Lock(); len(f.asked) != 0 {
		t.Fatalf("lists were read with nothing to watch: %v", f.asked)
	}
	f.mu.Unlock()

	errs, err := h.SetTriggers(watchApp,
		cat.Trigger{ID: "t2", Params: obj{"repos": "acme/app\nacme/gone\nnot a repo"}},
		cat.Trigger{ID: "t3", Params: obj{"mentions": true}})
	if err != nil || len(errs) != 2 || !strings.Contains(errs["t2"], "「not a repo」") || !strings.Contains(errs["t3"], "读取通知失败") {
		t.Fatalf("errors: %v %v", errs, err)
	}
	if errs, _ := h.SetTriggers(watchApp, cat.Trigger{ID: "t2", Params: obj{"repos": "acme/app\nacme/gone"}}); !strings.Contains(errs["t2"], "acme/gone：找不到") {
		t.Fatalf("errors: %v", errs)
	}
	// What a rule comes to watch begins at the present, whatever was read of
	// it long ago; and a message is for every rule that watches its repository.
	f.rounds(t, 3)
	none(t, h)
	later := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	f.set(app+"/issues/comments", comment_(502, bob, "acme/app/issues/3", later, "", "now"), old)
	if ev := h.NextEvent(); ev.Payload["text"] != "now" || !reflect.DeepEqual(ev.Triggers, []string{"t1", "t2"}) {
		t.Fatalf("event: %+v for %v", ev.Payload, ev.Triggers)
	}
	// No rule any more: nothing is read any more.
	if _, err := h.SetTriggers(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	f.mu.Lock()
	n := len(f.asked)
	f.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if f.mu.Lock(); len(f.asked) != n {
		t.Fatal("lists are still read with nothing to watch")
	}
	f.mu.Unlock()
}

func TestBadConfigFailsStart(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"token": "wrong", "api_base": f.URL}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "Token 无效") {
		t.Fatalf("err = %v", err)
	}
}

// The wizard signs in and no more: what to watch is not its to ask.
func TestSetupAsksAgainUntilEverythingChecksOut(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "token"}
	h.Answers <- map[string]any{"token": "wrong"}
	h.Answers <- map[string]any{"method": "token"}
	h.Answers <- map[string]any{"token": "good-token"}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg, map[string]any{"token": "good-token", "api_base": f.URL}) {
		t.Fatalf("config: %+v", cfg)
	}
	asked := forms(h)
	if len(asked) != 4 || !strings.Contains(asked[2].Description, "Token 无效") {
		t.Fatalf("forms: %+v", asked)
	}
}

// fakeGH makes the stand-in for gh from testdata the gh "installed on this
// machine", and returns the directory it keeps its state in.
func fakeGH(t *testing.T) string {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in for gh is a shell script")
	}
	dir := t.TempDir()
	script, err := os.ReadFile(filepath.Join("testdata", "fakebin", "gh"))
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "gh")
	if err := os.WriteFile(bin, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_CONFIG_DIR", "")
	ghInstalled = func() (string, bool) { return bin, true }
	t.Cleanup(func() { ghInstalled = func() (string, bool) { return "", false } })
	return dir
}

func TestSignInThroughGH(t *testing.T) {
	f := newFake(t)
	state := fakeGH(t)

	// Authorization refused on GitHub: said so, and asked again.
	os.WriteFile(filepath.Join(state, "deny"), nil, 0o644)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "browser"}
	h.Answers <- nil
	if _, err := h.RunSetup(map[string]any{"api_base": f.URL}); err == nil {
		t.Fatal("setup should have been cancelled")
	}
	if asked := forms(h); len(asked) != 2 || !strings.Contains(asked[1].Description, "登录没有成功：error: authorization was denied") {
		t.Fatalf("forms: %+v", asked)
	}
	os.Remove(filepath.Join(state, "deny"))

	h = cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "browser"}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "good-token" {
		t.Fatalf("config: %+v", cfg)
	}
	var asked, codes []cat.Step
	for len(h.Steps) > 0 {
		switch s := <-h.Steps; s.Kind {
		case cat.StepForm:
			asked = append(asked, s)
		case cat.StepQRCode:
			codes = append(codes, s)
		}
	}
	// gh has no login of its own here, so there is none to offer.
	methods := asked[0].Schema["properties"].(map[string]any)["method"].(map[string]any)["enum"]
	if len(asked) != 1 || !reflect.DeepEqual(methods, []any{"browser", "token"}) {
		t.Fatalf("forms: %+v", asked)
	}
	if len(codes) != 1 || !strings.Contains(codes[0].Title, "WDJB-MJHT") || codes[0].Notice != "验证码已复制到剪贴板" || codes[0].URL != "https://github.test/login/device" || codes[0].Text != codes[0].URL {
		t.Fatalf("codes shown: %+v", codes)
	}
	// It signed in for the right host, apart from the login the machine has.
	args, _ := os.ReadFile(filepath.Join(state, "login-args"))
	host := strings.TrimPrefix(f.URL, "http://")
	if !strings.Contains(string(args), "--hostname "+host) || !strings.Contains(string(args), "--web") || !strings.Contains(string(args), "--insecure-storage") {
		t.Fatalf("gh auth login was run with: %s", args)
	}
	if ghHost("https://api.github.com") != "github.com" || ghHost("https://ghe.corp.example/api/v3") != "ghe.corp.example" {
		t.Fatal("ghHost")
	}
}

func TestTheLoginGHAlreadyHasIsOffered(t *testing.T) {
	f := newFake(t)
	state := fakeGH(t)
	os.WriteFile(filepath.Join(state, "own-token"), []byte("good-token\n"), 0o600)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "existing"}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["token"] != "good-token" {
		t.Fatalf("config: %+v", cfg)
	}
	asked := forms(h)
	method := asked[0].Schema["properties"].(map[string]any)["method"].(map[string]any)
	if !reflect.DeepEqual(method["enum"], []any{"existing", "browser", "token"}) || !strings.Contains(fmt.Sprint(method["x-enum-titles"]), "@cat-bot") ||
		asked[0].Values["method"] != "existing" {
		t.Fatalf("the form to sign in: %+v", asked[0])
	}
	if _, err := os.Stat(filepath.Join(state, "login-args")); err == nil {
		t.Fatal("gh was made to sign in again")
	}
}

func TestMentions(t *testing.T) {
	b := &bot{me: account{Login: "Cat-Bot"}}
	for text, want := range map[string]bool{
		"hey @cat-bot": true, "@Cat-Bot, look": true, "@cat-bot-fan @cat-bot!": true,
		"@cat-bot-fan": false, "cat-bot": false, "": false,
	} {
		if got := b.mentions(text); got != want {
			t.Errorf("mentions(%q) = %v", text, got)
		}
	}
}
