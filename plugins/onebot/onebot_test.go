package main_test

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

func build(t *testing.T, out, pkg string) {
	t.Helper()
	if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const token = "s3cret"

// fakeOneBot is a OneBot 11 forward WebSocket server standing in for the
// service the user runs. Each connection after the wizard's check is sent one
// group message; what send_msg is called with is recorded.
type fakeOneBot struct {
	*httptest.Server
	mu    sync.Mutex
	conns int
	sent  []string
}

func newFakeOneBot(t *testing.T) *fakeOneBot {
	f := &fakeOneBot{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(403)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		f.mu.Lock()
		f.conns++
		n := f.conns
		f.mu.Unlock()
		_ = wsjson.Write(ctx, c, map[string]any{"post_type": "meta_event", "meta_event_type": "lifecycle", "self_id": 10001})
		if n > 1 { // the first connection is the wizard checking the address
			go func() {
				time.Sleep(200 * time.Millisecond)
				_ = wsjson.Write(ctx, c, map[string]any{
					"post_type": "message", "message_type": "group", "time": 1700000000, "self_id": 10001,
					"user_id": 20002, "group_id": 30003, "message_id": 777, "raw_message": "[CQ:at,qq=10001] 你好",
					"message": []map[string]any{{"type": "at", "data": map[string]any{"qq": "10001"}}, {"type": "text", "data": map[string]any{"text": " 你好"}}},
					"sender":  map[string]any{"nickname": "nick", "card": "群名片"},
				})
			}()
		}
		for {
			var req struct {
				Action string         `json:"action"`
				Params map[string]any `json:"params"`
				Echo   string         `json:"echo"`
			}
			if err := wsjson.Read(ctx, c, &req); err != nil {
				return
			}
			var data any
			switch req.Action {
			case "get_login_info":
				data = map[string]any{"user_id": 10001, "nickname": "机器猫"}
			case "send_msg":
				p, _ := json.Marshal(req.Params)
				f.mu.Lock()
				f.sent = append(f.sent, string(p))
				f.mu.Unlock()
				data = map[string]any{"message_id": 888}
			default:
				_ = wsjson.Write(ctx, c, map[string]any{"status": "failed", "retcode": 1404, "message": "no such action", "echo": req.Echo})
				continue
			}
			_ = wsjson.Write(ctx, c, map[string]any{"status": "ok", "retcode": 0, "data": data, "echo": req.Echo})
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOneBot) wsURL() string { return "ws" + strings.TrimPrefix(f.URL, "http") }

func (f *fakeOneBot) sentLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.sent, "\n")
}

// host runs the shipped plugin, with its real manifest, under the real plugin host.
func host(t *testing.T) (*pluginhost.Host, *engine.Engine) {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "plugins")
	pdir := filepath.Join(root, "onebot")
	_ = os.MkdirAll(pdir, 0o755)
	bin := "cat-plugin-onebot"
	if os.PathSeparator == '\\' {
		bin += ".exe"
	}
	build(t, filepath.Join(pdir, bin), ".")
	raw, _ := os.ReadFile("plugin.json")
	_ = os.WriteFile(filepath.Join(pdir, "plugin.json"), raw, 0o644)

	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	e := engine.New(st, filepath.Join(tmp, "data"), nil)
	ph := pluginhost.New(e, root)
	if errs := ph.Scan(); len(errs) > 0 {
		t.Fatal(errs)
	}
	if err := e.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Stop(); st.Close() })
	return ph, e
}

