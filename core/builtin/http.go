package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dounx/cat/core/plugin"
)

func httpEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "http", Name: "HTTP 请求", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "globe",
			Description:  "向任意地址发送 HTTP 请求。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "request", Title: "发送请求",
				Description: "状态码 ≥ 400 视为失败。输出：status、body、json。",
				Params: map[string]any{
					"type":     "object",
					"required": []string{"url"},
					"x-order":  []string{"url", "method", "headers", "body"},
					"properties": map[string]any{
						"url":     map[string]any{"type": "string", "title": "地址"},
						"method":  map[string]any{"type": "string", "title": "方法", "enum": []string{"POST", "GET", "PUT", "PATCH", "DELETE"}, "default": "POST"},
						"headers": map[string]any{"type": "object", "title": "请求头"},
						"body":    map[string]any{"title": "请求体", "description": "字符串原样发送；对象按 JSON 发送。", "format": "textarea"},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &httpAction{client: &http.Client{Timeout: 60 * time.Second}}, nil
		},
	}
}

type httpAction struct {
	plugin.Base
	client *http.Client
}

func (h *httpAction) Invoke(ctx context.Context, action string, p map[string]any, _ plugin.InvokeContext) (map[string]any, error) {
	if action != "request" {
		return nil, plugin.ErrUnknownAction(action)
	}
	url := str(p, "url")
	if url == "" {
		return nil, fmt.Errorf("url is required")
	}
	method := strings.ToUpper(str(p, "method"))
	var body io.Reader
	contentType := ""
	switch b := p["body"].(type) {
	case nil:
	case string:
		if b != "" {
			body = strings.NewReader(b)
			if json.Valid([]byte(b)) {
				contentType = "application/json"
			} else {
				contentType = "text/plain; charset=utf-8"
			}
		}
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
		contentType = "application/json"
	}
	if method == "" {
		method = "POST"
		if body == nil {
			method = "GET"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "cat-event-hub")
	if hs, ok := p["headers"].(map[string]any); ok {
		for k, v := range hs {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := map[string]any{"status": resp.StatusCode, "body": string(data)}
	var parsed any
	if json.Unmarshal(data, &parsed) == nil {
		out["json"] = parsed
	}
	if resp.StatusCode >= 400 {
		return out, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 200))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
