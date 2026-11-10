package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

const (
	testToken = "QDG6eK"
	testKey   = "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C"
	testCorp  = "wx5823bf96d3bd56c7"
)

// The vector published in WeCom's callback documentation: a known ciphertext,
// its signature and the plaintext it must decrypt to.
func TestOfficialVector(t *testing.T) {
	c, err := newCodec(testToken, testKey, testCorp)
	if err != nil {
		t.Fatal(err)
	}
	encrypted := "RypEvHKD8QQKFhvQ6QleEB4J58tiPdvo+rtK1I9qca6aM/wvqnLSV5zEPeusUiX5L5X/0lWfrf0QADHHhGd3QczcdCUpj911L3vg3W/sYYvuJTs3TUUkSUXxaccAS0qhxchrRYt66wiSpGLYL42aM6A8dTT+6k4aSknmPj48kzJs8qLjvd4Xgpue06DOdnLxAUHzM6+kDZ+HMZfJYuR+LtwGc2hgf5gsijff0ekUNXZiqATP7PF5mZxZ3Izoun1s4zG4LUMnvw2r+KqCKIw+3IQH03v+BCA9nMELNqbSf6tiWSrXJB3LAVGUcallcrw8V2t9EL4EhzJWrQUax5wLVMNS0+rUPA3k22Ncx4XXZS9o0MBH27Bo6BpNelZpS+/uh9KsNlY6bHCmJU9p8g7m3fVKn28H3KDYA5Pl/T8Z1ptDAVe0lXdQ2YoyyH2uyPIGHBZZIs2pDBS8R07+qN+E7Q=="
	if got := c.signature("1409659813", "1372623149", encrypted); got != "477715d11cdb4164915debcba66cb864d751f3e6" {
		t.Fatalf("signature = %s", got)
	}
	plain, err := c.decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(plain), "<Content><![CDATA[hello]]></Content>") || !strings.Contains(string(plain), "<FromUserName><![CDATA[mycreate]]></FromUserName>") {
		t.Fatalf("plaintext: %s", plain)
	}
}

func fakeWeCom(t *testing.T) (*httptest.Server, func() []map[string]any) {
	var mu sync.Mutex
	var sent []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cgi-bin/gettoken":
			if r.URL.Query().Get("corpsecret") != "s3" {
				w.Write([]byte(`{"errcode":40001,"errmsg":"invalid credential"}`))
				return
			}
			w.Write([]byte(`{"errcode":0,"access_token":"tok","expires_in":7200}`))
		case "/cgi-bin/message/send":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["_token"] = r.URL.Query().Get("access_token")
			mu.Lock()
			sent = append(sent, body)
			mu.Unlock()
			w.Write([]byte(`{"errcode":0,"msgid":"m1"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []map[string]any { mu.Lock(); defer mu.Unlock(); return append([]map[string]any(nil), sent...) }
}

func TestCallbackAndReply(t *testing.T) {
	srv, sent := fakeWeCom(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"corp_id": testCorp, "agent_id": "1000002", "secret": "s3", "token": testToken, "aes_key": testKey, "api_base": srv.URL,
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	c, _ := newCodec(testToken, testKey, testCorp)
	random := []byte("0123456789abcdef")
	query := func(enc string) map[string][]string {
		return map[string][]string{"msg_signature": {c.signature("1700000000", "n1", enc)}, "timestamp": {"1700000000"}, "nonce": {"n1"}}
	}

	// URL verification: the decrypted echostr is returned verbatim.
	echo := c.encrypt([]byte("verify-me"), random)
	qv := query(echo)
	qv["echostr"] = []string{echo}
	resp, err := h.Ingress(cat.IngressRequest{Method: "GET", Path: "/", Query: qv})
	if err != nil || resp.Status != 200 || string(resp.Body) != "verify-me" {
		t.Fatalf("verification: %+v %v", resp, err)
	}

	// An encrypted text message becomes an event.
	msg := `<xml><ToUserName><![CDATA[` + testCorp + `]]></ToUserName><FromUserName><![CDATA[zhangsan]]></FromUserName><CreateTime>1700000000</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[部署一下]]></Content><MsgId>987</MsgId><AgentID>1000002</AgentID></xml>`
	enc := c.encrypt([]byte(msg), random)
	body := []byte(`<xml><ToUserName><![CDATA[` + testCorp + `]]></ToUserName><Encrypt><![CDATA[` + enc + `]]></Encrypt><AgentID><![CDATA[1000002]]></AgentID></xml>`)
	post := cat.IngressRequest{Method: "POST", Path: "/", Query: query(enc), Body: body}
	resp, err = h.Ingress(post)
	if err != nil || resp.Status != 200 || string(resp.Body) != "success" {
		t.Fatalf("callback: %+v %v", resp, err)
	}
	ev := h.NextEvent()
	if ev.Type != "message.created" || ev.Actor.ID != "zhangsan" || ev.Payload["text"] != "部署一下" || ev.Payload["message_id"] != "987" {
		t.Fatalf("event: %+v %+v", ev, ev.Payload)
	}
	// WeCom's retry of the same message is acknowledged but not emitted again.
	if resp, _ = h.Ingress(post); resp.Status != 200 || len(h.Events) != 0 {
		t.Fatalf("retry: status %d, %d extra events", resp.Status, len(h.Events))
	}

	// A forged signature and a tampered body are both refused.
	bad := query(enc)
	bad["msg_signature"] = []string{strings.Repeat("0", 40)}
	if resp, _ = h.Ingress(cat.IngressRequest{Method: "POST", Query: bad, Body: body}); resp.Status != 403 {
		t.Fatalf("forged signature accepted: %d", resp.Status)
	}
	if len(h.Events) != 0 {
		t.Fatal("a forged callback must not produce an event")
	}

	out, err := h.Invoke("reply", map[string]any{"text": "好的"}, &ev)
	if err != nil || out["msgid"] != "m1" {
		t.Fatalf("reply: %v %v", out, err)
	}
	s := sent()[0]
	if s["touser"] != "zhangsan" || s["agentid"] != float64(1000002) || s["text"].(map[string]any)["content"] != "好的" || s["_token"] != "tok" {
		t.Fatalf("send: %+v", s)
	}
	// Long text is split on UTF-8 boundaries within the byte limit.
	if _, err := h.Invoke("send", map[string]any{"to_user": "lisi", "text": strings.Repeat("猫", 1500)}, nil); err != nil {
		t.Fatal(err)
	}
	all := sent()
	if len(all) != 4 {
		t.Fatalf("want 3 parts for the long text, got %d sends", len(all)-1)
	}
	for _, m := range all[1:] {
		part := m["text"].(map[string]any)["content"].(string)
		if len(part) > maxBytes || strings.ContainsRune(part, '�') {
			t.Fatalf("bad split: %d bytes", len(part))
		}
	}
}

func TestWrongSecretFailsStart(t *testing.T) {
	srv, _ := fakeWeCom(t)
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"corp_id": testCorp, "agent_id": "1", "secret": "nope", "token": testToken, "aes_key": testKey, "api_base": srv.URL,
	}})
	if err := h.Start(); err == nil || !strings.Contains(err.Error(), "拒绝了凭据") {
		t.Fatalf("err = %v", err)
	}
}
