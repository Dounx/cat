package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/plugin"
)

const (
	// ApprovalID is the built-in human-in-the-loop approval plugin and singleton instance ID.
	ApprovalID         = "approval"
	approvalState      = "requests"
	approvalHistoryMax = 100
)

// ApprovalTick is how often pending approvals are checked for expiration. Tests shorten it.
var ApprovalTick = 5 * time.Second

// Sentinel errors returned by ApprovalManager.DecideApproval.
var (
	ErrApprovalNotFound       = errors.New("确认事项不存在")
	ErrApprovalAlreadyDecided = errors.New("该确认事项已处理")
	ErrInvalidDecision        = errors.New("decision 必须是 approve 或 reject")
)

// ApprovalStatus is the lifecycle state of a human approval request.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalRejected ApprovalStatus = "rejected"
	ApprovalExpired  ApprovalStatus = "expired"
)

// ApprovalRequest represents one human-in-the-loop approval gate created by a rule step.
type ApprovalRequest struct {
	ID             string         `json:"id"`
	RuleID         string         `json:"rule_id"`
	RuleName       string         `json:"rule_name,omitempty"`
	StepID         string         `json:"step_id"`
	EventID        string         `json:"event_id"`
	RunID          string         `json:"run_id,omitempty"`
	Title          string         `json:"title"`
	Detail         string         `json:"detail,omitempty"`
	Status         ApprovalStatus `json:"status"`
	Comment        string         `json:"comment,omitempty"`
	OnTimeout      string         `json:"on_timeout,omitempty"` // "reject" | "approve"
	ResumeOnReject bool           `json:"resume_on_reject,omitempty"`
	CreatedAt      time.Time      `json:"created_at"`
	ExpiresAt      *time.Time     `json:"expires_at,omitempty"`
	DecidedAt      *time.Time     `json:"decided_at,omitempty"`
}

// ApprovalManager is implemented by the built-in approval instance so the
// engine and HTTP API can inspect and decide pending approvals.
type ApprovalManager interface {
	ListApprovals() []ApprovalRequest
	GetApproval(id string) (ApprovalRequest, bool)
	DecideApproval(id, decision, comment string) (ApprovalRequest, error)
	SetApprovalNotifier(fn func(ApprovalRequest))
}

func approvalEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: ApprovalID, Name: "人工确认", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "shield-check",
			Description:  "让规则停在这里等待人工确认；点击同意后继续执行后面的节点，拒绝或超时则按设定处理。",
			Capabilities: []string{plugin.CapAction},
			Actions: []plugin.ActionSpec{{
				Name: "request", Title: "等待人工确认后继续",
				Description: "创建一个待确认事项并暂停当前分支，在「记录」页点击同意后继续往下走。输出：approved、decision、approval_id、comment、decided_at。",
				Params: map[string]any{
					"type":    "object",
					"x-order": []string{"title", "detail", "timeout_minutes", "on_timeout", "resume_on_reject"},
					"properties": map[string]any{
						"title": map[string]any{
							"type":        "string",
							"title":       "确认事项标题",
							"default":     "请确认是否继续",
							"description": "展示在待确认卡片上的标题，支持变量，例如「确认执行部署」。",
						},
						"detail": map[string]any{
							"type":        "string",
							"format":      "textarea",
							"title":       "详细信息",
							"default":     "{{ payload.text }}",
							"description": "供确认人查看的补充上下文，默认取事件内容。",
						},
						"timeout_minutes": map[string]any{
							"type":        "number",
							"title":       "超时分钟数",
							"default":     60,
							"description": "超过多少分钟未处理则自动按超时策略处理；填 0 表示永不超时。",
						},
						"on_timeout": map[string]any{
							"type":          "string",
							"title":         "超时处理",
							"enum":          []string{"reject", "approve"},
							"x-enum-titles": []string{"自动拒绝", "自动同意"},
							"default":       "reject",
							"description":   "超时未处理时的默认决定。",
						},
						"resume_on_reject": map[string]any{
							"type":        "boolean",
							"title":       "拒绝时也继续后续节点（用于条件分支）",
							"default":     false,
							"description": "默认关闭：拒绝后规则直接停在这里，避免误执行后续危险动作。开启后拒绝也会继续，可用条件节点判断 steps.<节点ID>.approved == true 分流。",
						},
					},
				},
			}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) {
			return &approval{now: time.Now, requests: map[string]*ApprovalRequest{}}, nil
		},
	}
}

type approval struct {
	plugin.Base
	host plugin.Host
	now  func() time.Time

	mu       sync.Mutex
	requests map[string]*ApprovalRequest
	notifier func(ApprovalRequest)
	stop     context.CancelFunc
	done     chan struct{}
}

func (a *approval) SetApprovalNotifier(fn func(ApprovalRequest)) {
	a.mu.Lock()
	a.notifier = fn
	a.mu.Unlock()
}

