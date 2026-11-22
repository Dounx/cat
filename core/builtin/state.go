package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/plugin"
)

const (
	stateID        = "state"
	stateStoreKey  = "entries"
	stateMax       = 10000
	defaultOnceTTL = 86400
)

func stateScopeProp() map[string]any {
	return map[string]any{
		"type":          "string",
		"title":         "作用域",
		"enum":          []string{"rule", "global"},
		"x-enum-titles": []string{"仅当前规则", "全局共享"},
		"default":       "rule",
		"description":   "「仅当前规则」按当前规则隔离；「全局共享」可在多条规则之间共享同一个键。",
	}
}

func stateEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: stateID, Name: "状态记忆", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "database",
			Description:  "跨次运行记住键值、计数器或去重记录，重启不丢失。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{
				{
					Name: "get", Title: "读取值",
					Description: "读取之前存下的值；键不存在或已过期时返回默认值。输出：value、found、key。",
					Params: map[string]any{
						"type":     "object",
						"required": []string{"key"},
						"x-order":  []string{"key", "scope", "default"},
						"properties": map[string]any{
							"key": map[string]any{
								"type":        "string",
								"title":       "键名",
								"description": "要读取的键名，支持变量，例如 {{ payload.user_id }}。",
							},
							"scope": stateScopeProp(),
							"default": map[string]any{
								"type":        "string",
								"title":       "默认值（可选）",
								"description": "键不存在或已过期时返回这个值。",
							},
						},
					},
				},
				{
					Name: "set", Title: "写入值",
					Description: "写入一个键值，可选过期时间。输出：key、value、previous、changed。",
					Params: map[string]any{
						"type":     "object",
						"required": []string{"key", "value"},
						"x-order":  []string{"key", "value", "scope", "ttl_seconds"},
						"properties": map[string]any{
							"key": map[string]any{
								"type":        "string",
								"title":       "键名",
								"description": "要写入的键名，支持变量。",
							},
							"value": map[string]any{
								"type":        "string",
								"title":       "值",
								"format":      "textarea",
								"description": "要记住的内容，支持模板变量。",
							},
							"scope": stateScopeProp(),
							"ttl_seconds": map[string]any{
								"type":        "number",
								"title":       "过期时间（秒，可选）",
								"default":     0,
								"description": "填 0 或留空表示永不过期。",
							},
						},
					},
				},
				{
					Name: "incr", Title: "计数器加减",
					Description: "对数字键做加减计数（默认 +1），键不存在时从 0 开始。输出：key、value、previous。",
					Params: map[string]any{
						"type":     "object",
						"required": []string{"key"},
						"x-order":  []string{"key", "delta", "scope", "ttl_seconds"},
						"properties": map[string]any{
							"key": map[string]any{
								"type":        "string",
								"title":       "键名",
								"description": "计数器的键名，例如 processed_count。",
							},
							"delta": map[string]any{
								"type":        "number",
								"title":       "增减量",
								"default":     1,
								"description": "默认加 1；填负数可以减，例如 -1。",
							},
							"scope": stateScopeProp(),
							"ttl_seconds": map[string]any{
								"type":        "number",
								"title":       "过期时间（秒，可选）",
								"description": "留空则保留原有过期时间；填 0 表示永不过期。",
							},
						},
					},
				},
				{
					Name: "once", Title: "去重（见过的不再往下走）",
					Description: "首次见到这个键时记下并放行；有效期内再见到同一个键，规则停在这里跳过后续节点。输出：seen、first、key。",
					Params: map[string]any{
						"type":     "object",
						"required": []string{"key"},
						"x-order":  []string{"key", "scope", "ttl_seconds"},
						"properties": map[string]any{
							"key": map[string]any{
								"type":        "string",
								"title":       "去重键",
								"description": "唯一标识这条记录的键，例如 {{ payload.id }} 或 {{ payload.url }}。",
							},
							"scope": stateScopeProp(),
							"ttl_seconds": map[string]any{
								"type":        "number",
								"title":       "去重有效时间（秒）",
								"default":     defaultOnceTTL,
								"description": "默认 86400 秒（24 小时）；填 0 表示永久去重。",
							},
						},
					},
				},
				{
					Name: "delete", Title: "删除键",
					Description: "删除之前存下的键（或清除去重记录）。输出：key、deleted。",
					Params: map[string]any{
						"type":     "object",
						"required": []string{"key"},
						"x-order":  []string{"key", "scope"},
						"properties": map[string]any{
							"key": map[string]any{
								"type":        "string",
								"title":       "键名",
								"description": "要删除的键名。",
							},
							"scope": stateScopeProp(),
						},
					},
				},
			},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &stateStore{now: time.Now, entries: map[string]stateItem{}}, nil
		},
	}
}

