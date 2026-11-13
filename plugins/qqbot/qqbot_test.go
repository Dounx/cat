package main

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeQQ is a tiny open platform: it hands out a token, runs a gateway that
// pushes queued events after login, and records what the bot sends.
type fakeQQ struct {
	*httptest.Server
	mu     sync.Mutex
	queue  []map[string]any // events pushed after the next login
	logins []map[string]any // identify / resume payloads, in order
	sent   []map[string]any
	paths  []string
	drop   chan struct{} // closes the current gateway connection
	refuse int           // how many logins to turn away first
}

func newFake(t *testing.T) *fakeQQ {
	f := &fakeQQ{drop: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/app/getAppAccessToken", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["appId"] != "1024" || in["clientSecret"] != "good-secret" {
			json.NewEncoder(w).Encode(map[string]any{"code": 10004, "message": "机器人不存在"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": "7200"})
	})
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "QQBot tok" {
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]any{"code": 11244, "message": "AccessToken无效或过期"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/gateway", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"url": "ws" + strings.TrimPrefix(f.URL, "http") + "/ws"})
	}))
	mux.HandleFunc("/users/@me", authed(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "1", "username": "小猫"})
	}))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx := r.Context()
		push := func(v any) { data, _ := json.Marshal(v); _ = conn.Write(ctx, websocket.MessageText, data) }
		push(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 20}})
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var login struct {
			Op int            `json:"op"`
			D  map[string]any `json:"d"`
		}
		_ = json.Unmarshal(data, &login)
		login.D["op"] = float64(login.Op)
		f.mu.Lock()
		f.logins = append(f.logins, login.D)
		queue := f.queue
		f.queue = nil
		f.mu.Unlock()
		f.mu.Lock()
		refused := f.refuse > 0
		if refused {
			f.refuse--
		}
		f.mu.Unlock()
		if login.D["token"] != "QQBot tok" || refused {
			push(map[string]any{"op": 9, "d": false})
			return
		}
		seq := 1
		if login.Op == 2 {
			push(map[string]any{"op": 0, "s": seq, "t": "READY", "d": map[string]any{"session_id": "sess-1", "user": map[string]any{"id": "1", "username": "小猫", "bot": true}}})
		}
		for _, ev := range queue {
			seq++
			ev["op"], ev["s"] = 0, seq
			push(ev)
		}
		if login.Op == 6 {
			push(map[string]any{"op": 0, "s": seq + 1, "t": "RESUMED", "d": ""})
		}
		go func() {
			<-f.drop
			conn.CloseNow()
		}()
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var hb struct{ Op int }
			if json.Unmarshal(data, &hb) == nil && hb.Op == 1 {
				push(map[string]any{"op": 11})
			}
		}
	})
	mux.HandleFunc("/v2/", authed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["msg_type"] == float64(2) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"code": 304036, "message": "无Markdown模板权限"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sent = append(f.sent, body)
		f.paths = append(f.paths, r.URL.Path)
		json.NewEncoder(w).Encode(map[string]any{"id": "ROBOT1.0_" + string(rune('a'+len(f.sent)-1)), "timestamp": "2026-11-13T10:00:00+08:00"})
	}))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeQQ) snapshot() (sent []map[string]any, paths []string, logins []map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...), append([]string(nil), f.paths...), append([]map[string]any(nil), f.logins...)
}

func config(f *fakeQQ) map[string]any {
	return map[string]any{"app_id": "1024", "secret": "good-secret", "api_base": f.URL}
}

