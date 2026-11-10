package builtin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

func webhookEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "webhook", Name: "Webhook", Version: "1.0.0", Builtin: true, Ingress: true, Icon: "webhook",
			Description:  "每个 Webhook 有自己的接收地址，发到这个地址的任何 HTTP 请求都会变成一个事件。",
			Capabilities: []string{plugin.CapSource},
			Events:       []plugin.EventSpec{{Type: "webhook.received", Title: "收到请求"}},
			ConfigSchema: map[string]any{
				"type":    "object",
				"x-order": []string{"secret", "event_type"},
				"properties": map[string]any{
					"secret": map[string]any{
						"type": "string", "title": "密钥（可选）", "format": "password",
						"description": "设置后，请求须带 X-Cat-Secret 头或 ?secret= 参数，或使用该密钥生成的 X-Hub-Signature-256 签名。",
					},
					"event_type": map[string]any{
						"type": "string", "title": "事件类型", "default": "webhook.received",
					},
				},
			},
		},
		Factory: func(cfg map[string]any) (plugin.Instance, error) {
			t := str(cfg, "event_type")
			if t == "" {
				t = "webhook.received"
			}
			return &webhook{secret: str(cfg, "secret"), eventType: t}, nil
		},
	}
}

type webhook struct {
	plugin.Base
	secret, eventType string
	host              plugin.Host
}

func (w *webhook) Start(_ context.Context, h plugin.Host) error {
	w.host = h
	return nil
}

func (w *webhook) authorized(req plugin.IngressRequest) bool {
	if w.secret == "" {
		return true
	}
	eq := func(a string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(w.secret)) == 1 }
	if eq(req.Headers["X-Cat-Secret"]) {
		return true
	}
	if q := req.Query["secret"]; len(q) > 0 && eq(q[0]) {
		return true
	}
	if sig := strings.TrimPrefix(req.Headers["X-Hub-Signature-256"], "sha256="); sig != "" {
		mac := hmac.New(sha256.New, []byte(w.secret))
		mac.Write(req.Body)
		want := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(sig), []byte(want))
	}
	return false
}

func (w *webhook) HandleIngress(_ context.Context, req plugin.IngressRequest) (plugin.IngressResponse, error) {
	if !w.authorized(req) {
		return plugin.IngressResponse{Status: 401, Body: []byte(`{"error":"unauthorized"}`)}, nil
	}
	payload := map[string]any{}
	body := string(req.Body)
	var parsed any
	if len(req.Body) > 0 && json.Unmarshal(req.Body, &parsed) == nil {
		if obj, ok := parsed.(map[string]any); ok {
			payload = obj
		} else {
			payload["data"] = parsed
		}
	} else if body != "" {
		payload["text"] = body
	}
	headers := map[string]string{}
	for k, v := range req.Headers {
		if k != "X-Cat-Secret" && k != "Authorization" && k != "Cookie" {
			headers[k] = v
		}
	}
	query := map[string][]string{}
	for k, v := range req.Query {
		if k != "secret" {
			query[k] = v
		}
	}
	id, err := w.host.Emit(&event.Event{
		Type:    w.eventType,
		Payload: payload,
		Raw:     map[string]any{"method": req.Method, "path": req.Path, "query": query, "headers": headers, "body": body},
	})
	if err != nil {
		return plugin.IngressResponse{Status: 500}, err
	}
	out, _ := json.Marshal(map[string]string{"id": id})
	return plugin.IngressResponse{Status: 202, Headers: map[string]string{"Content-Type": "application/json"}, Body: out}, nil
}
