package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/plugin"
)

const (
	// DelayID is the built-in delay plugin and singleton instance ID.
	DelayID    = "delay"
	delayState = "timers"
	delayMax   = 5000
)

// DelayTick is the maximum interval between checks for due delay timers and
// retries for rules that were temporarily disabled. Tests shorten it.
var DelayTick = 2 * time.Second

func delayEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: DelayID, Name: "延时等待", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "timer",
			Description:  "让规则停在这里等一段时间，到点后再继续执行后面的节点；等待任务会持久化，Cat 重启后仍会按时恢复。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "wait", Title: "等待一段时间后继续",
				Description: "暂停当前分支并记录恢复时间，到点后自动继续执行后面的节点。输出：waited_seconds、resumed_at、note。",
				Params: map[string]any{
					"type":    "object",
					"x-order": []string{"seconds", "note"},
					"properties": map[string]any{
						"seconds": map[string]any{
							"type":        "number",
							"title":       "等待秒数",
							"default":     60,
							"description": "等待多少秒后继续执行，默认 60 秒（支持小数或模板表达式）。",
						},
						"note": map[string]any{
							"type":        "string",
							"title":       "备注（可选）",
							"description": "说明为什么等待，会显示在运行记录里，例如「等待服务预热」。",
						},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &delay{now: time.Now, timers: map[string]*delayTimer{}, wake: make(chan struct{}, 1)}, nil
		},
	}
}

type delayTimer struct {
	ID            string    `json:"id"`
	RuleID        string    `json:"rule_id"`
	StepID        string    `json:"step_id"`
	EventID       string    `json:"event_id"`
	RunID         string    `json:"run_id,omitempty"`
	WaitedSeconds float64   `json:"waited_seconds"`
	Reason        string    `json:"reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	ResumeAt      time.Time `json:"resume_at"`
}

type delay struct {
	plugin.Base
	host plugin.Host
	now  func() time.Time

	mu     sync.Mutex
	timers map[string]*delayTimer
	wake   chan struct{}
	stop   context.CancelFunc
	done   chan struct{}
}

func (d *delay) Start(ctx context.Context, h plugin.Host) error {
	d.mu.Lock()
	d.host = h
	d.timers = map[string]*delayTimer{}
	if d.wake == nil {
		d.wake = make(chan struct{}, 1)
	}
	if h != nil {
		if raw, ok := h.StateGet(delayState); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &d.timers); err != nil {
				h.Log("warn", "延时等待任务读不出来，已丢弃："+err.Error())
				d.timers = map[string]*delayTimer{}
			}
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	d.stop = cancel
	done := make(chan struct{})
	d.done = done
	d.mu.Unlock()

	go d.loop(runCtx, done)
	return nil
}

func (d *delay) Stop() error {
	d.mu.Lock()
	stop := d.stop
	done := d.done
	d.stop = nil
	d.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	return nil
}

func (d *delay) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	for {
		d.release()
		wait := d.nextWait()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-d.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (d *delay) nextWait() time.Duration {
	tick := DelayTick
	if tick <= 0 {
		tick = time.Second
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	wait := tick
	for _, t := range d.timers {
		if rem := t.ResumeAt.Sub(now); rem > 0 && rem < wait {
			wait = rem
		}
	}
	if wait < 10*time.Millisecond {
		wait = 10 * time.Millisecond
	}
	return wait
}

func (d *delay) signalWake() {
	if d.wake == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

func (d *delay) saveLocked() {
	if d.host == nil {
		return
	}
	raw, err := json.Marshal(d.timers)
	if err != nil {
		return
	}
	if err := d.host.StateSet(delayState, string(raw)); err != nil {
		d.host.Log("warn", "延时等待任务没能存下来："+err.Error())
	}
}

func (d *delay) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "wait" {
		return nil, plugin.ErrUnknownAction(action)
	}
	secs := 60.0
	if _, has := p["seconds"]; has && text(p, "seconds") != "" {
		secs = amount(p, "seconds")
	}
	if math.IsNaN(secs) || math.IsInf(secs, 0) || secs <= 0 {
		return nil, errors.New("等待秒数要大于 0")
	}
	reason := text(p, "note")
	if reason == "" {
		reason = text(p, "reason")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timers == nil {
		return nil, errors.New("还没有启动")
	}
	if len(d.timers) >= delayMax {
		return nil, fmt.Errorf("等待中的延时任务已达到上限（%d）", delayMax)
	}
	now := d.now()
	resumeAt := now.Add(time.Duration(secs * float64(time.Second)))
	evID := ""
	if ictx.Event != nil {
		evID = ictx.Event.ID
	}
	t := &delayTimer{
		ID:            id.New(),
		RuleID:        ictx.RuleID,
		StepID:        ictx.StepID,
		EventID:       evID,
		RunID:         ictx.RunID,
		WaitedSeconds: secs,
		Reason:        reason,
		CreatedAt:     now,
		ResumeAt:      resumeAt,
	}
	d.timers[t.ID] = t
	d.saveLocked()
	d.signalWake()

	note := fmt.Sprintf("等待 %s 秒后继续，预计 %s 恢复", formatSeconds(secs), resumeAt.Local().Format("15:04:05"))
	if reason != "" {
		note = reason + "（" + note + "）"
	}
	return map[string]any{
		"timer_id":       t.ID,
		"waited_seconds": secs,
		"resume_at":      resumeAt.Format(time.RFC3339),
		"note":           note,
	}, plugin.ErrHeld
}

func (d *delay) release() {
	d.mu.Lock()
	if d.host == nil || len(d.timers) == 0 {
		d.mu.Unlock()
		return
	}
	now := d.now()
	var due []*delayTimer
	for _, t := range d.timers {
		if !now.Before(t.ResumeAt) {
			cp := *t
			due = append(due, &cp)
		}
	}
	host := d.host
	d.mu.Unlock()

	if len(due) == 0 {
		return
	}

	var remove []string
	for _, t := range due {
		out := map[string]any{
			"waited_seconds": t.WaitedSeconds,
			"resumed_at":     now.Format(time.RFC3339),
		}
		if t.Reason != "" {
			out["note"] = t.Reason
		}
		err := host.Resume(t.RuleID, t.StepID, t.EventID, out)
		if err != nil && !errors.Is(err, plugin.ErrGone) {
			// Rule is disabled or temporarily unavailable: keep timer pending.
			continue
		}
		remove = append(remove, t.ID)
	}

	if len(remove) == 0 {
		return
	}
	d.mu.Lock()
	changed := false
	for _, tid := range remove {
		if _, ok := d.timers[tid]; ok {
			delete(d.timers, tid)
			changed = true
		}
	}
	if changed {
		d.saveLocked()
	}
	d.mu.Unlock()
}

func formatSeconds(secs float64) string {
	if math.Abs(secs-math.Round(secs)) < 1e-6 {
		return strconv.FormatInt(int64(math.Round(secs)), 10)
	}
	return strconv.FormatFloat(secs, 'f', -1, 64)
}
