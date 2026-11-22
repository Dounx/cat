package builtin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/plugin"
)

const (
	defaultWebhookTimeoutSec = 30
	minWebhookTimeoutSec     = 1
	maxWebhookTimeoutSec     = 300
)

func webhookEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "webhook", Name: "Webhook", Version: "1.0.0", Builtin: true, Ingress: true, Icon: "webhook",
			Description:  "每个 Webhook 有自己的接收地址，发到这个地址的任何 HTTP 请求都会变成一个事件；开启同步响应后，可等待规则回复自定义内容。",
			Capabilities: []string{plugin.CapSource, plugin.CapAction},
			Events:       []plugin.EventSpec{{Type: "webhook.received", Title: "收到请求"}},
			ConfigSchema: map[string]any{
				"type":    "object",
				"x-order": []string{"secret", "event_type", "sync_response", "response_timeout"},
				"properties": map[string]any{
					"secret": map[string]any{
						"type": "string", "title": "密钥（可选）", "format": "password",
						"description": "设置后，请求须带 X-Cat-Secret 头或 ?secret= 参数，或使用该密钥生成的 X-Hub-Signature-256 签名。",
					},
					"event_type": map[string]any{
						"type": "string", "title": "事件类型", "default": "webhook.received",
					},
					"sync_response": map[string]any{
						"type":        "boolean",
						"title":       "等待规则返回响应",
						"default":     false,
						"description": "开启后，收到请求时会等待规则里的「回复请求」动作返回自定义状态码和内容，而不是立即返回 202。",
					},
					"response_timeout": map[string]any{
						"type":        "number",
						"title":       "等待超时（秒）",
						"default":     defaultWebhookTimeoutSec,
						"description": "同步等待模式下的最长等待时间（1~300 秒），超时后返回 504。",
					},
				},
			},
			Actions: []plugin.ActionSpec{{
				Name:        "respond",
				Title:       "回复请求",
				Description: "把自定义状态码、响应头和响应体返回给触发本次运行的 Webhook 调用方。",
				Params: map[string]any{
					"type":    "object",
					"x-order": []string{"status", "body", "content_type", "headers"},
					"properties": map[string]any{
						"status": map[string]any{
							"type":    "number",
							"title":   "状态码",
							"default": 200,
						},
						"body": map[string]any{
							"title":       "响应体",
							"format":      "textarea",
							"default":     "{{ payload.text }}",
							"description": "字符串原样返回；对象或合法 JSON 按 application/json 返回。",
						},
						"content_type": map[string]any{
							"type":        "string",
							"title":       "Content-Type（可选）",
							"description": "留空时自动判断：合法 JSON 用 application/json，否则用 text/plain; charset=utf-8。",
						},
						"headers": map[string]any{
							"type":  "object",
							"title": "响应头（可选）",
						},
					},
				},
			}},
		},
		Factory: func(cfg map[string]any) (plugin.Instance, error) {
			t := str(cfg, "event_type")
			if t == "" {
				t = "webhook.received"
			}
			syncResp, _ := cfg["sync_response"].(bool)
			timeoutSec := float64(defaultWebhookTimeoutSec)
			if _, ok := cfg["response_timeout"]; ok {
				timeoutSec = num(cfg, "response_timeout", defaultWebhookTimeoutSec)
				if timeoutSec < minWebhookTimeoutSec {
					timeoutSec = minWebhookTimeoutSec
				} else if timeoutSec > maxWebhookTimeoutSec {
					timeoutSec = maxWebhookTimeoutSec
				}
			}
			return &webhook{
				secret:       str(cfg, "secret"),
				eventType:    t,
				syncResponse: syncResp,
				timeout:      time.Duration(timeoutSec * float64(time.Second)),
				pending:      map[string]chan plugin.IngressResponse{},
			}, nil
		},
	}
}

type webhook struct {
	plugin.Base
	secret, eventType string
	syncResponse      bool
	timeout           time.Duration
	host              plugin.Host

	mu      sync.Mutex
	pending map[string]chan plugin.IngressResponse
	stopped bool
}

func (w *webhook) Start(_ context.Context, h plugin.Host) error {
	w.host = h
	return nil
}

