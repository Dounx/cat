package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

// fakeFeishu stands in for the Feishu open platform: REST endpoints for token,
// bot info, chats, media, reactions and messages, plus the protobuf-over-WebSocket
// long connection that pushes queued events and collects the client's ACKs.
type fakeFeishu struct {
	*httptest.Server
	mu               sync.Mutex
	queued           []string
	sent             []map[string]any
	updated          []map[string]any
	reactionsCreated int
	reactionsDeleted int
	acks             []larkws.Response
	wsConns          int
	drop             chan struct{}
}

func newFake(t *testing.T) *fakeFeishu {
	f := &fakeFeishu{drop: make(chan struct{}, 1)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws" {
			f.serveWS(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		reply := func(v map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(v)
		}
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/tenant_access_token/internal"):
			if body["app_secret"] != "good" {
				reply(map[string]any{"code": 10014, "msg": "app secret invalid"})
				return
			}
			reply(map[string]any{"code": 0, "tenant_access_token": "t-1", "expire": 7200})
		case path == larkws.GenEndpointUri:
			if body["AppSecret"] != "good" {
				reply(map[string]any{"code": larkws.AuthFailed, "msg": "auth failed"})
				return
			}
			wsURL := "ws" + strings.TrimPrefix(f.URL, "http") + "/ws?device_id=dev1&service_id=1"
			reply(map[string]any{
				"code": larkws.OK, "msg": "ok",
				"data": larkws.Endpoint{
					Url: wsURL,
					ClientConfig: &larkws.ClientConfig{
						ReconnectCount: -1, ReconnectInterval: 1, ReconnectNonce: 0, PingInterval: 60,
					},
				},
			})
		case path == "/open-apis/bot/v3/info":
			reply(map[string]any{"code": 0, "bot": map[string]any{"open_id": "ou_bot", "app_name": "Cat"}})
		case path == "/open-apis/im/v1/chats/oc_group":
			reply(map[string]any{"code": 0, "data": map[string]any{"name": "产品群"}})
		case strings.HasPrefix(path, "/open-apis/im/v1/messages/") && strings.Contains(path, "/resources/"):
			_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n"))
		case path == "/open-apis/im/v1/images":
			reply(map[string]any{"code": 0, "data": map[string]any{"image_key": "img_k1"}})
		case path == "/open-apis/im/v1/files":
			reply(map[string]any{"code": 0, "data": map[string]any{"file_key": "file_k1"}})
		case strings.HasPrefix(path, "/open-apis/im/v1/messages/") && strings.Contains(path, "/reactions"):
			f.mu.Lock()
			if r.Method == http.MethodPost {
				f.reactionsCreated++
				f.mu.Unlock()
				reply(map[string]any{"code": 0, "data": map[string]any{"reaction_id": "react_1"}})
			} else if r.Method == http.MethodDelete {
				f.reactionsDeleted++
				f.mu.Unlock()
				reply(map[string]any{"code": 0, "data": map[string]any{"reaction_id": "react_1"}})
			} else {
				f.mu.Unlock()
				w.WriteHeader(405)
			}
		case strings.HasPrefix(path, "/open-apis/im/v1/messages/") && r.Method == http.MethodPut:
			f.mu.Lock()
			if body == nil {
				body = map[string]any{}
			}
			body["path"] = path
			f.updated = append(f.updated, body)
			f.mu.Unlock()
			reply(map[string]any{"code": 0, "data": map[string]any{"message_id": strings.TrimPrefix(path, "/open-apis/im/v1/messages/")}})
		case strings.HasPrefix(path, "/open-apis/im/v1/messages"):
			f.mu.Lock()
			body["path"] = path
			f.sent = append(f.sent, body)
			n := len(f.sent)
			f.mu.Unlock()
			reply(map[string]any{"code": 0, "data": map[string]any{"message_id": fmt.Sprintf("om_sent%d", n), "chat_id": "oc_group"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFeishu) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	ctx := r.Context()

	f.mu.Lock()
	f.wsConns++
	connNo := f.wsConns
	queued := f.queued
	f.queued = nil
	f.mu.Unlock()

	for i, payload := range queued {
		headers := larkws.Headers{}
		headers.Add(larkws.HeaderType, string(larkws.MessageTypeEvent))
		headers.Add(larkws.HeaderMessageID, fmt.Sprintf("m-%d-%d", connNo, i))
		headers.Add(larkws.HeaderTraceID, fmt.Sprintf("t-%d-%d", connNo, i))
		headers.Add(larkws.HeaderSum, "1")
		headers.Add(larkws.HeaderSeq, "0")
		raw, err := (&larkws.Frame{
			SeqID:   uint64(i + 1),
			LogID:   uint64(i + 1),
			Service: 1,
			Method:  int32(larkws.FrameTypeData),
			Headers: headers,
			Payload: []byte(payload),
		}).Marshal()
		if err != nil || conn.Write(ctx, websocket.MessageBinary, raw) != nil {
			return
		}
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-f.drop:
			conn.CloseNow()
		case <-done:
		case <-ctx.Done():
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var frame larkws.Frame
		if frame.Unmarshal(data) != nil {
			continue
		}
		if larkws.FrameType(frame.Method) == larkws.FrameTypeData {
			var resp larkws.Response
			if json.Unmarshal(frame.Payload, &resp) == nil {
				f.mu.Lock()
				f.acks = append(f.acks, resp)
				f.mu.Unlock()
			}
		}
	}
}

func (f *fakeFeishu) snapshot() (sent, updated []map[string]any, acks []larkws.Response, conns, reactCreated, reactDeleted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.sent...),
		append([]map[string]any(nil), f.updated...),
		append([]larkws.Response(nil), f.acks...),
		f.wsConns, f.reactionsCreated, f.reactionsDeleted
}

func msgEvent(id, eventBody string) string {
	return fmt.Sprintf(`{"schema":"2.0","header":{"event_id":%q,"event_type":"im.message.receive_v1","create_time":"1700000000000"},"event":%s}`, id, eventBody)
}

func TestReceiveAndReply(t *testing.T) {
	f := newFake(t)
	blobs := t.TempDir()
	f.queued = []string{
		// group message mentioning both the bot and someone else, quoting om_0
		msgEvent("e1", `{
			"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},
			"message":{"message_id":"om_1","parent_id":"om_0","chat_id":"oc_group","chat_type":"group","message_type":"text",
				"content":"{\"text\":\"@_user_1 帮 @_user_2 看看这个\"}",
				"mentions":[{"key":"@_user_1","id":{"open_id":"ou_bot"},"name":"Cat"},{"key":"@_user_2","id":{"open_id":"ou_bob"},"name":"Bob"}]}}`),
		// bot's own message: must be dropped
		msgEvent("e2", `{
			"sender":{"sender_id":{"open_id":"ou_bot"},"sender_type":"app"},
			"message":{"message_id":"om_self","chat_id":"oc_group","chat_type":"group","message_type":"text","content":"{\"text\":\"我说的\"}"}}`),
		// private chat: counts as mentioned
		msgEvent("e3", `{
			"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},
			"message":{"message_id":"om_2","chat_id":"oc_p2p","chat_type":"p2p","message_type":"text","content":"{\"text\":\"你好\"}"}}`),
		// group message without mention: mentioned=false
		msgEvent("e4", `{
			"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},
			"message":{"message_id":"om_3","chat_id":"oc_group","chat_type":"group","message_type":"text","content":"{\"text\":\"路过\"}"}}`),
		// incoming image resource: downloaded into BlobsDir
		msgEvent("e5", `{
			"sender":{"sender_id":{"open_id":"ou_alice"},"sender_type":"user"},
			"message":{"message_id":"om_img","chat_id":"oc_p2p","chat_type":"p2p","message_type":"image","content":"{\"image_key\":\"img_in_1\"}"}}`),
	}
	h := cattest.New(t, New(), cattest.Options{
		BlobsDir: blobs,
		Config:   map[string]any{"app_id": "cli_1", "app_secret": "good", "domain": f.URL},
	})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}

	got := map[string]cat.Event{}
	for range 4 {
		ev := h.NextEvent()
		got[ev.Payload["message_id"].(string)] = ev
	}

	group := got["om_1"]
	if group.Type != "message.created" || group.Payload["text"] != "帮 @Bob 看看这个" || group.Payload["mentioned"] != true ||
		group.Payload["reply_to_message_id"] != "om_0" {
		t.Fatalf("group payload: %+v", group.Payload)
	}
	if c := group.Conversation; c == nil || c.ID != "oc_group" || c.Kind != "group" || c.Name != "产品群" {
		t.Fatalf("group conversation: %+v", c)
	}
	if group.Actor == nil || group.Actor.ID != "ou_alice" || group.Actor.IsBot || group.Time == nil || group.Time.UnixMilli() != 1700000000000 {
		t.Fatalf("group actor/time: %+v %v", group.Actor, group.Time)
	}
	if p := got["om_2"]; p.Payload["mentioned"] != true || p.Conversation.Kind != "private" || p.Payload["text"] != "你好" {
		t.Fatalf("private: %+v %+v", p.Payload, p.Conversation)
	}
	if g := got["om_3"]; g.Payload["mentioned"] != false {
		t.Fatalf("passing remark counted as mention: %+v", g.Payload)
	}
	if _, self := got["om_self"]; self {
		t.Fatal("the bot heard itself")
	}

	// Incoming image attachment was downloaded into BlobsDir.
	imgEv := got["om_img"]
	atts, _ := imgEv.Payload["attachments"].([]any)
	if len(atts) != 1 {
		t.Fatalf("image attachments: %+v", imgEv.Payload["attachments"])
	}
	imgAtt := atts[0].(map[string]any)
	imgPath, _ := imgAtt["path"].(string)
	if imgAtt["kind"] != "image" || !strings.HasPrefix(imgPath, blobs) {
		t.Fatalf("downloaded image att = %+v", imgAtt)
	}

	// All 5 pushed event frames were ACKed with HTTP 200 over the WS connection.
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, _, acks, _, _, _ := f.snapshot(); len(acks) >= 5 {
			break
		}
	}
	_, _, acks, _, _, _ := f.snapshot()
	if len(acks) != 5 {
		t.Fatalf("acks = %d, want 5", len(acks))
	}
	for _, a := range acks {
		if a.StatusCode != 200 {
			t.Fatalf("ack status = %d", a.StatusCode)
		}
	}

	// typing reaction creates once (deduped), then clears when reply is invoked.
	if _, err := h.Invoke("typing", nil, &group); err != nil {
		t.Fatalf("typing 1: %v", err)
	}
	if _, err := h.Invoke("typing", nil, &group); err != nil {
		t.Fatalf("typing 2: %v", err)
	}

	// Reply quotes by default; quote=false posts to the chat; send supports markdown, files and edit.
	out, err := h.Invoke("reply", map[string]any{"text": "收到", "markdown": false}, &group)
	if err != nil || out["message_id"] != "om_sent1" {
		t.Fatalf("reply: %v %v", out, err)
	}
	if _, err := h.Invoke("reply", map[string]any{"text": "不引用", "quote": false, "markdown": false}, &group); err != nil {
		t.Fatalf("unquoted reply: %v", err)
	}
	if _, err := h.Invoke("send", map[string]any{"chat_id": "oc_group", "text": "**加粗**的通知", "markdown": true}, nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	doc := filepath.Join(t.TempDir(), "doc.txt")
	_ = os.WriteFile(doc, []byte("hello"), 0o600)
	if _, err := h.Invoke("send", map[string]any{"chat_id": "oc_group", "files": []any{imgAtt, doc}}, nil); err != nil {
		t.Fatalf("send files: %v", err)
	}
	if out, err := h.Invoke("reply", map[string]any{"text": "更新后的回复", "edit_message_id": "om_sent1", "markdown": false}, &group); err != nil || out["message_id"] != "om_sent1" {
		t.Fatalf("edit: %v %v", out, err)
	}
	if _, err := h.Invoke("send", map[string]any{"chat_id": "oc_group", "text": "  "}, nil); err == nil {
		t.Error("an empty message was sent")
	}
	if _, err := h.Invoke("send", map[string]any{"text": "hi"}, nil); err == nil {
		t.Error("send without chat_id should fail")
	}
	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Error("reply without a triggering event should fail")
	}

	sent, updated, _, _, rc, rd := f.snapshot()
	if len(sent) != 5 {
		t.Fatalf("sent %d messages: %+v", len(sent), sent)
	}
	if s := sent[0]; s["path"] != "/open-apis/im/v1/messages/om_1/reply" || s["msg_type"] != "text" || !strings.Contains(s["content"].(string), "收到") {
		t.Errorf("reply was sent as %+v", s)
	}
	if s := sent[1]; s["path"] != "/open-apis/im/v1/messages" || s["receive_id"] != "oc_group" || s["msg_type"] != "text" {
		t.Errorf("unquoted reply was sent as %+v", s)
	}
	if s := sent[2]; s["path"] != "/open-apis/im/v1/messages" || s["receive_id"] != "oc_group" || s["msg_type"] != "post" || !strings.Contains(s["content"].(string), "加粗") {
		t.Errorf("markdown was sent as %+v", s)
	}
	if sent[3]["msg_type"] != "image" || sent[4]["msg_type"] != "file" {
		t.Errorf("uploaded messages = %+v", sent[3:])
	}
	if rc != 1 || rd != 1 || len(updated) != 1 || updated[0]["msg_type"] != "text" || !strings.Contains(updated[0]["content"].(string), "更新后的回复") {
		t.Errorf("typing/edit state: created=%d deleted=%d updated=%+v", rc, rd, updated)
	}
}

func TestReconnectsAfterDrop(t *testing.T) {
	f := newFake(t)
	f.queued = []string{
		msgEvent("e1", `{"sender":{"sender_id":{"open_id":"ou_a"},"sender_type":"user"},"message":{"message_id":"om_before","chat_id":"oc_p2p","chat_type":"p2p","message_type":"text","content":"{\"text\":\"before\"}"}}`),
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"app_id": "cli_1", "app_secret": "good", "domain": f.URL}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if ev := h.NextEvent(); ev.Payload["message_id"] != "om_before" {
		t.Fatalf("before drop: %+v", ev.Payload)
	}

	f.mu.Lock()
	f.queued = []string{
		msgEvent("e2", `{"sender":{"sender_id":{"open_id":"ou_a"},"sender_type":"user"},"message":{"message_id":"om_after","chat_id":"oc_p2p","chat_type":"p2p","message_type":"text","content":"{\"text\":\"after\"}"}}`),
	}
	f.mu.Unlock()
	f.drop <- struct{}{}

	if ev := h.NextEvent(); ev.Payload["message_id"] != "om_after" {
		t.Fatalf("after reconnect: %+v", ev.Payload)
	}
	if _, _, _, conns, _, _ := f.snapshot(); conns < 2 {
		t.Fatalf("wsConns = %d, want >= 2", conns)
	}
}

func TestBadConfigFailsStart(t *testing.T) {
	f := newFake(t)
	if err := cattest.New(t, New(), cattest.Options{Config: map[string]any{"app_id": "cli_1", "app_secret": "wrong", "domain": f.URL}}).Start(); err == nil {
		t.Fatal("a wrong secret passed")
	}
	if err := cattest.New(t, New(), cattest.Options{Config: map[string]any{"app_id": "cli_1"}}).Start(); err == nil {
		t.Fatal("a bot without a secret passed")
	}
}

func TestSetupManual(t *testing.T) {
	f := newFake(t)
	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "manual"}
	h.Answers <- map[string]any{"app_id": "cli_1", "app_secret": "wrong", "domain": f.URL}
	h.Answers <- map[string]any{"method": "manual"}
	h.Answers <- map[string]any{"app_id": "cli_1", "app_secret": "good", "domain": f.URL}
	cfg, err := h.RunSetup(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["app_id"] != "cli_1" || cfg["app_secret"] != "good" || cfg["domain"] != f.URL {
		t.Fatalf("config: %+v", cfg)
	}
	var forms []cat.Step
	var last cat.Step
	for len(h.Steps) > 0 {
		s := <-h.Steps
		last = s
		if s.Kind == cat.StepForm {
			forms = append(forms, s)
		}
	}
	if len(forms) != 4 || !strings.HasPrefix(forms[2].Description, "⚠ ") {
		t.Fatalf("expected error on retry form: %+v", forms)
	}
	if last.Kind != cat.StepInfo || last.Title != "已连接「Cat」" {
		t.Fatalf("final step: %+v", last)
	}
}

func TestSetupByScanning(t *testing.T) {
	f := newFake(t)

	// Point the built-in feishu/lark edition endpoints at fakeFeishu so the
	// wizard can verify the credentials returned by registration.
	oldFeishu, oldLark := bases["feishu"], bases["lark"]
	bases["feishu"], bases["lark"] = f.URL, f.URL
	t.Cleanup(func() { bases["feishu"], bases["lark"] = oldFeishu, oldLark })

	// Fake registration servers: attempt 1 denies access (wizard retries);
	// attempt 2 reports a Lark tenant so the SDK switches to larkReg and finishes.
	var mu sync.Mutex
	attempts := 0
	larkPolled := false
	larkReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		larkPolled = true
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{
			"client_id":     "cli_1",
			"client_secret": "good",
			"user_info":     map[string]any{"open_id": "ou_owner", "tenant_brand": "lark"},
		})
	}))
	t.Cleanup(larkReg.Close)

	feishuReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		switch r.FormValue("action") {
		case "begin":
			attempts++
			json.NewEncoder(w).Encode(map[string]any{
				"device_code":               fmt.Sprintf("dev-%d", attempts),
				"verification_uri_complete": fmt.Sprintf("https://accounts.feishu.test/register?code=dev-%d", attempts),
				"interval":                  1,
				"expire_in":                 60,
			})
		case "poll":
			if r.FormValue("device_code") == "dev-1" {
				json.NewEncoder(w).Encode(map[string]any{"error": "access_denied", "error_description": "user cancelled"})
				return
			}
			// Switch to larkReg on the first poll of dev-2 (no sleep before re-poll).
			json.NewEncoder(w).Encode(map[string]any{"user_info": map[string]any{"tenant_brand": "lark"}})
		}
	}))
	t.Cleanup(feishuReg.Close)

	oldReg, oldLarkReg := registerDomain, registerLarkDomain
	registerDomain, registerLarkDomain = feishuReg.URL, larkReg.URL
	t.Cleanup(func() { registerDomain, registerLarkDomain = oldReg, oldLarkReg })

	h := cattest.New(t, New(), cattest.Options{})
	h.Answers <- map[string]any{"method": "qrcode"}
	h.Answers <- map[string]any{"method": "qrcode"}
	cfg, err := h.RunSetup(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["app_id"] != "cli_1" || cfg["app_secret"] != "good" || cfg["domain"] != "lark" {
		t.Fatalf("config: %+v", cfg)
	}
	mu.Lock()
	switched := larkPolled
	mu.Unlock()
	if !switched {
		t.Fatal("registration did not switch to Lark domain")
	}
	var codes []string
	var retryDesc string
	for len(h.Steps) > 0 {
		s := <-h.Steps
		if s.Kind == cat.StepQRCode {
			codes = append(codes, s.URL)
		}
		if s.Kind == cat.StepForm && strings.Contains(s.Description, "user cancelled") {
			retryDesc = s.Description
		}
	}
	if len(codes) != 2 || !strings.Contains(codes[0], "source=go-sdk%2Fcat") || !strings.Contains(codes[1], "code=dev-2") {
		t.Fatalf("qr codes: %v", codes)
	}
	if retryDesc == "" {
		t.Fatal("expected retry form to report the scan failure")
	}
}
