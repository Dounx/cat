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
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

var offline atomic.Bool

func main() {
	if os.Getenv("FAKE_NAPCAT_DETACH") != "" {
		detach()
	}
	dir := filepath.Dir(os.Args[0])
	if d := os.Getenv("FAKE_NAPCAT_DIR"); d != "" { // run "in a container": config is mounted from elsewhere
		dir = d
	}
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
	} else if _, err := os.Stat(filepath.Join(dir, "refused")); os.Getenv("FAKE_NAPCAT_REFUSE_ONCE") != "" && err != nil {
		// QQ turns the first login down; like the real thing, no new QR code follows.
		_ = os.WriteFile(filepath.Join(dir, "refused"), nil, 0o644)
		fmt.Println("[warn] 二维码解码URL: https://txz.qq.com/p?k=REFUSED&f=1")
		time.Sleep(600 * time.Millisecond)
		fmt.Println("[info] [Core] [Login] 二维码已被扫描，等待确认...")
		fmt.Println(`[error] [Core] [Login] Login Error , ErrInfo:  [1,{"account":"10001","serverErrorCode":168,"title":"","message":"你的账号近期存在安全风险","additionalType":0}]`)
		select {}
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
		// Like QQ handing over what was sent while the account was offline,
		// the message goes to whoever is connected first, once.
		first := offline.CompareAndSwap(false, true)
		go func() {
			if !first {
				return
			}
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
	addr := fmt.Sprintf("%s:%d", srv.Host, srv.Port)
	if a := os.Getenv("FAKE_NAPCAT_LISTEN"); a != "" { // the port the "container" publishes
		logf("listen " + addr)
		addr = a
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("[error]", err)
		os.Exit(1)
	}
}
