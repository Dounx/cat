package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/plugin"
)

// The collect action gathers what comes through it instead of passing each
// thing on: the run stops there, and once there is enough, or the time has
// come, what follows runs once for the whole batch.

const (
	collectID = "collect"
	// collectMax is how many items a batch may grow to before it is passed on
	// whatever its settings say.
	collectMax = 500
	// collectState is the key the batches are kept under between runs.
	collectState = "batches"
)

// CollectTick is how often the batches are checked for one that is due. Tests
// shorten it.
var CollectTick = 15 * time.Second

func collectEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: collectID, Name: "攒一批", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "layers",
			Description:  "把经过这里的内容先存起来，攒够了或到了时间，再一起交给后面的节点。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "add", Title: "收集，攒够了再往下走",
				Description: "每来一个事件就收下一条，规则先停在这里。攒够条数或到了时间，后面的节点为这一整批运行一次。" +
					"「攒够几条」「每天几点」「每隔多久」至少填一个，填了几个就以先到的为准。输出：text（合在一起的内容）、items（每条一项）、count（条数）。",
				Params: map[string]any{
					"type":    "object",
					"x-order": []string{"item", "count", "at", "every", "key"},
					"properties": map[string]any{
						"item": map[string]any{"type": "string", "title": "收集什么", "format": "textarea", "default": "{{ payload.text }}"},
						"count": map[string]any{"type": "number", "title": "攒够几条就发出",
							"description": "留空表示不按条数。"},
						"at": map[string]any{"type": "string", "title": "每天几点发出",
							"description": "例如 9:00，多个时间用逗号分隔；按这台机器的时区。到点时一条都没有就不发。"},
						"every": map[string]any{"type": "number", "title": "每隔多少分钟发出",
							"description": "从这一批的第一条算起。"},
						"key": map[string]any{"type": "string", "title": "分开攒（可选）",
							"description": "填了就按它的值各攒各的，例如 {{ conversation.id }} 让每个会话各成一批。"},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) { return &collect{now: time.Now}, nil },
	}
}

// A batch is what one collect step of one rule has gathered so far.
type batch struct {
	RuleID string   `json:"rule_id"`
	StepID string   `json:"step_id"`
	Key    string   `json:"key,omitempty"`
	Items  []string `json:"items"`
	// EventID is the event the last item came with: the run that passes the
	// batch on is for that event.
	EventID string `json:"event_id"`
	// Due is when the batch is passed on however few it holds; zero when
	// only its count decides.
	Due time.Time `json:"due,omitzero"`
	// Timing is what Due was worked out from: changed settings start it over.
	Timing string `json:"timing,omitempty"`
}

type collect struct {
	plugin.Base
	host plugin.Host
	now  func() time.Time

	mu      sync.Mutex
	batches map[string]*batch
	stop    context.CancelFunc
}

