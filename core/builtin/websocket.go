package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

const (
	wsDialTimeout  = 15 * time.Second
	wsPingInterval = 30 * time.Second
	wsMaxBackoff   = 30 * time.Second
	wsReadLimit    = 4 << 20
)

func websocketEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: "websocket", Name: "Websocket", Version: "1.0.0", Builtin: true, Icon: "cable",
			Description:  "连接到一个 WebSocket 地址并保持在线，收到的每条消息都会变成一个事件，也可以向它发送消息。",
			Capabilities: []string{plugin.CapSource, plugin.CapAction},
			Events:       []plugin.EventSpec{{Type: "websocket.message", Title: "收到消息"}},
			ConfigSchema: map[string]any{
				"type":     "object",
				"required": []string{"url"},
				"x-order":  []string{"url", "token", "headers", "event_type"},
				"properties": map[string]any{
					"url": map[string]any{
						"type": "string", "title": "地址",
						"description": "以 ws:// 或 wss:// 开头。断线后会自动重连。",
					},
					"token": map[string]any{
						"type": "string", "title": "令牌（可选）", "format": "password",
						"description": "设置后，连接时带上 Authorization: Bearer 头。",
					},
					"headers": map[string]any{"type": "object", "title": "请求头（可选）"},
					"event_type": map[string]any{
						"type": "string", "title": "事件类型", "default": "websocket.message",
					},
				},
			},
			Actions: []plugin.ActionSpec{{
				Name: "send", Title: "发送消息",
				Description: "连接断开时失败。",
				Params: map[string]any{
					"type":     "object",
					"required": []string{"message"},
					"properties": map[string]any{
						"message": map[string]any{"title": "消息", "description": "字符串原样发送；对象按 JSON 发送。", "format": "textarea"},
					},
				},
			}},
		},
		Factory: func(cfg map[string]any) (plugin.Instance, error) {
			raw := str(cfg, "url")
			u, err := url.Parse(raw)
			if raw == "" || err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
				return nil, fmt.Errorf("地址须以 ws:// 或 wss:// 开头")
			}
			header := http.Header{}
			if hs, ok := cfg["headers"].(map[string]any); ok {
				for k, v := range hs {
					header.Set(k, fmt.Sprint(v))
				}
			}
			if t := str(cfg, "token"); t != "" {
				header.Set("Authorization", "Bearer "+t)
			}
			t := str(cfg, "event_type")
			if t == "" {
				t = "websocket.message"
			}
			return &wsClient{url: raw, header: header, eventType: t}, nil
		},
	}
}

// wsClient keeps one outbound WebSocket connection open for as long as the
// instance runs.
type wsClient struct {
	plugin.Base
	url, eventType string
	header         http.Header
	host           plugin.Host

	mu   sync.Mutex
	conn *websocket.Conn
}

// Start makes the first connection attempt itself, so a reachable server is
// connected by the time the instance shows as running. Either way the
// connection is then kept up in the background.
func (w *wsClient) Start(ctx context.Context, h plugin.Host) error {
	w.host = h
	conn, err := w.dial(ctx)
	if err != nil && ctx.Err() != nil {
		return err
	}
	go w.run(ctx, conn, err)
	return nil
}

func (w *wsClient) Stop() error {
	if c := w.current(); c != nil {
		c.CloseNow()
	}
	return nil
}

func (w *wsClient) current() *websocket.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.conn
}

func (w *wsClient) dial(ctx context.Context) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, wsDialTimeout)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, w.url, &websocket.DialOptions{HTTPHeader: w.header})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(wsReadLimit)
	return conn, nil
}

// run reads from conn until it drops, then redials with backoff, until ctx
// ends. conn is nil when the first attempt failed with err.
func (w *wsClient) run(ctx context.Context, conn *websocket.Conn, err error) {
	backoff := time.Second
	for {
		if conn != nil {
			w.mu.Lock()
			w.conn = conn
			w.mu.Unlock()
			err = w.read(ctx, conn)
			w.mu.Lock()
			w.conn = nil
			w.mu.Unlock()
			conn.CloseNow()
			conn = nil
			backoff = time.Second
			if ctx.Err() != nil {
				return
			}
			w.host.SetStatus("error", "连接已断开，正在重连："+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < wsMaxBackoff {
			backoff *= 2
		}
		conn, err = w.dial(ctx)
		if ctx.Err() != nil {
			if conn != nil {
				conn.CloseNow()
			}
			return
		}
		if err != nil {
			w.host.SetStatus("error", "连接失败，正在重试："+err.Error())
		} else {
			w.host.SetStatus("running", "")
		}
	}
}

// read emits an event per message and returns why the connection ended.
func (w *wsClient) read(ctx context.Context, conn *websocket.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // a dead peer is only noticed when a ping goes unanswered
		tick := time.NewTicker(wsPingInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			pctx, pcancel := context.WithTimeout(ctx, wsDialTimeout)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				conn.CloseNow()
				return
			}
		}
	}()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		payload := map[string]any{}
		raw := map[string]any{"url": w.url}
		if typ == websocket.MessageBinary {
			payload["binary"] = base64.StdEncoding.EncodeToString(data)
			raw["type"] = "binary"
		} else {
			raw["type"], raw["body"] = "text", string(data)
			var parsed any
			if json.Unmarshal(data, &parsed) != nil {
				payload["text"] = string(data)
			} else if obj, ok := parsed.(map[string]any); ok {
				payload = obj
			} else {
				payload["data"] = parsed
			}
		}
		if _, err := w.host.Emit(&event.Event{Type: w.eventType, Payload: payload, Raw: raw}); err != nil {
			w.host.Log("warn", "emit: "+err.Error())
		}
	}
}

func (w *wsClient) Invoke(ctx context.Context, action string, p map[string]any, _ plugin.InvokeContext) (map[string]any, error) {
	if action != "send" {
		return nil, plugin.ErrUnknownAction(action)
	}
	var data []byte
	switch m := p["message"].(type) {
	case nil:
		return nil, errors.New("message is required")
	case string:
		if m == "" {
			return nil, errors.New("message is required")
		}
		data = []byte(m)
	default:
		var err error
		if data, err = json.Marshal(m); err != nil {
			return nil, err
		}
	}
	conn := w.current()
	if conn == nil {
		return nil, errors.New("连接已断开，正在重连")
	}
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		return nil, err
	}
	return map[string]any{"sent": true, "bytes": len(data)}, nil
}
