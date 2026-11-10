// A stand-in for RSSHub used by the plugin's tests. Like the real thing it
// takes PORT, ACCESS_KEY and LISTEN_INADDR_ANY from the environment, answers
// /healthz, and refuses requests that do not carry the access key. Every
// start is appended to the file named by FAKE_RSSHUB_LOG.
package main

import (
	"fmt"
	"net/http"
	"os"
)

const feed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Fake Hub</title><link>https://example.com/</link>
<item><title>Hello</title><link>https://example.com/1</link><guid>1</guid></item>
</channel></rss>`

func main() {
	port, key := os.Getenv("PORT"), os.Getenv("ACCESS_KEY")
	host := "0.0.0.0"
	if os.Getenv("LISTEN_INADDR_ANY") == "0" {
		host = "127.0.0.1"
	}
	if path := os.Getenv("FAKE_RSSHUB_LOG"); path != "" {
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "listen %s:%s key=%t node_env=%s\n", host, port, key != "", os.Getenv("NODE_ENV"))
			f.Close()
		}
	}
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if key != "" && r.URL.Query().Get("key") != key {
				http.Error(w, "Authentication failed. Access denied.", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/healthz", guard(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") }))
	mux.HandleFunc("/test/feed", guard(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, feed) }))
	fmt.Printf("\x1b[32minfo\x1b[39m: RSSHub is running on port %s! Cheers!\n", port)
	if err := http.ListenAndServe("127.0.0.1:"+port, mux); err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
}
