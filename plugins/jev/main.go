// Command cat-plugin-jev puts questions to Jev, TypeSafe's System One model:
// state and a typed question in, a decision with probabilities out. It writes
// no text, so all it offers is "judge", which a rule's condition node asks.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/harness"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

const (
	defaultBase  = "https://api.typesafe.ai"
	defaultModel = "jev-latest"
	// maxChoices is how many options one Choice question takes.
	maxChoices = 255
	// the one question each request asks
	questionID = "q"
)

type client struct {
	base, key, model string
	http             *http.Client
	// how long to wait before asking again when told to back off
	backoff time.Duration
}

type answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          *float64           `json:"noul"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// ask sends one question about the state and returns its answer.
func (c *client) ask(ctx context.Context, state any, question map[string]any) (answer, string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.model, "state": state, "questions": map[string]any{questionID: question},
	})
	if err != nil {
		return answer{}, "", err
	}
	var status int
	var raw []byte
	// 429 and 529 mean "later": ask again a few times, a little slower each time.
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.base, "/")+"/v1/systemone", bytes.NewReader(body))
		if err != nil {
			return answer{}, "", err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.key)
		resp, err := c.http.Do(req)
		if err != nil {
			return answer{}, "", err
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		status = resp.StatusCode
		if (status != 429 && status != 529) || try == 3 {
			break
		}
		select {
		case <-ctx.Done():
			return answer{}, "", ctx.Err()
		case <-time.After(c.backoff << try):
		}
	}
	if status != 200 {
		msg := strings.TrimSpace(string(raw))
		// Errors come as {"error": {"message": …}}: say the message, not the JSON.
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		switch status {
		case 401:
			msg = "API Key 无效"
		case 429, 529:
			msg = "请求太频繁或服务繁忙，稍后再试"
		}
		if r := []rune(msg); len(r) > 300 {
			msg = string(r[:300])
		}
		return answer{}, "", fmt.Errorf("Jev 接口返回 %d：%s", status, msg)
	}
	var out struct {
		Model   string            `json:"model"`
		Answers map[string]answer `json:"answers"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return answer{}, "", fmt.Errorf("Jev 接口的回复看不懂：%w", err)
	}
	a, ok := out.Answers[questionID]
	if !ok {
		return answer{}, "", errors.New("Jev 接口没有给出答案")
	}
	return a, out.Model, nil
}

// descriptions reads what each option means: one "option: what it covers"
// per line. Options left out go to the model by name alone.
func descriptions(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		i := strings.IndexAny(line, ":：")
		if i < 0 {
			continue
		}
		name, about := strings.TrimSpace(line[:i]), strings.TrimSpace(strings.TrimLeft(line[i:], ":："))
		if name != "" && about != "" {
			out[name] = about
		}
	}
	return out
}

func number(p map[string]any, key string, or float64) (float64, error) {
	switch v := p[key].(type) {
	case nil:
		return or, nil
	case float64:
		return v, nil
	case string:
		if strings.TrimSpace(v) == "" {
			return or, nil
		}
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err != nil {
			return 0, fmt.Errorf("%s 不是数字：%q", key, v)
		}
		return f, nil
	}
	return 0, fmt.Errorf("%s 不是数字", key)
}

func New() cat.Plugin {
	var (
		c     *client
		guard harness.Guard
	)
	return cat.Plugin{
		Start: func(_ context.Context, h *cat.Host) error {
			c = &client{
				base: harness.Str(h.Config, "base_url"), key: harness.Str(h.Config, "api_key"), model: harness.Str(h.Config, "model"),
				http: &http.Client{Timeout: time.Minute}, backoff: 500 * time.Millisecond,
			}
			if c.base == "" {
				c.base = defaultBase
			}
			if c.model == "" {
				c.model = defaultModel
			}
			if c.key == "" {
				return errors.New("需要填写 API Key")
			}
			guard = harness.NewGuard(h.Config, true)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			// With choices, a Choice question: which of them fits. Without,
			// a Noul: how likely the answer is yes, held against a threshold.
			"judge": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				if err := guard.Check(actx); err != nil {
					return nil, err
				}
				prompt := harness.Str(p, "prompt")
				if prompt == "" {
					return nil, errors.New("没有写要判断什么")
				}
				// What is judged: text, or an object or list kept as it is.
				// Left empty, the event that set the rule off.
				state := p["state"]
				if s, ok := state.(string); ok && strings.TrimSpace(s) == "" {
					state = nil
				}
				if state == nil {
					if actx.Event == nil {
						return nil, errors.New("没有可判断的内容")
					}
					state = map[string]any{
						"type": actx.Event.Type, "conversation": actx.Event.Conversation,
						"actor": actx.Event.Actor, "payload": actx.Event.Payload,
					}
				}
				choices := harness.Choices(p)
				if len(choices) > 0 && !(len(choices) == 2 && choices[0] == "yes" && choices[1] == "no") {
					if len(choices) < 2 {
						return nil, errors.New("至少要有两个选项")
					}
					if len(choices) > maxChoices {
						return nil, fmt.Errorf("选项太多：%d 个，最多 %d 个", len(choices), maxChoices)
					}
					about := descriptions(harness.Str(p, "descriptions"))
					criteria := map[string]any{}
					for _, ch := range choices {
						criteria[ch] = nil
						if d, ok := about[ch]; ok {
							criteria[ch] = d
						}
					}
					a, model, err := c.ask(ctx, state, map[string]any{"type": "choice", "instructions": prompt, "criteria": criteria})
					if err != nil {
						return nil, err
					}
					if _, ok := criteria[a.Choice]; !ok {
						return nil, fmt.Errorf("Jev 的答案不在选项里：%q", a.Choice)
					}
					out := map[string]any{"choice": a.Choice, "probabilities": a.Probabilities, "model": model}
					if a.Confidence != nil {
						out["confidence"] = *a.Confidence
					}
					return out, nil
				}
				threshold, err := number(p, "threshold", 0.5)
				if err != nil {
					return nil, err
				}
				if threshold <= 0 || threshold >= 1 {
					return nil, fmt.Errorf("阈值要在 0 和 1 之间：%g", threshold)
				}
				a, model, err := c.ask(ctx, state, map[string]any{"type": "noul", "instructions": prompt})
				if err != nil {
					return nil, err
				}
				if a.Noul == nil {
					return nil, errors.New("Jev 接口没有给出答案")
				}
				choice := "no"
				if *a.Noul >= threshold {
					choice = "yes"
				}
				return map[string]any{"choice": choice, "probability": *a.Noul, "model": model}, nil
			},
		},
	}
}