type stateItem struct {
	Value     any   `json:"v"`
	ExpiresAt int64 `json:"e,omitempty"` // unix nanos; 0 means never expires
	UpdatedAt int64 `json:"u,omitempty"` // unix nanos; used for eviction
}

func (it stateItem) expired(now time.Time) bool {
	return it.ExpiresAt > 0 && !now.Before(time.Unix(0, it.ExpiresAt))
}

type stateStore struct {
	plugin.Base
	host plugin.Host
	now  func() time.Time
	max  int // defaults to stateMax when <= 0

	mu      sync.Mutex
	entries map[string]stateItem
}

func (s *stateStore) limit() int {
	if s.max > 0 {
		return s.max
	}
	return stateMax
}

func (s *stateStore) Start(_ context.Context, h plugin.Host) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.host = h
	if s.entries == nil {
		s.entries = map[string]stateItem{}
	}
	if h == nil {
		return nil
	}
	raw, ok := h.StateGet(stateStoreKey)
	if !ok || raw == "" {
		return nil
	}
	var saved map[string]stateItem
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		h.Log("warn", "状态记忆数据读不出来，已忽略："+err.Error())
		return nil
	}
	now := s.now()
	for k, it := range saved {
		if !it.expired(now) {
			if f, ok := it.Value.(float64); ok {
				it.Value = cleanNumber(f)
			}
			s.entries[k] = it
		}
	}
	return nil
}

func (s *stateStore) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	switch action {
	case "get", "set", "incr", "once", "delete":
	default:
		return nil, plugin.ErrUnknownAction(action)
	}

	key := text(p, "key")
	if key == "" {
		return nil, errors.New("键名不能为空")
	}
	sk, err := scopedKey(p, ictx.RuleID, key)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = map[string]stateItem{}
	}
	now := s.now()

	switch action {
	case "get":
		it, ok := s.entries[sk]
		if ok && it.expired(now) {
			delete(s.entries, sk)
			s.persistLocked()
			ok = false
		}
		if !ok {
			return map[string]any{
				"value": p["default"],
				"found": false,
				"key":   key,
			}, nil
		}
		return map[string]any{
			"value": it.Value,
			"found": true,
			"key":   key,
		}, nil

	case "set":
		val, hasVal := p["value"]
		if !hasVal || val == nil {
			return nil, errors.New("缺少要写入的值（value）")
		}
		ttl, _, err := optNum(p, "ttl_seconds", 0)
		if err != nil {
			return nil, err
		}
		if ttl < 0 {
			return nil, errors.New("过期时间（ttl_seconds）不能小于 0")
		}
		var prev any
		var hadPrev bool
		if old, ok := s.entries[sk]; ok && !old.expired(now) {
			prev = old.Value
			hadPrev = true
		}
		changed := !hadPrev || !stateValuesEqual(prev, val)
		s.entries[sk] = stateItem{
			Value:     val,
			ExpiresAt: expiryUnixNano(now, ttl),
			UpdatedAt: now.UnixNano(),
		}
		s.compactLocked(now, sk)
		s.persistLocked()
		return map[string]any{
			"key":      key,
			"value":    val,
			"previous": prev,
			"changed":  changed,
		}, nil

	case "incr":
		delta, _, err := optNum(p, "delta", 1)
		if err != nil {
			return nil, err
		}
		ttl, hasTTL, err := optNum(p, "ttl_seconds", 0)
		if err != nil {
			return nil, err
		}
		if ttl < 0 {
			return nil, errors.New("过期时间（ttl_seconds）不能小于 0")
		}
		var oldCount float64
		var exp int64
		if old, ok := s.entries[sk]; ok && !old.expired(now) {
			v, ok := asFloat(old.Value)
			if !ok {
				return nil, fmt.Errorf("键 %q 当前的值不是数字，无法加减", key)
			}
			oldCount = v
			exp = old.ExpiresAt
		}
		if hasTTL {
			exp = expiryUnixNano(now, ttl)
		}
		newCount := oldCount + delta
		cleanNew := cleanNumber(newCount)
		cleanOld := cleanNumber(oldCount)
		s.entries[sk] = stateItem{
			Value:     cleanNew,
			ExpiresAt: exp,
			UpdatedAt: now.UnixNano(),
		}
		s.compactLocked(now, sk)
		s.persistLocked()
		return map[string]any{
			"key":      key,
			"value":    cleanNew,
			"previous": cleanOld,
		}, nil

	case "once":
		ttl, _, err := optNum(p, "ttl_seconds", defaultOnceTTL)
		if err != nil {
			return nil, err
		}
		if ttl < 0 {
			return nil, errors.New("去重有效时间（ttl_seconds）不能小于 0")
		}
		if old, ok := s.entries[sk]; ok && !old.expired(now) {
			return map[string]any{
				"seen":  true,
				"first": false,
				"key":   key,
				"note":  fmt.Sprintf("已处理过（%s），本次跳过", key),
			}, plugin.ErrHeld
		}
		s.entries[sk] = stateItem{
			Value:     true,
			ExpiresAt: expiryUnixNano(now, ttl),
			UpdatedAt: now.UnixNano(),
		}
		s.compactLocked(now, sk)
		s.persistLocked()
		return map[string]any{
			"seen":  false,
			"first": true,
			"key":   key,
		}, nil

	case "delete":
		old, ok := s.entries[sk]
		if !ok {
			return map[string]any{"key": key, "deleted": false}, nil
		}
		deleted := !old.expired(now)
		delete(s.entries, sk)
		s.persistLocked()
		return map[string]any{"key": key, "deleted": deleted}, nil
	}
	return nil, plugin.ErrUnknownAction(action)
}