func TestReceiveAndReply(t *testing.T) {
	f := newFake(t)
	now := time.Now().Format(time.RFC3339)
	f.queue = []map[string]any{
		{"t": "GROUP_AT_MESSAGE_CREATE", "id": "GROUP_AT_MESSAGE_CREATE:1", "d": map[string]any{
			"id": "MSG1", "content": " /deploy now ", "timestamp": now, "group_openid": "G1",
			"author":      map[string]any{"member_openid": "M1"},
			"attachments": []map[string]any{{"content_type": "image/png", "filename": "a.png", "url": "https://x/a.png"}},
		}},
		{"t": "C2C_MESSAGE_CREATE", "id": "C2C_MESSAGE_CREATE:2", "d": map[string]any{
			"id": "MSG2", "content": "你好", "timestamp": now, "author": map[string]any{"user_openid": "U1"},
		}},
		{"t": "GROUP_ADD_ROBOT", "d": map[string]any{"timestamp": 1700000000, "group_openid": "G2", "op_member_openid": "M9"}},
	}
	h := cattest.New(t, New(), cattest.Options{Config: config(f)})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}

	group := h.NextEvent()
	if group.Type != "message.created" || group.Conversation.ID != "group:G1" || group.Conversation.Kind != "group" || group.Actor.ID != "M1" {
		t.Fatalf("group event: %+v %+v %+v", group, group.Conversation, group.Actor)
	}
	p := group.Payload
	if p["text"] != "/deploy now" || p["message_id"] != "MSG1" || p["mentioned"] != true || p["command"] != "deploy" || p["args"] != "now" {
		t.Fatalf("group payload: %+v", p)
	}
	if a := p["attachments"].([]any)[0].(map[string]any); a["kind"] != "image" || a["url"] != "https://x/a.png" {
		t.Fatalf("attachment: %+v", a)
	}
	private := h.NextEvent()
	if private.Conversation.ID != "user:U1" || private.Conversation.Kind != "private" || private.Payload["text"] != "你好" {
		t.Fatalf("private event: %+v %+v", private.Conversation, private.Payload)
	}
	notice := h.NextEvent()
	if notice.Type != "notice" || notice.Payload["notice_type"] != "group_add_robot" || notice.Conversation.ID != "group:G2" || notice.Time.Unix() != 1700000000 {
		t.Fatalf("notice: %+v", notice)
	}

	// A reply names the message it answers; Markdown QQ refuses goes out as text.
	out, err := h.Invoke("reply", map[string]any{"text": "**好的**", "markdown": true}, &group)
	if err != nil {
		t.Fatal(err)
	}
	sent, paths, logins := f.snapshot()
	if out["message_id"] != "ROBOT1.0_a" || paths[0] != "/v2/groups/G1/messages" {
		t.Fatalf("reply: %+v %v", out, paths)
	}
	if sent[0]["msg_id"] != "MSG1" || sent[0]["content"] != "**好的**" || sent[0]["msg_type"] != float64(0) || sent[0]["msg_seq"] == nil {
		t.Fatalf("reply body: %+v", sent[0])
	}
	if logins[0]["op"] != float64(2) || logins[0]["intents"] != float64(1<<25) {
		t.Fatalf("identify: %+v", logins[0])
	}

	// Sending back to where the trigger came from goes out as a reply, with a
	// msg_seq of its own; sending elsewhere is proactive.
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "user:U1", "text": "one"}, &private); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "group:G9", "text": "two"}, &private); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("typing", nil, &private); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Invoke("typing", nil, &group); err != nil {
		t.Fatal(err)
	}
	sent, paths, _ = f.snapshot()
	if len(sent) != 4 {
		t.Fatalf("want 4 sends, got %d: %+v", len(sent), sent)
	}
	if paths[1] != "/v2/users/U1/messages" || sent[1]["msg_id"] != "MSG2" || sent[1]["msg_seq"] == sent[0]["msg_seq"] {
		t.Fatalf("passive send: %s %+v", paths[1], sent[1])
	}
	if paths[2] != "/v2/groups/G9/messages" || sent[2]["msg_id"] != nil {
		t.Fatalf("proactive send: %s %+v", paths[2], sent[2])
	}
	if sent[3]["msg_type"] != float64(6) || sent[3]["msg_id"] != "MSG2" || paths[3] != "/v2/users/U1/messages" {
		t.Fatalf("typing: %+v", sent[3])
	}

	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Fatal("reply without a triggering message should fail")
	}
	if _, err := h.Invoke("send", map[string]any{"conversation_id": "12345", "text": "x"}, nil); err == nil {
		t.Fatal("a conversation id without a kind should be refused")
	}
}

func TestResumesAfterDisconnect(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: config(f)})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.queue = []map[string]any{{"t": "C2C_MESSAGE_CREATE", "d": map[string]any{"id": "MISSED", "content": "还在吗", "author": map[string]any{"user_openid": "U1"}}}}
	f.mu.Unlock()
	f.drop <- struct{}{}
	if ev := h.NextEvent(); ev.Payload["message_id"] != "MISSED" {
		t.Fatalf("event after reconnect: %+v", ev.Payload)
	}
	_, _, logins := f.snapshot()
	if len(logins) != 2 || logins[1]["op"] != float64(6) || logins[1]["session_id"] != "sess-1" || logins[1]["seq"] != float64(1) {
		t.Fatalf("logins: %+v", logins)
	}
}

// A login right after another one is turned away; the bot waits and tries
// again instead of failing the instance.
func TestRetriesRefusedLogin(t *testing.T) {
	f := newFake(t)
	f.refuse = 1
	old := loginRetry
	loginRetry = 10 * time.Millisecond
	t.Cleanup(func() { loginRetry = old })
	h := cattest.New(t, New(), cattest.Options{Config: config(f)})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if _, _, logins := f.snapshot(); len(logins) != 2 {
		t.Fatalf("logins: %+v", logins)
	}
}

