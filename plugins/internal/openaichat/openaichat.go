// Package openaichat is a small client for OpenAI-compatible chat completion
// endpoints with conversation history kept on disk.
package openaichat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Client struct {
	Base  string // e.g. https://api.deepseek.com or https://api.openai.com/v1
	Key   string
	Model string
	HTTP  *http.Client
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

// Chat sends the messages and returns the assistant's reply.
func (c *Client) Chat(ctx context.Context, msgs []Message) (string, Usage, error) {
	if c.Key == "" {
		return "", Usage{}, errors.New("缺少 API Key")
	}
	body, _ := json.Marshal(map[string]any{"model": c.Model, "messages": msgs, "stream": false})
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.Base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Key)
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
		Usage Usage `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(raw))
		if out.Error != nil && out.Error.Message != "" {
			msg = out.Error.Message
		}
		if resp.StatusCode == 401 {
			msg = "API Key 无效"
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", Usage{}, fmt.Errorf("模型接口返回 %d：%s", resp.StatusCode, msg)
	}
	if len(out.Choices) == 0 {
		return "", out.Usage, errors.New("模型接口没有返回内容")
	}
	return out.Choices[0].Message.Content, out.Usage, nil
}

// History stores each conversation as a JSON file under Dir.
type History struct {
	Dir string
	// Max is the number of most recent messages kept (0 = 40).
	Max int
}

// ID is the stable session id for a key.
func ID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

func (h History) path(key string) string { return filepath.Join(h.Dir, ID(key)+".json") }

func (h History) Load(key string) []Message {
	var msgs []Message
	if b, err := os.ReadFile(h.path(key)); err == nil {
		_ = json.Unmarshal(b, &msgs)
	}
	return msgs
}

func (h History) Save(key string, msgs []Message) error {
	max := h.Max
	if max <= 0 {
		max = 40
	}
	if len(msgs) > max {
		msgs = msgs[len(msgs)-max:]
		// Never start the kept window with an assistant turn.
		for len(msgs) > 0 && msgs[0].Role != "user" {
			msgs = msgs[1:]
		}
	}
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(msgs)
	return os.WriteFile(h.path(key), b, 0o600)
}

func (h History) Clear(key string) { _ = os.Remove(h.path(key)) }

// Converse runs one turn: it loads the conversation for key (if any), asks
// the model, and stores the exchange. An empty key means a one-off exchange.
func Converse(ctx context.Context, c *Client, h History, key, system, prompt string, fresh bool) (text, sessionID string, usage Usage, err error) {
	var past []Message
	if key != "" {
		if fresh {
			h.Clear(key)
		}
		past = h.Load(key)
	}
	msgs := make([]Message, 0, len(past)+2)
	if system != "" {
		msgs = append(msgs, Message{Role: "system", Content: system})
	}
	msgs = append(msgs, past...)
	msgs = append(msgs, Message{Role: "user", Content: prompt})
	text, usage, err = c.Chat(ctx, msgs)
	if err != nil {
		return "", "", usage, err
	}
	if key != "" {
		sessionID = ID(key)
		past = append(past, Message{Role: "user", Content: prompt}, Message{Role: "assistant", Content: text})
		if err := h.Save(key, past); err != nil {
			return text, sessionID, usage, err
		}
	}
	return text, sessionID, usage, nil
}