func (s *stateStore) compactLocked(now time.Time, keepKey string) {
	for k, it := range s.entries {
		if it.expired(now) {
			delete(s.entries, k)
		}
	}
	maxEntries := s.limit()
	for len(s.entries) > maxEntries {
		// Evict earliest-expiring entry first; if none have a TTL, evict oldest updated.
		var victimExpKey string
		var victimExp int64
		var victimOldKey string
		var victimOld int64
		for k, it := range s.entries {
			if k == keepKey {
				continue
			}
			if it.ExpiresAt > 0 {
				if victimExpKey == "" || it.ExpiresAt < victimExp {
					victimExpKey, victimExp = k, it.ExpiresAt
				}
			}
			if victimOldKey == "" || it.UpdatedAt < victimOld {
				victimOldKey, victimOld = k, it.UpdatedAt
			}
		}
		switch {
		case victimExpKey != "":
			delete(s.entries, victimExpKey)
		case victimOldKey != "":
			delete(s.entries, victimOldKey)
		default:
			return
		}
	}
}

func (s *stateStore) persistLocked() {
	if s.host == nil {
		return
	}
	raw, err := json.Marshal(s.entries)
	if err != nil {
		return
	}
	if err := s.host.StateSet(stateStoreKey, string(raw)); err != nil {
		s.host.Log("warn", "状态记忆没能存下来："+err.Error())
	}
}

func scopedKey(p map[string]any, ruleID, key string) (string, error) {
	scope := strings.TrimSpace(str(p, "scope"))
	switch scope {
	case "", "rule":
		return "rule\x00" + ruleID + "\x00" + key, nil
	case "global":
		return "global\x00" + key, nil
	default:
		return "", fmt.Errorf("无效的作用域：%s（应为 rule 或 global）", scope)
	}
}

func expiryUnixNano(now time.Time, ttlSeconds float64) int64 {
	if ttlSeconds <= 0 {
		return 0
	}
	return now.Add(time.Duration(ttlSeconds * float64(time.Second))).UnixNano()
}

// optNum reads an optional numeric parameter. It distinguishes omitted/empty
// values (returning def, false, nil) from explicit values including 0.
func optNum(m map[string]any, key string, def float64) (float64, bool, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return def, false, nil
	}
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, true, fmt.Errorf("参数 %s 不是有效数字", key)
		}
		return v, true, nil
	case int:
		return float64(v), true, nil
	case int64:
		return float64(v), true, nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, true, fmt.Errorf("参数 %s 不是有效数字", key)
		}
		return f, true, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return def, false, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, true, fmt.Errorf("参数 %s（%q）不是有效数字", key, s)
		}
		return f, true, nil
	default:
		return 0, true, fmt.Errorf("参数 %s 不是有效数字", key)
	}
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	default:
		return 0, false
	}
}

func cleanNumber(f float64) any {
	if !math.IsNaN(f) && !math.IsInf(f, 0) && math.Trunc(f) == f && math.Abs(f) <= 1<<53 {
		return int64(f)
	}
	return f
}

func stateValuesEqual(a, b any) bool {
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return fmt.Sprint(a) == fmt.Sprint(b)
	}
	return bytes.Equal(ja, jb)
}