// TestConnectEndToEnd runs the wizard and the instance it saves against a
// OneBot service: a wrong address and a wrong token are reported and asked
// again, then messages flow both ways.
func TestConnectEndToEnd(t *testing.T) {
	srv := newFakeOneBot(t)
	ph, e := host(t)

	view, err := ph.StartSetup("onebot", "", "我的 QQ")
	if err != nil {
		t.Fatal(err)
	}
	state := func() pluginhost.SetupView {
		v, _ := ph.Setup(view.ID)
		if v.State == pluginhost.SetupError {
			t.Fatalf("setup failed: %s", v.Error)
		}
		return v
	}
	form := func(problem string) func() bool {
		return func() bool {
			s := state()
			desc, _ := s.Step["description"].(string)
			return s.State == pluginhost.SetupWaiting && s.Step["title"] == "连接 OneBot 服务" && strings.Contains(desc, problem)
		}
	}
	answer := func(v map[string]any) {
		t.Helper()
		if err := ph.AnswerSetup(view.ID, v); err != nil {
			t.Fatal(err)
		}
	}

	// The wizard goes straight to the address: there is nothing else to choose.
	waitFor(t, "address form", form(""))
	if s := state(); s.Step["kind"] != "form" {
		t.Fatalf("first step: %+v", s.Step)
	}
	answer(map[string]any{"ws_url": "ws://127.0.0.1:1"})
	waitFor(t, "unreachable address reported", form("无法连接"))
	answer(map[string]any{"ws_url": srv.wsURL(), "token": "wrong"})
	waitFor(t, "wrong token reported", form("访问令牌不正确"))
	answer(map[string]any{"ws_url": srv.wsURL(), "token": token})
	waitFor(t, "setup done", func() bool { return state().State == pluginhost.SetupDone })

	inst, ok := e.Instance(view.InstanceID)
	if !ok || inst.Name != "我的 QQ" || inst.Config["ws_url"] != srv.wsURL() || inst.Config["token"] != engine.SecretMask {
		t.Fatalf("instance: %+v", inst)
	}
	if _, has := inst.Config["mode"]; has {
		t.Fatalf("there is only one way to connect, nothing to record: %+v", inst.Config)
	}
	waitFor(t, "instance running", func() bool {
		v, _ := e.Instance(view.InstanceID)
		if v.Status == engine.StatusError {
			t.Fatalf("instance failed: %s", v.Error)
		}
		return v.Status == engine.StatusRunning
	})

	// A group message that mentions the bot arrives as an event...
	waitFor(t, "message event", func() bool {
		l, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
		return len(l) >= 1
	})
	list, _ := e.Store.ListEvents(store.EventFilter{Type: "message.created"})
	ev := list[0]
	if ev.Conversation.ID != "group:30003" || ev.Conversation.Kind != "group" || ev.Actor.ID != "20002" || ev.Actor.Name != "群名片" {
		t.Fatalf("event: %+v %+v", ev.Conversation, ev.Actor)
	}
	if ev.Payload["text"] != "你好" || ev.Payload["mentioned"] != true || ev.Payload["message_id"] != "777" {
		t.Fatalf("payload: %+v", ev.Payload)
	}
	// ...and a reply goes back to the same group, quoting it.
	out, err := e.Invoke(context.Background(), view.InstanceID, "reply", map[string]any{"text": "喵"}, plugin.InvokeContext{Event: &ev})
	if err != nil || out["message_id"] != "888" {
		t.Fatalf("reply: %v %v", out, err)
	}
	if log := srv.sentLog(); !strings.Contains(log, `"group_id":30003`) || !strings.Contains(log, `"type":"reply"`) || !strings.Contains(log, "喵") {
		t.Fatalf("send_msg not as expected:\n%s", log)
	}
	if _, err := e.Invoke(context.Background(), view.InstanceID, "send", map[string]any{"conversation_id": "nonsense", "text": "x"}, plugin.InvokeContext{}); err == nil {
		t.Fatal("a malformed conversation id should be rejected")
	}
}

// An instance saved back when Cat ran NapCat itself has no service to connect
// to any more: it says so, and what to do, rather than failing obscurely.
func TestInstanceFromManagedNapCatExplains(t *testing.T) {
	_, e := host(t)
	for _, mode := range []string{"managed", "docker"} {
		inst, err := e.SaveInstance(store.Instance{Plugin: "onebot", Name: mode, Enabled: true,
			Config: map[string]any{"mode": mode, "ws_port": 3001, "token": token, "uin": "10001"}})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, mode+" instance to report", func() bool {
			v, _ := e.Instance(inst.ID)
			return v.Status == engine.StatusError
		})
		if v, _ := e.Instance(inst.ID); !strings.Contains(v.Error, "不再代为运行 NapCat") || !strings.Contains(v.Error, "接入向导") {
			t.Fatalf("%s: %q", mode, v.Error)
		}
	}
}
