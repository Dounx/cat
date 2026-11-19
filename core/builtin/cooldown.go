package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/plugin"
)

const (
	cooldownID    = "cooldown"
	cooldownState = "seen"
	cooldownMax   = 10000
)

func cooldownEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: cooldownID, Name: "冷却限流", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "clock",
			Description:  "在设定的冷却时间内，同一个分组键只放行一次，重复事件停在这里，避免告警刷屏或重复触发。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "allow", Title: "冷却期内不重复往下走",
				Description: "首次经过时放行并开始计时；冷却秒数内再次经过同一个分组键，规则停在这里不再执行后续节点。输出：passed、key、remaining_seconds。",
				Params: map[string]any{
					"type":    "object",
					"x-order": []string{"seconds", "key"},
					"properties": map[string]any{
						"seconds": map[string]any{
							"type":        "number",
							"title":       "冷却秒数",
							"default":     60,
							"description": "同一个分组键两次放行之间至少间隔多少秒，默认 60 秒。",
						},
						"key": map[string]any{
							"type":        "string",
							"title":       "分组键（可选）",
							"description": "填了就按它的值各自独立冷却，例如 {{ conversation.id }} 或 {{ payload.service }}；留空则整条规则共用同一个冷却窗口。",
						},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &cooldown{now: time.Now, seen: map[string]time.Time{}}, nil
		},
	}
}

type cooldown struct {
	plugin.Base
	host plugin.Host
	now  func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

func (c *cooldown) Start(_ context.Context, h plugin.Host) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.host = h
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if h == nil {
		return nil
	}
	raw, ok := h.StateGet(cooldownState)
	if !ok || raw == "" {
		return nil
	}
	var saved map[string]int64
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return nil
	}
	now := c.now()
	for k, ns := range saved {
		until := time.Unix(0, ns)
		if now.Before(until) {
			c.seen[k] = until
		}
	}
	return nil
}

func (c *cooldown) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "allow" {
		return nil, plugin.ErrUnknownAction(action)
	}
	secs := num(p, "seconds", 60)
	if secs <= 0 {
		return nil, errors.New("冷却秒数要大于 0")
	}
	key := strings.TrimSpace(str(p, "key"))
	scope := ictx.RuleID + "\x00" + ictx.StepID + "\x00" + key

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	now := c.now()
	if until, ok := c.seen[scope]; ok && now.Before(until) {
		rem := int(math.Ceil(until.Sub(now).Seconds()))
		if rem < 1 {
			rem = 1
		}
		return map[string]any{
			"passed":            false,
			"key":               key,
			"remaining_seconds": rem,
			"note":              fmt.Sprintf("冷却中（还剩 %d 秒），本次先停在这里", rem),
		}, plugin.ErrHeld
	}
	c.seen[scope] = now.Add(time.Duration(secs * float64(time.Second)))
	for k, until := range c.seen {
		if !now.Before(until) {
			delete(c.seen, k)
		}
	}
	if len(c.seen) > cooldownMax {
		// Evict earliest-expiring entry if map grows beyond cap.
		var oldestKey string
		var oldestTime time.Time
		for k, u := range c.seen {
			if oldestKey == "" || u.Before(oldestTime) {
				oldestKey, oldestTime = k, u
			}
		}
		delete(c.seen, oldestKey)
	}
	c.persistLocked()
	return map[string]any{"passed": true, "key": key}, nil
}

func (c *cooldown) persistLocked() {
	if c.host == nil {
		return
	}
	dump := make(map[string]int64, len(c.seen))
	for k, u := range c.seen {
		dump[k] = u.UnixNano()
	}
	if raw, err := json.Marshal(dump); err == nil {
		_ = c.host.StateSet(cooldownState, string(raw))
	}
}