func TestBadSecretFailsStart(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"app_id": "1024", "secret": "wrong", "api_base": f.URL}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "AppSecret 不正确") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetupRetriesUntilCredentialsValid(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "manual"}
	h.Answers <- map[string]any{"app_id": "1024", "secret": "wrong"}
	h.Answers <- map[string]any{"app_id": "1024", "secret": "good-secret", "sandbox": true}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["secret"] != "good-secret" || cfg["sandbox"] != true || cfg["api_base"] != f.URL {
		t.Fatalf("config: %+v", cfg)
	}
	var titles []string
	for len(h.Steps) > 0 {
		s := <-h.Steps
		titles = append(titles, s.Kind+":"+s.Title)
		if s.Kind == cat.StepForm && len(titles) > 2 && !strings.Contains(s.Description, "AppSecret 不正确") {
			t.Errorf("second form should explain the failure: %q", s.Description)
		}
	}
	if titles[len(titles)-1] != "info:已连接 小猫" {
		t.Fatalf("steps: %v", titles)
	}
}

// fakeBind stands in for q.qq.com's scan-to-bind endpoints: the first task
// expires, the second is scanned and hands back the bot's credentials.
func fakeBind(t *testing.T, appID, secret string) (tasks *[]string) {
	var mu sync.Mutex
	keys := map[string][]byte{}
	polls := map[string]int{}
	tasks = &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		defer mu.Unlock()
		reply := func(data any) {
			json.NewEncoder(w).Encode(map[string]any{"retcode": 0, "msg": "success", "data": data})
		}
		switch r.URL.Path {
		case "/lite/create_bind_task":
			id := "task-" + string(rune('1'+len(*tasks)))
			keys[id], _ = base64.StdEncoding.DecodeString(in["key"])
			*tasks = append(*tasks, id)
			reply(map[string]any{"task_id": id})
		case "/lite/poll_bind_result":
			id := in["task_id"]
			polls[id]++
			switch {
			case id == "task-1":
				reply(map[string]any{"status": 3, "bot_appid": "0"})
			case polls[id] < 2:
				reply(map[string]any{"status": 1, "bot_appid": "0"})
			default:
				block, _ := aes.NewCipher(keys[id])
				gcm, _ := cipher.NewGCM(block)
				iv := []byte("0123456789ab")
				sealed := gcm.Seal(iv, iv, []byte(secret), nil)
				reply(map[string]any{"status": 2, "bot_appid": appID, "bot_encrypt_secret": base64.StdEncoding.EncodeToString(sealed)})
			}
		}
	}))
	t.Cleanup(srv.Close)
	oldBase, oldEvery := bindBase, bindPollEvery
	bindBase, bindPollEvery = srv.URL, 5*time.Millisecond
	t.Cleanup(func() { bindBase, bindPollEvery = oldBase, oldEvery })
	return tasks
}

func TestSetupByScanning(t *testing.T) {
	f := newFake(t)
	tasks := fakeBind(t, "1024", "good-secret")
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "qrcode"}
	cfg, err := h.RunSetup(map[string]any{"api_base": f.URL})
	if err != nil {
		t.Fatal(err)
	}
	if cfg["app_id"] != "1024" || cfg["secret"] != "good-secret" {
		t.Fatalf("config: %+v", cfg)
	}
	if _, _, logins := f.snapshot(); len(logins) != 0 {
		t.Fatalf("the wizard should stay off the gateway: %+v", logins)
	}
	var codes []string
	for len(h.Steps) > 0 {
		if s := <-h.Steps; s.Kind == cat.StepQRCode {
			codes = append(codes, s.Text)
		}
	}
	// The expired code was replaced by a fresh one.
	if len(*tasks) != 2 || len(codes) != 2 || !strings.HasSuffix(codes[1], "/qqbot/openclaw/connect.html?task_id=task-2&_wv=2") {
		t.Fatalf("tasks %v, codes %v", *tasks, codes)
	}
}

func TestSandboxAndDefaultHosts(t *testing.T) {
	if b := newBot(map[string]any{}); b.base != apiBase || b.tokenURL != tokenBase {
		t.Fatalf("default: %s %s", b.base, b.tokenURL)
	}
	if b := newBot(map[string]any{"sandbox": true}); b.base != sandboxBase || b.tokenURL != tokenBase {
		t.Fatalf("sandbox: %s %s", b.base, b.tokenURL)
	}
}