func (w *webhook) Stop() error {
	w.mu.Lock()
	w.stopped = true
	pending := w.pending
	w.pending = nil
	w.mu.Unlock()

	resp := jsonErrResp(503, "webhook stopped")
	for _, ch := range pending {
		select {
		case ch <- resp:
		default:
		}
	}
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

func (w *webhook) HandleIngress(ctx context.Context, req plugin.IngressRequest) (plugin.IngressResponse, error) {
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
	ev := &event.Event{
		Type:    w.eventType,
		Payload: payload,
		Raw:     map[string]any{"method": req.Method, "path": req.Path, "query": query, "headers": headers, "body": body},
	}

	if !w.syncResponse {
		evID, err := w.host.Emit(ev)
		if err != nil {
			return plugin.IngressResponse{Status: 500}, err
		}
		out, _ := json.Marshal(map[string]string{"id": evID})
		return plugin.IngressResponse{Status: 202, Headers: map[string]string{"Content-Type": "application/json"}, Body: out}, nil
	}

	ev.ID = id.New()
	ch := make(chan plugin.IngressResponse, 1)
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return jsonErrResp(503, "webhook stopped"), nil
	}
	if w.pending == nil {
		w.pending = map[string]chan plugin.IngressResponse{}
	}
	w.pending[ev.ID] = ch
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		if w.pending != nil {
			delete(w.pending, ev.ID)
		}
		w.mu.Unlock()
	}()

	if _, err := w.host.Emit(ev); err != nil {
		return plugin.IngressResponse{Status: 500}, err
	}

	timeout := w.timeout
	if timeout <= 0 {
		timeout = defaultWebhookTimeoutSec * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case resp := <-ch:
		return resp, nil
	case <-timer.C:
		return jsonErrResp(504, "webhook response timeout"), nil
	case <-ctx.Done():
		return jsonErrResp(499, "request cancelled"), nil
	}
}

func (w *webhook) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "respond" {
		return nil, plugin.ErrUnknownAction(action)
	}
	status, err := parseWebhookStatus(p["status"])
	if err != nil {
		return nil, err
	}
	bodyBytes, autoCT, err := encodeWebhookBody(p["body"])
	if err != nil {
		return nil, err
	}
	headers, err := parseWebhookHeaders(p["headers"])
	if err != nil {
		return nil, err
	}
	ct := strings.TrimSpace(str(p, "content_type"))
	if ct != "" {
		deleteHeaderFold(headers, "Content-Type")
		headers["Content-Type"] = ct
	} else if autoCT != "" && !hasHeaderFold(headers, "Content-Type") {
		headers["Content-Type"] = autoCT
	}

	if ictx.Event == nil || ictx.Event.ID == "" {
		return nil, errors.New("没有等待中的请求（可能已回复、已超时，或该 Webhook 未开启同步响应）")
	}
	evID := ictx.Event.ID

	w.mu.Lock()
	var ch chan plugin.IngressResponse
	if w.pending != nil {
		ch = w.pending[evID]
		delete(w.pending, evID)
	}
	w.mu.Unlock()

	if ch == nil {
		return nil, errors.New("没有等待中的请求（可能已回复、已超时，或该 Webhook 未开启同步响应）")
	}

	resp := plugin.IngressResponse{
		Status:  status,
		Headers: headers,
		Body:    bodyBytes,
	}
	select {
	case ch <- resp:
	default:
		return nil, errors.New("没有等待中的请求（可能已回复或已超时）")
	}

	return map[string]any{
		"responded": true,
		"status":    status,
		"event_id":  evID,
	}, nil
}

func jsonErrResp(status int, msg string) plugin.IngressResponse {
	out, _ := json.Marshal(map[string]string{"error": msg})
	return plugin.IngressResponse{
		Status:  status,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    out,
	}
}

func parseWebhookStatus(v any) (int, error) {
	if v == nil {
		return 200, nil
	}
	var status int
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, fmt.Errorf("无效的状态码：%v（须为 100~599 的整数）", v)
		}
		status = int(n)
	case int:
		status = n
	case int64:
		status = int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("无效的状态码：%v", v)
		}
		status = int(i)
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 200, nil
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("无效的状态码：%q（须为 100~599 的整数）", s)
		}
		status = i
	default:
		return 0, fmt.Errorf("无效的状态码：%v", v)
	}
	if status < 100 || status > 599 {
		return 0, fmt.Errorf("无效的状态码：%d（须为 100~599）", status)
	}
	return status, nil
}

func encodeWebhookBody(v any) ([]byte, string, error) {
	switch b := v.(type) {
	case nil:
		return nil, "", nil
	case string:
		if b == "" {
			return nil, "", nil
		}
		raw := []byte(b)
		if json.Valid(raw) {
			return raw, "application/json", nil
		}
		return raw, "text/plain; charset=utf-8", nil
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, "", fmt.Errorf("响应体序列化失败：%w", err)
		}
		return raw, "application/json", nil
	}
}

func parseWebhookHeaders(v any) (map[string]string, error) {
	out := map[string]string{}
	switch h := v.(type) {
	case nil:
		return out, nil
	case map[string]any:
		for k, val := range h {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = fmt.Sprint(val)
			}
		}
		return out, nil
	case map[string]string:
		for k, val := range h {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = val
			}
		}
		return out, nil
	case string:
		s := strings.TrimSpace(h)
		if s == "" {
			return out, nil
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(s), &parsed); err != nil {
			return nil, errors.New("响应头须为合法的 JSON 对象")
		}
		for k, val := range parsed {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = fmt.Sprint(val)
			}
		}
		return out, nil
	default:
		return nil, errors.New("响应头须为 JSON 对象")
	}
}

func hasHeaderFold(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

func deleteHeaderFold(headers map[string]string, name string) {
	for k := range headers {
		if strings.EqualFold(k, name) {
			delete(headers, k)
		}
	}
}
