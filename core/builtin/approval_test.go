package builtin

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

func TestApprovalApproveAndReject(t *testing.T) {
	now := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	host := newRecordHost(ApprovalID)
	a := &approval{now: func() time.Time { return now }, requests: map[string]*ApprovalRequest{}}
	var notified []ApprovalRequest
	a.SetApprovalNotifier(func(r ApprovalRequest) {
		notified = append(notified, r)
	})
	if err := a.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	// 1. Request approval -> ErrHeld + persisted pending + SSE notified.
	ictx := plugin.InvokeContext{
		RuleID:   "rule-deploy",
		RuleName: "生产部署",
		StepID:   "gate-1",
		RunID:    "run-1",
		Event:    &event.Event{ID: "ev-1"},
	}
	out, err := a.Invoke(context.Background(), "request", map[string]any{
		"title":           "确认执行部署",
		"detail":          "发布 v1.2.3 到生产环境",
		"timeout_minutes": 30,
	}, ictx)
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatalf("expected ErrHeld, got err=%v out=%+v", err, out)
	}
	reqID, _ := out["approval_id"].(string)
	if reqID == "" || out["status"] != "pending" || !strings.Contains(out["note"].(string), "确认执行部署") {
		t.Fatalf("unexpected held output: %+v", out)
	}
	if len(notified) != 1 || notified[0].Status != ApprovalPending || notified[0].RuleName != "生产部署" {
		t.Fatalf("unexpected notifications: %+v", notified)
	}

	// 2. Approve -> resumes downstream and notifies SSE.
	now = now.Add(2 * time.Minute)
	decided, err := a.DecideApproval(reqID, "approve", "已核对变更清单")
	if err != nil {
		t.Fatalf("approve failed: %v", err)
	}
	if decided.Status != ApprovalApproved || decided.Comment != "已核对变更清单" || decided.DecidedAt == nil {
		t.Fatalf("unexpected decided record: %+v", decided)
	}
	if host.resumeCount() != 1 {
		t.Fatalf("expected 1 resume on approve, got %d", host.resumeCount())
	}
	call := host.resumes[0]
	if call.RuleID != "rule-deploy" || call.StepID != "gate-1" || call.EventID != "ev-1" {
		t.Fatalf("unexpected resume target: %+v", call)
	}
	if call.Output["approved"] != true || call.Output["decision"] != "approve" || call.Output["comment"] != "已核对变更清单" {
		t.Fatalf("unexpected resume output: %+v", call.Output)
	}
	if _, err := a.DecideApproval(reqID, "reject", ""); !errors.Is(err, ErrApprovalAlreadyDecided) {
		t.Fatalf("expected ErrApprovalAlreadyDecided, got %v", err)
	}

	// 3. Reject with default resume_on_reject=false -> does NOT resume downstream.
	out2, err := a.Invoke(context.Background(), "request", map[string]any{"title": "删除数据库"}, ictx)
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	reqID2 := out2["approval_id"].(string)
	decided2, err := a.DecideApproval(reqID2, "reject", "风险过高")
	if err != nil || decided2.Status != ApprovalRejected {
		t.Fatalf("reject failed: req=%+v err=%v", decided2, err)
	}
	if host.resumeCount() != 1 {
		t.Fatalf("expected NO resume on default reject, got %d resumes", host.resumeCount())
	}

	// 4. Reject with resume_on_reject=true -> resumes downstream with approved=false.
	out3, err := a.Invoke(context.Background(), "request", map[string]any{
		"title":            "审批分流测试",
		"resume_on_reject": true,
	}, ictx)
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	reqID3 := out3["approval_id"].(string)
	decided3, err := a.DecideApproval(reqID3, "reject", "退回修改")
	if err != nil || decided3.Status != ApprovalRejected {
		t.Fatalf("reject with resume_on_reject failed: req=%+v err=%v", decided3, err)
	}
	if host.resumeCount() != 2 {
		t.Fatalf("expected resume on reject with resume_on_reject=true, got %d", host.resumeCount())
	}
	if host.resumes[1].Output["approved"] != false || host.resumes[1].Output["decision"] != "reject" {
		t.Fatalf("unexpected reject resume output: %+v", host.resumes[1].Output)
	}
}

func TestApprovalPersistAndRestartRecovery(t *testing.T) {
	now := time.Date(2026, 11, 22, 11, 0, 0, 0, time.UTC)
	host := newRecordHost(ApprovalID)

	a1 := &approval{now: func() time.Time { return now }, requests: map[string]*ApprovalRequest{}}
	if err := a1.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	out, err := a1.Invoke(context.Background(), "request", map[string]any{
		"title":           "重启恢复测试",
		"detail":          "重启后仍可确认",
		"timeout_minutes": 0,
	}, plugin.InvokeContext{RuleID: "r1", StepID: "s1", Event: &event.Event{ID: "e1"}})
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	reqID := out["approval_id"].(string)
	_ = a1.Stop()

	a2 := &approval{now: func() time.Time { return now }, requests: map[string]*ApprovalRequest{}}
	if err := a2.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer a2.Stop()

	list := a2.ListApprovals()
	if len(list) != 1 || list[0].ID != reqID || list[0].Status != ApprovalPending || list[0].ExpiresAt != nil {
		t.Fatalf("reloaded approvals mismatch: %+v", list)
	}
	if _, err := a2.DecideApproval(reqID, "approve", "ok"); err != nil {
		t.Fatalf("approve after restart failed: %v", err)
	}
	if host.resumeCount() != 1 {
		t.Fatalf("expected resume after restart, got %d", host.resumeCount())
	}
}

func TestApprovalTimeout(t *testing.T) {
	origTick := ApprovalTick
	ApprovalTick = 20 * time.Millisecond
	defer func() { ApprovalTick = origTick }()

	var mu sync.Mutex
	now := time.Date(2026, 11, 22, 14, 0, 0, 0, time.UTC)
	getNow := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}

	host := newRecordHost(ApprovalID)
	a := &approval{now: getNow, requests: map[string]*ApprovalRequest{}}
	if err := a.Start(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	defer a.Stop()

	outReject, err := a.Invoke(context.Background(), "request", map[string]any{
		"title":           "超时拒绝",
		"timeout_minutes": 1,
		"on_timeout":      "reject",
	}, plugin.InvokeContext{RuleID: "r1", StepID: "s1", Event: &event.Event{ID: "e1"}})
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	idReject := outReject["approval_id"].(string)

	outApprove, err := a.Invoke(context.Background(), "request", map[string]any{
		"title":           "超时自动同意",
		"timeout_minutes": 1,
		"on_timeout":      "approve",
	}, plugin.InvokeContext{RuleID: "r2", StepID: "s2", Event: &event.Event{ID: "e2"}})
	if !errors.Is(err, plugin.ErrHeld) {
		t.Fatal(err)
	}
	idApprove := outApprove["approval_id"].(string)

	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r1, _ := a.GetApproval(idReject)
		r2, _ := a.GetApproval(idApprove)
		if r1.Status == ApprovalExpired && r2.Status == ApprovalApproved {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	r1, _ := a.GetApproval(idReject)
	r2, _ := a.GetApproval(idApprove)
	if r1.Status != ApprovalExpired {
		t.Fatalf("expected expired status for timeout reject, got %+v", r1)
	}
	if r2.Status != ApprovalApproved {
		t.Fatalf("expected approved status for timeout approve, got %+v", r2)
	}
	if host.resumeCount() != 1 || host.resumes[0].RuleID != "r2" || host.resumes[0].Output["timed_out"] != true {
		t.Fatalf("unexpected timeout resumes: %+v", host.resumes)
	}
}