func (c *collect) Start(ctx context.Context, h plugin.Host) error {
	c.mu.Lock()
	c.host = h
	c.batches = map[string]*batch{}
	if raw, ok := h.StateGet(collectState); ok {
		if err := json.Unmarshal([]byte(raw), &c.batches); err != nil {
			h.Log("warn", "攒下的内容读不出来，已丢弃："+err.Error())
			c.batches = map[string]*batch{}
		}
	}
	ctx, c.stop = context.WithCancel(ctx)
	c.mu.Unlock()
	go func() {
		t := time.NewTicker(CollectTick)
		defer t.Stop()
		for {
			c.release()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (c *collect) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stop != nil {
		c.stop()
	}
	return nil
}

// save writes the batches down. The caller holds c.mu.
func (c *collect) save() {
	raw, _ := json.Marshal(c.batches)
	if err := c.host.StateSet(collectState, string(raw)); err != nil {
		c.host.Log("warn", "攒下的内容没能存下来："+err.Error())
	}
}

func batchID(ruleID, stepID, key string) string {
	sum := sha256.Sum256([]byte(ruleID + "\x00" + stepID + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

// output is what a batch gives the nodes after its step.
func (b *batch) output() map[string]any {
	items := make([]any, len(b.Items))
	for i, s := range b.Items {
		items[i] = s
	}
	return map[string]any{"text": strings.Join(b.Items, "\n\n"), "items": items, "count": len(b.Items)}
}

func (c *collect) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "add" {
		return nil, plugin.ErrUnknownAction(action)
	}
	count := int(amount(p, "count"))
	every := time.Duration(amount(p, "every") * float64(time.Minute))
	at, err := parseTimes(text(p, "at"))
	if err != nil {
		return nil, err
	}
	if count <= 0 && every <= 0 && len(at) == 0 {
		return nil, errors.New("「攒够几条」「每天几点」「每隔多久」至少要填一个")
	}
	item := text(p, "item")
	if item == "" {
		return nil, errors.New("没有可收集的内容（「收集什么」算出来是空的）")
	}
	key := text(p, "key")
	timing := fmt.Sprint(every, at)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.batches == nil {
		return nil, errors.New("还没有启动")
	}
	id := batchID(ictx.RuleID, ictx.StepID, key)
	b := c.batches[id]
	if b == nil {
		b = &batch{RuleID: ictx.RuleID, StepID: ictx.StepID, Key: key}
		c.batches[id] = b
	}
	b.Items = append(b.Items, item)
	if ictx.Event != nil {
		b.EventID = ictx.Event.ID
	}
	if len(b.Items) == 1 || b.Timing != timing { // a batch begins, or its settings changed
		b.Timing, b.Due = timing, nextDue(c.now(), every, at)
	}
	if (count > 0 && len(b.Items) >= count) || len(b.Items) >= collectMax {
		delete(c.batches, id)
		c.save()
		return b.output(), nil
	}
	c.save()
	out := map[string]any{"count": len(b.Items)}
	if !b.Due.IsZero() {
		out["due"] = b.Due.Format(time.RFC3339)
	}
	return out, plugin.ErrHeld
}

// release passes on the batches whose time has come.
func (c *collect) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now, changed := c.now(), false
	for id, b := range c.batches {
		if b.Due.IsZero() || now.Before(b.Due) || len(b.Items) == 0 {
			continue
		}
		err := c.host.Resume(b.RuleID, b.StepID, b.EventID, b.output())
		if err != nil && !errors.Is(err, plugin.ErrGone) {
			continue // the rule is switched off: kept for when it is on again
		}
		delete(c.batches, id)
		changed = true
	}
	if changed {
		c.save()
	}
}

// A clock is a time of day, in minutes past midnight.
type clock int

// parseTimes reads times of day such as "9:00, 18:30".
func parseTimes(s string) ([]clock, error) {
	var out []clock
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '、' }) {
		h, m, ok := strings.Cut(strings.ReplaceAll(f, "：", ":"), ":")
		if !ok {
			m = "0"
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			return nil, fmt.Errorf("「每天几点」里的 %q 不是一个时间，要写成 9:00 这样", f)
		}
		out = append(out, clock(hh*60+mm))
	}
	return out, nil
}

// nextDue is when a batch begun now is passed on: after `every`, or at the
// next of the times of day, whichever is first; zero when neither is set.
func nextDue(now time.Time, every time.Duration, at []clock) time.Time {
	var due time.Time
	if every > 0 {
		due = now.Add(every)
	}
	for _, c := range at {
		t := time.Date(now.Year(), now.Month(), now.Day(), int(c)/60, int(c)%60, 0, 0, now.Location())
		if !t.After(now) {
			t = t.AddDate(0, 0, 1)
		}
		if due.IsZero() || t.Before(due) {
			due = t
		}
	}
	return due
}

// amount reads a parameter as a number, typed as one or as text; 0 when it is
// neither.
func amount(m map[string]any, key string) float64 {
	if s, ok := m[key].(string); ok {
		f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
		return f
	}
	return num(m, key, 0)
}

// text reads a parameter as trimmed text, whatever its template came to.
func text(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}
