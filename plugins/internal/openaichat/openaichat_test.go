package openaichat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestChatStreamSSE(t *testing.T) {
	var gotStream bool
	var gotIncludeUsage bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Stream        bool            `json:"stream"`
			StreamOptions map[string]bool `json:"stream_options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotStream = body.Stream
		gotIncludeUsage = body.StreamOptions["include_usage"]

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		chunks := []string{
			`{"choices":[{"delta":{"role":"assistant","content":""}}]}`,
			`{"choices":[{"delta":{"content":"你好"}}]}`,
			`{"choices":[{"delta":{"content":"，世界"}}]}`,
			`{"choices":[{"delta":{"content":"！"}}]}`,
			`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":5}}`,
		}
		for _, c := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", c)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "k-1", Model: "test-model"}
	hist := History{Dir: filepath.Join(t.TempDir(), "sessions"), Max: 10}

	var texts []string
	var deltas []string
	text, sid, usage, err := ConverseStream(context.Background(), c, hist, "conv-1", "sys", "hi", nil, false, func(acc, delta string) {
		texts = append(texts, acc)
		deltas = append(deltas, delta)
	})
	if err != nil {
		t.Fatalf("ConverseStream: %v", err)
	}
	if !gotStream || !gotIncludeUsage {
		t.Fatalf("expected stream=true & include_usage=true, got stream=%v include_usage=%v", gotStream, gotIncludeUsage)
	}
	if text != "你好，世界！" {
		t.Fatalf("text = %q, want 你好，世界！", text)
	}
	if sid == "" {
		t.Fatal("expected non-empty session id")
	}
	if usage.PromptTokens != 12 || usage.CompletionTokens != 5 {
		t.Fatalf("usage = %+v", usage)
	}
	if len(texts) != 3 || texts[0] != "你好" || texts[1] != "你好，世界" || texts[2] != "你好，世界！" {
		t.Fatalf("progress texts = %v", texts)
	}
	if len(deltas) != 3 || deltas[0] != "你好" || deltas[1] != "，世界" || deltas[2] != "！" {
		t.Fatalf("progress deltas = %v", deltas)
	}
	// Verify history was persisted.
	saved := hist.Load("conv-1")
	if len(saved) != 2 || saved[0].Role != "user" || saved[0].Content != "hi" || saved[1].Role != "assistant" || saved[1].Content != "你好，世界！" {
		t.Fatalf("saved history = %+v", saved)
	}
}

func TestChatStreamFallbackJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"role": "assistant", "content": "plain json reply"}}},
			"usage":   map[string]int{"prompt_tokens": 4, "completion_tokens": 3},
		})
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "k-1", Model: "m1"}
	var got []string
	text, usage, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(acc, delta string) {
		got = append(got, acc+"|"+delta)
	})
	if err != nil {
		t.Fatalf("ChatStream fallback: %v", err)
	}
	if text != "plain json reply" || usage.PromptTokens != 4 || usage.CompletionTokens != 3 {
		t.Fatalf("got text=%q usage=%+v", text, usage)
	}
	if len(got) != 1 || got[0] != "plain json reply|plain json reply" {
		t.Fatalf("progress = %v", got)
	}
}

func TestChatStreamRetryWithoutStreamOptions(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, has := body["stream_options"]; has {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown parameter: stream_options"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "k-1", Model: "m1"}
	var seen string
	text, _, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(acc, _ string) {
		seen = acc
	})
	if err != nil {
		t.Fatalf("ChatStream retry: %v", err)
	}
	if calls != 2 || text != "ok" || seen != "ok" {
		t.Fatalf("calls=%d text=%q seen=%q", calls, text, seen)
	}
}

func TestChatStreamErrorInSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"overloaded\"}}\n\n")
	}))
	defer srv.Close()

	c := &Client{Base: srv.URL, Key: "k-1", Model: "m1"}
	_, _, err := c.ChatStream(context.Background(), []Message{{Role: "user", Content: "hi"}}, func(string, string) {})
	if err == nil || !strings.Contains(err.Error(), "overloaded") {
		t.Fatalf("expected overloaded error, got %v", err)
	}
}
