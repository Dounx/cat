// A stand-in for NapCat used by the plugin's tests. Like the real thing it
// reads config/onebot11.json next to itself, prints a login QR link unless
// started with an account for quick login, and only then opens its OneBot
// WebSocket server.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	dir := filepath.Dir(os.Args[0])
	var cfg struct {
		Network struct {
			Servers []struct {
				Host  string `json:"host"`
				Port  int    `json:"port"`
				Token string `json:"token"`
			} `json:"websocketServers"`
		} `json:"network"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "config", "onebot11.json"))
	if err != nil || json.Unmarshal(b, &cfg) != nil || len(cfg.Network.Servers) == 0 {
		fmt.Println("[error] no onebot config")
		os.Exit(1)
	}
	srv := cfg.Network.Servers[0]
	logf := func(line string) {
		f, _ := os.OpenFile(filepath.Join(dir, "fake.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		fmt.Fprintln(f, line)
		f.Close()
	}
	if len(os.Args) > 1 {
		logf("quick-login " + os.Args[1])
		fmt.Println("[info] 快速登录", os.Args[1])
	} else {
		fmt.Println("[info] [NapCat] 请扫描下面的二维码")
		fmt.Println("[warn] 二维码解码URL: https://txz.qq.com/p?k=FAKEQR&f=1")
		time.Sleep(400 * time.Millisecond) // the user scans
	}
	fmt.Println("[info] 登录成功")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+srv.Token {
			w.WriteHeader(403)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := context.Background()
		_ = wsjson.Write(ctx, c, map[string]any{"post_type": "meta_event", "meta_event_type": "lifecycle", "self_id": 10001})
		go func() {
			time.Sleep(300 * time.Millisecond)
			_ = wsjson.Write(ctx, c, map[string]any{
				"post_type": "message", "message_type": "group", "time": 1700000000, "self_id": 10001,
				"user_id": 20002, "group_id": 30003, "message_id": 777, "raw_message": "[CQ:at,qq=10001] 你好",
				"message": []map[string]any{{"type": "at", "data": map[string]any{"qq": "10001"}}, {"type": "text", "data": map[string]any{"text": " 你好"}}},
				"sender":  map[string]any{"nickname": "nick", "card": "群名片"},
			})
		}()
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
				logf("send_msg " + string(p))
				data = map[string]any{"message_id": 888}
			default:
				_ = wsjson.Write(ctx, c, map[string]any{"status": "failed", "retcode": 1404, "message": "no such action", "echo": req.Echo})
				continue
			}
			_ = wsjson.Write(ctx, c, map[string]any{"status": "ok", "retcode": 0, "data": data, "echo": req.Echo})
		}
	})
	if err := http.ListenAndServe(fmt.Sprintf("%s:%d", srv.Host, srv.Port), mux); err != nil {
		fmt.Println("[error]", err)
		os.Exit(1)
	}
}
