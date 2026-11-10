// Integration test: runs the real plugin (through bun) under the real plugin
// host, against a fake Discord REST API and Gateway.
package discord_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/pluginhost"
	"github.com/dounx/cat/core/store"
)

type fakeDiscord struct {
	*httptest.Server
	mu       sync.Mutex
	posts    []map[string]any
	typing   int
	handshak []map[string]any // identify / resume payloads, in order
	conn     *websocket.Conn
	ready    chan struct{}
}

func newFake(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{ready: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bot good-token" {
			w.WriteHeader(401)
			w.Write([]byte(`{"message":"401: Unauthorized"}`))
			return false
		}
		return true
	}
	mux.HandleFunc("GET /users/@me", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			w.Write([]byte(`{"id":"900","username":"catbot"}`))
		}
	})
	mux.HandleFunc("GET /gateway/bot", func(w http.ResponseWriter, r *http.Request) {
		if authed(w, r) {
			json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(f.URL, "http") + "/gateway"})
		}
	})
	mux.HandleFunc("POST /channels/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		body["channel"] = r.PathValue("id")
		f.mu.Lock()
		f.posts = append(f.posts, body)
		n := len(f.posts)
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"id": "m" + string(rune('0'+n))})
	})
	mux.HandleFunc("POST /channels/{id}/typing", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.typing++
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("/gateway/", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 100}})
		for {
			var msg map[string]any
			if err := wsjson.Read(ctx, c, &msg); err != nil {
				return
			}
			switch msg["op"].(float64) {
			case 1:
				_ = wsjson.Write(ctx, c, map[string]any{"op": 11})
			case 2, 6:
				f.mu.Lock()
				f.handshak = append(f.handshak, msg)
				f.conn = c
				f.mu.Unlock()
				kind := "READY"
				if msg["op"].(float64) == 6 {
					kind = "RESUMED"
				}
				_ = wsjson.Write(ctx, c, map[string]any{"op": 0, "t": kind, "s": 1, "d": map[string]any{
					"session_id": "sess-1", "resume_gateway_url": "ws" + strings.TrimPrefix(f.URL, "http") + "/gateway",
				}})
				f.ready <- struct{}{}
			}
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeDiscord) push(t *testing.T, seq int, typ string, d map[string]any) {
	t.Helper()
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if err := wsjson.Write(context.Background(), c, map[string]any{"op": 0, "t": typ, "s": seq, "d": d}); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func start(t *testing.T) (*engine.Engine, *pluginhost.Host) {
	t.Helper()
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	if _, err := os.Stat("node_modules"); err != nil {
		t.Skip("run `bun install` in plugins/discord first")
	}
	// Install a copy of the manifest that runs the TypeScript source directly.
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	_ = json.Unmarshal(raw, &manifest)
	src, _ := filepath.Abs("src/index.ts")
	manifest["command"] = []string{"bun", "run", src}
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "discord"), 0o755)
	b, _ := json.Marshal(manifest)
	_ = os.WriteFile(filepath.Join(root, "discord", "plugin.json"), b, 0o644)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, t.TempDir(), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })
	return e, ph
}