func (a *approval) notify(req ApprovalRequest) {
	a.mu.Lock()
	fn := a.notifier
	a.mu.Unlock()
	if fn != nil {
		fn(req)
	}
}

func (a *approval) Start(ctx context.Context, h plugin.Host) error {
	a.mu.Lock()
	a.host = h
	a.requests = map[string]*ApprovalRequest{}
	if h != nil {
		if raw, ok := h.StateGet(approvalState); ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &a.requests); err != nil {
				h.Log("warn", "待确认记录读不出来，已丢弃："+err.Error())
				a.requests = map[string]*ApprovalRequest{}
			}
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	a.stop = cancel
	done := make(chan struct{})
	a.done = done
	a.mu.Unlock()

	go func() {
		defer close(done)
		tick := ApprovalTick
		if tick <= 0 {
			tick = time.Second
		}
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			a.expireDue()
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (a *approval) Stop() error {
	a.mu.Lock()
	stop := a.stop
	done := a.done
	a.stop = nil
	a.mu.Unlock()
	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}
	return nil
}

func (a *approval) saveLocked() {
	a.pruneHistoryLocked()
	if a.host == nil {
		return
	}
	raw, err := json.Marshal(a.requests)
	if err != nil {
		return
	}
	if err := a.host.StateSet(approvalState, string(raw)); err != nil {
		a.host.Log("warn", "待确认记录没能存下来："+err.Error())
	}
}

// pruneHistoryLocked keeps all pending requests and at most approvalHistoryMax
// decided/expired requests (newest first).
func (a *approval) pruneHistoryLocked() {
	var finished []*ApprovalRequest
	for _, r := range a.requests {
		if r.Status != ApprovalPending {
			finished = append(finished, r)
		}
	}
	if len(finished) <= approvalHistoryMax {
		return
	}
	sort.Slice(finished, func(i, j int) bool {
		return approvalSortTime(finished[i]).After(approvalSortTime(finished[j]))
	})
	for _, r := range finished[approvalHistoryMax:] {
		delete(a.requests, r.ID)
	}
}

func approvalSortTime(r *ApprovalRequest) time.Time {
	if r.DecidedAt != nil && !r.DecidedAt.IsZero() {
		return *r.DecidedAt
	}
	return r.CreatedAt
}

func (a *approval) Invoke(_ context.Context, action string, p map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	if action != "request" {
		return nil, plugin.ErrUnknownAction(action)
	}
	title := text(p, "title")
	if title == "" {
		title = "请确认是否继续"
	}
	detail := text(p, "detail")
	timeoutMins := 60.0
	if _, has := p["timeout_minutes"]; has && text(p, "timeout_minutes") != "" {
		timeoutMins = amount(p, "timeout_minutes")
	}
	if math.IsNaN(timeoutMins) || math.IsInf(timeoutMins, 0) || timeoutMins < 0 {
		return nil, errors.New("超时分钟数不能小于 0")
	}
	onTimeout := strings.ToLower(text(p, "on_timeout"))
	if onTimeout == "" {
		onTimeout = "reject"
	}
	if onTimeout != "reject" && onTimeout != "approve" {
		return nil, errors.New("超时处理只能选 reject 或 approve")
	}
	resumeOnReject := boolVal(p, "resume_on_reject", false)

	a.mu.Lock()
	if a.requests == nil {
		a.mu.Unlock()
		return nil, errors.New("还没有启动")
	}
	now := a.now()
	var expiresAt *time.Time
	if timeoutMins > 0 {
		exp := now.Add(time.Duration(timeoutMins * float64(time.Minute)))
		expiresAt = &exp
	}
	evID := ""
	if ictx.Event != nil {
		evID = ictx.Event.ID
	}
	req := &ApprovalRequest{
		ID:             id.New(),
		RuleID:         ictx.RuleID,
		RuleName:       ictx.RuleName,
		StepID:         ictx.StepID,
		EventID:        evID,
		RunID:          ictx.RunID,
		Title:          title,
		Detail:         detail,
		Status:         ApprovalPending,
		OnTimeout:      onTimeout,
		ResumeOnReject: resumeOnReject,
		CreatedAt:      now,
		ExpiresAt:      expiresAt,
	}
	a.requests[req.ID] = req
	a.saveLocked()
	snapshot := cloneApproval(req)
	a.mu.Unlock()

	a.notify(snapshot)

	out := map[string]any{
		"approval_id": req.ID,
		"status":      string(ApprovalPending),
		"title":       req.Title,
		"note":        "等待人工确认：" + req.Title,
	}
	if expiresAt != nil {
		out["expires_at"] = expiresAt.Format(time.RFC3339)
	}
	return out, plugin.ErrHeld
}

func (a *approval) ListApprovals() []ApprovalRequest {
	a.mu.Lock()
	out := make([]ApprovalRequest, 0, len(a.requests))
	for _, r := range a.requests {
		out = append(out, cloneApproval(r))
	}
	a.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		pi := out[i].Status == ApprovalPending
		pj := out[j].Status == ApprovalPending
		if pi != pj {
			return pi
		}
		ti := approvalSortTime(&out[i])
		tj := approvalSortTime(&out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

func (a *approval) GetApproval(reqID string) (ApprovalRequest, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.requests[reqID]
	if !ok {
		return ApprovalRequest{}, false
	}
	return cloneApproval(r), true
}

func (a *approval) DecideApproval(reqID, decision, comment string) (ApprovalRequest, error) {
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "approve" && decision != "reject" {
		return ApprovalRequest{}, ErrInvalidDecision
	}
	comment = strings.TrimSpace(comment)

	a.mu.Lock()
	req, ok := a.requests[reqID]
	if !ok {
		a.mu.Unlock()
		return ApprovalRequest{}, ErrApprovalNotFound
	}
	if req.Status != ApprovalPending {
		a.mu.Unlock()
		return ApprovalRequest{}, ErrApprovalAlreadyDecided
	}
	now := a.now()
	host := a.host
	shouldResume := decision == "approve" || req.ResumeOnReject
	ruleID, stepID, eventID := req.RuleID, req.StepID, req.EventID
	a.mu.Unlock()

	if shouldResume && host != nil {
		out := map[string]any{
			"approved":    decision == "approve",
			"decision":    decision,
			"approval_id": reqID,
			"comment":     comment,
			"decided_at":  now.Format(time.RFC3339),
		}
		if err := host.Resume(ruleID, stepID, eventID, out); err != nil {
			if errors.Is(err, plugin.ErrGone) {
				if decision == "approve" {
					return ApprovalRequest{}, fmt.Errorf("对应的规则或节点已不存在，无法继续执行：%w", err)
				}
				// Rejecting a request whose rule was deleted can still close the approval cleanly.
			} else {
				return ApprovalRequest{}, err
			}
		}
	}

	a.mu.Lock()
	req, ok = a.requests[reqID]
	if !ok {
		a.mu.Unlock()
		return ApprovalRequest{}, ErrApprovalNotFound
	}
	if req.Status != ApprovalPending {
		a.mu.Unlock()
		return ApprovalRequest{}, ErrApprovalAlreadyDecided
	}
	if decision == "approve" {
		req.Status = ApprovalApproved
	} else {
		req.Status = ApprovalRejected
	}
	req.Comment = comment
	req.DecidedAt = &now
	a.saveLocked()
	snapshot := cloneApproval(req)
	a.mu.Unlock()

	a.notify(snapshot)
	return snapshot, nil
}

func (a *approval) expireDue() {
	a.mu.Lock()
	if len(a.requests) == 0 {
		a.mu.Unlock()
		return
	}
	now := a.now()
	var due []ApprovalRequest
	for _, r := range a.requests {
		if r.Status == ApprovalPending && r.ExpiresAt != nil && !now.Before(*r.ExpiresAt) {
			due = append(due, cloneApproval(r))
		}
	}
	host := a.host
	a.mu.Unlock()

	if len(due) == 0 {
		return
	}

	for _, item := range due {
		autoApprove := item.OnTimeout == "approve"
		shouldResume := autoApprove || item.ResumeOnReject
		comment := "超时自动拒绝"
		status := ApprovalExpired
		decision := "reject"
		if autoApprove {
			comment = "超时自动同意"
			status = ApprovalApproved
			decision = "approve"
		}

		if shouldResume && host != nil {
			out := map[string]any{
				"approved":    autoApprove,
				"decision":    decision,
				"approval_id": item.ID,
				"comment":     comment,
				"decided_at":  now.Format(time.RFC3339),
				"timed_out":   true,
			}
			if err := host.Resume(item.RuleID, item.StepID, item.EventID, out); err != nil {
				if errors.Is(err, plugin.ErrGone) {
					status = ApprovalExpired
					comment = "超时且对应规则已删除"
				} else {
					// Rule is disabled: keep pending so it can resolve once re-enabled.
					continue
				}
			}
		}

		var updated *ApprovalRequest
		a.mu.Lock()
		if cur, ok := a.requests[item.ID]; ok && cur.Status == ApprovalPending {
			cur.Status = status
			cur.Comment = comment
			t := now
			cur.DecidedAt = &t
			a.saveLocked()
			cp := cloneApproval(cur)
			updated = &cp
		}
		a.mu.Unlock()
		if updated != nil {
			a.notify(*updated)
		}
	}
}

func cloneApproval(r *ApprovalRequest) ApprovalRequest {
	cp := *r
	if r.ExpiresAt != nil {
		t := *r.ExpiresAt
		cp.ExpiresAt = &t
	}
	if r.DecidedAt != nil {
		t := *r.DecidedAt
		cp.DecidedAt = &t
	}
	return cp
}

func boolVal(m map[string]any, key string, def bool) bool {
	switch v := m[key].(type) {
	case bool:
		return v
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "true" || s == "1" || s == "yes" {
			return true
		}
		if s == "false" || s == "0" || s == "no" {
			return false
		}
	}
	return def
}
