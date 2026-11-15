package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dounx/cat/plugins/internal/mailtest"
	"github.com/dounx/cat/sdk/go/cattest"
)

const incoming = `From: =?utf-8?b?5byg5LiJ?= <Ann@Example.com>
To: cat@mail.test, bob@example.com
Cc: carol@example.com
Subject: =?utf-8?b?5ZGo5oql?=
Message-ID: <m1@example.com>
References: <m0@example.com>
Date: Fri, 13 Nov 2026 10:00:00 +0800
MIME-Version: 1.0
Content-Type: multipart/mixed; boundary=b1

--b1
Content-Type: multipart/alternative; boundary=b2

--b2
Content-Type: text/plain; charset=gbk
Content-Transfer-Encoding: base64

1eLW3MfrsunK1aGj

--b2
Content-Type: text/html; charset=utf-8

<p>ignored when there is plain text</p>
--b2--
--b1
Content-Type: application/pdf
Content-Disposition: attachment; filename="report.pdf"

%PDF
--b1--
`

func TestReceiveReplyAndSend(t *testing.T) {
	srv, err := mailtest.Start("127.0.0.1:0", "127.0.0.1:0", "cat@mail.test", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// what is in the mailbox before the plugin first looks is left alone
	if err := srv.Deliver("From: old@example.com\nSubject: old\n\nbefore\n"); err != nil {
		t.Fatal(err)
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{
		"address": "Cat@mail.test", "password": "secret", "name": "猫", "imap": srv.IMAP, "smtp": srv.SMTP,
		"insecure": true, "interval": 5.0,
	}})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Deliver(incoming); err != nil {
		t.Fatal(err)
	}
	ev := h.NextEvent()
	if ev.Type != "message.created" || ev.Conversation.ID != "ann@example.com" || ev.Conversation.Kind != "private" || ev.Actor.Name != "张三" {
		t.Fatalf("event: %+v %+v %+v", ev, ev.Conversation, ev.Actor)
	}
	p := ev.Payload
	if p["subject"] != "周报" || p["body"] != "这周请查收。" || p["text"] != "周报\n\n这周请查收。" || p["message_id"] != "m1@example.com" || p["mentioned"] != true {
		t.Fatalf("payload: %+v", p)
	}
	if to := p["to"].([]any); len(to) != 2 || p["cc"].([]any)[0] != "carol@example.com" || p["attachments"].([]any)[0].(map[string]any)["name"] != "report.pdf" {
		t.Fatalf("recipients and attachments: %+v", p)
	}
	if ev.Time == nil || ev.Time.Unix() != time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("time: %v", ev.Time)
	}

	out, err := h.Invoke("reply", map[string]any{"text": "收到，谢谢。\n第二行", "reply_all": true}, &ev)
	if err != nil {
		t.Fatal(err)
	}
	sent := srv.Sent()
	if len(sent) != 1 || sent[0].From != "cat@mail.test" || strings.Join(sent[0].To, ",") != "ann@example.com,bob@example.com,carol@example.com" {
		t.Fatalf("reply went as %+v", sent)
	}
	reply, err := read([]byte(sent[0].Data))
	if err != nil {
		t.Fatal(err)
	}
	if reply.subject != "Re: 周报" || reply.body != "收到，谢谢。\n第二行" || reply.from.Name != "猫" || !reply.ours ||
		reply.inReplyTo[0] != "m1@example.com" || strings.Join(reply.references, ",") != "m0@example.com,m1@example.com" || reply.id != out["message_id"] {
		t.Fatalf("reply reads as %+v", reply)
	}

	if _, err := h.Invoke("send", map[string]any{"to": "dan@example.com, Eve@Example.com", "subject": "通知", "text": "一封新邮件"}, nil); err != nil {
		t.Fatal(err)
	}
	if sent = srv.Sent(); len(sent) != 2 || strings.Join(sent[1].To, ",") != "dan@example.com,eve@example.com" {
		t.Fatalf("mail went as %+v", sent)
	}
	if _, err := h.Invoke("send", map[string]any{"to": "nobody", "text": "x"}, nil); err == nil {
		t.Error("a mail to no address was sent")
	}
	if _, err := h.Invoke("reply", map[string]any{"text": "x"}, nil); err == nil {
		t.Error("a reply to nothing was sent")
	}

	// The test server puts what it is sent into the same mailbox. The plugin
	// knows its own mail and lets it pass; mail from elsewhere after it still
	// comes through, marked when a machine sent it.
	if err := srv.Deliver("From: news@example.com\nSubject: digest\nList-Id: <news.example.com>\nContent-Type: text/html\n\n<h1>Hi</h1><p>one<br>two</p><script>x()</script>\n"); err != nil {
		t.Fatal(err)
	}
	next := h.NextEvent()
	if next.Payload["subject"] != "digest" || next.Payload["automatic"] != true || next.Payload["body"] != "Hi\none\ntwo" {
		t.Fatalf("after its own mail: %+v", next.Payload)
	}
	// how far it got is written down once the event is out
	for deadline := time.Now().Add(2 * time.Second); !strings.HasSuffix(h.State("seen"), ":5"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("seen: %q", h.State("seen"))
		}
	}
}

func TestWrongPasswordAndServers(t *testing.T) {
	srv, err := mailtest.Start("127.0.0.1:0", "127.0.0.1:0", "cat@mail.test", "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	cfg := map[string]any{"address": "cat@mail.test", "password": "wrong", "imap": srv.IMAP, "smtp": srv.SMTP, "insecure": true}
	if err := cattest.New(t, New(), cattest.Options{Config: cfg}).Start(); err == nil || !strings.Contains(err.Error(), "IMAP 登录失败") {
		t.Fatalf("start with a wrong password: %v", err)
	}
	cfg["password"] = "secret"
	if err := verify(t.Context(), cfg); err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, bad := range []map[string]any{
		{"address": "nope", "password": "x", "imap": "a:1", "smtp": "b:2"},
		{"address": "a@b.c", "password": "x", "imap": "imap.b.c", "smtp": "b:2"},
		{"address": "a@b.c", "imap": "a:1", "smtp": "b:2"},
	} {
		if _, err := newAccount(bad); err == nil {
			t.Errorf("account %v was made", bad)
		}
	}
	if p := guess("someone@QQ.com"); p.imap != "imap.qq.com:993" || p.smtp != "smtp.qq.com:465" || p.note == "" {
		t.Errorf("qq: %+v", p)
	}
	if p := guess("me@corp.example"); p.imap != "imap.corp.example:993" || p.smtp != "smtp.corp.example:465" {
		t.Errorf("own domain: %+v", p)
	}
}