func TestDiscord(t *testing.T) {
	f := newFake(t)
	e, _ := start(t)
	inst, err := e.SaveInstance(store.Instance{Plugin: "discord", Enabled: true, Config: map[string]any{"token": "good-token", "api_base": f.URL}})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { v, _ := e.Instance(inst.ID); return v.Status == engine.StatusRunning })
	<-f.ready
	f.mu.Lock()
	identify := f.handshak[0]["d"].(map[string]any)
	f.mu.Unlock()
	if identify["token"] != "good-token" || int(identify["intents"].(float64))&(1<<15) == 0 {
		t.Fatalf("identify: %+v", identify)
	}

	// Our own messages are ignored; a guild message mentioning the bot is an event.
	f.push(t, 2, "MESSAGE_CREATE", map[string]any{"id": "own", "channel_id": "C1", "content": "me", "author": map[string]any{"id": "900", "username": "catbot", "bot": true}})
	f.push(t, 3, "MESSAGE_CREATE", map[string]any{
		"id": "M7", "channel_id": "C1", "guild_id": "G1", "content": "<@900> ship it", "timestamp": "2026-01-02T03:04:05Z",
		"author":   map[string]any{"id": "U1", "username": "ann", "global_name": "Ann"},
		"mentions": []map[string]any{{"id": "900"}},
	})
	waitFor(t, "event", func() bool {
		l, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
		return len(l) == 1
	})
	list, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
	ev := list[0]
	if ev.Conversation.ID != "C1" || ev.Conversation.Kind != "group" || ev.Actor.ID != "U1" || ev.Actor.Name != "Ann" {
		t.Fatalf("event: %+v %+v %+v", ev, ev.Conversation, ev.Actor)
	}
	if ev.Payload["text"] != "<@900> ship it" || ev.Payload["message_id"] != "M7" || ev.Payload["mentioned"] != true {
		t.Fatalf("payload: %+v", ev.Payload)
	}

	// reply goes to the same channel, quoting the message; long text is split.
	out, err := e.Invoke(context.Background(), inst.ID, "reply", map[string]any{"text": "shipping"}, plugin.InvokeContext{Event: &ev})
	if err != nil || out["message_id"] != "m1" {
		t.Fatalf("reply: %v %v", out, err)
	}
	if _, err := e.Invoke(context.Background(), inst.ID, "send", map[string]any{"channel_id": "C2", "text": strings.Repeat("x", 4500)}, plugin.InvokeContext{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Invoke(context.Background(), inst.ID, "typing", nil, plugin.InvokeContext{Event: &ev}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	posts, typing := f.posts, f.typing
	f.mu.Unlock()
	if len(posts) != 4 || typing != 1 {
		t.Fatalf("posts=%d typing=%d", len(posts), typing)
	}
	if posts[0]["channel"] != "C1" || posts[0]["content"] != "shipping" || posts[0]["message_reference"].(map[string]any)["message_id"] != "M7" {
		t.Fatalf("reply post: %+v", posts[0])
	}
	if posts[1]["channel"] != "C2" || len(posts[1]["content"].(string)) != 2000 {
		t.Fatalf("split post: %v", len(posts[1]["content"].(string)))
	}
	if _, err := e.Invoke(context.Background(), inst.ID, "reply", map[string]any{"text": "x"}, plugin.InvokeContext{}); err == nil {
		t.Fatal("reply without a triggering message should fail")
	}

	// Dropping the socket makes the plugin reconnect and resume its session.
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	c.Close(websocket.StatusGoingAway, "bye")
	select {
	case <-f.ready:
	case <-time.After(15 * time.Second):
		t.Fatal("plugin did not reconnect")
	}
	f.mu.Lock()
	second := f.handshak[1]
	f.mu.Unlock()
	if second["op"].(float64) != 6 || second["d"].(map[string]any)["session_id"] != "sess-1" {
		t.Fatalf("expected a resume, got %+v", second)
	}
	waitFor(t, "running again", func() bool { v, _ := e.Instance(inst.ID); return v.Status == engine.StatusRunning })
}

func TestDiscordBadToken(t *testing.T) {
	f := newFake(t)
	e, _ := start(t)
	inst, _ := e.SaveInstance(store.Instance{Plugin: "discord", Enabled: true, Config: map[string]any{"token": "nope", "api_base": f.URL}})
	waitFor(t, "error", func() bool { v, _ := e.Instance(inst.ID); return v.Status == engine.StatusError })
	if v, _ := e.Instance(inst.ID); !strings.Contains(v.Error, "Token 无效") {
		t.Fatalf("error = %q", v.Error)
	}
}

func TestDiscordSetup(t *testing.T) {
	f := newFake(t)
	e, ph := start(t)
	// The wizard validates against the API base carried in the existing config,
	// so seed an instance pointing at the fake.
	seed, _ := e.SaveInstance(store.Instance{Plugin: "discord", Name: "d", Enabled: false, Config: map[string]any{"api_base": f.URL}})
	view, err := ph.StartSetup("discord", seed.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	state := func() pluginhost.SetupView { v, _ := ph.Setup(view.ID); return v }
	waitFor(t, "token form", func() bool { return state().State == pluginhost.SetupWaiting })
	_ = ph.AnswerSetup(view.ID, map[string]any{"token": "nope"})
	waitFor(t, "form again with the problem", func() bool {
		s := state()
		desc, _ := s.Step["description"].(string)
		return s.State == pluginhost.SetupWaiting && strings.Contains(desc, "Token 无效")
	})
	_ = ph.AnswerSetup(view.ID, map[string]any{"token": "good-token"})
	waitFor(t, "invite step", func() bool { s := state(); return s.State == pluginhost.SetupWaiting && s.Step["kind"] == "confirm" })
	if url, _ := state().Step["url"].(string); !strings.Contains(url, "client_id=900") {
		t.Fatalf("invite url: %q", url)
	}
	_ = ph.AnswerSetup(view.ID, map[string]any{"confirmed": true})
	waitFor(t, "done", func() bool { return state().State == pluginhost.SetupDone })
	waitFor(t, "instance running with the new token", func() bool {
		v, _ := e.Instance(seed.ID)
		return v.Status == engine.StatusRunning && v.Config["token"] == "good-token"
	})
}
