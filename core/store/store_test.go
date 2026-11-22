package store

import (
	"testing"
	"time"
)

func TestRuleStats(t *testing.T) {
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)

	runs := []Run{
		// r1: 2 ok runs in 24h (OpenAI + Anthropic usage), 1 old run >24h ago, 1 replay run (ignored)
		{
			ID:        "0001",
			RuleID:    "r1",
			RuleName:  "AI Bot",
			EventID:   "e1",
			Status:    "ok",
			StartedAt: now.Add(-30 * time.Hour),
			Steps: []StepResult{{
				ID:     "s1",
				Output: map[string]any{"usage": map[string]any{"prompt_tokens": 100, "completion_tokens": 50}},
			}},
		},
		{
			ID:        "0002",
			RuleID:    "r1",
			RuleName:  "AI Bot",
			EventID:   "e2",
			Status:    "ok",
			StartedAt: now.Add(-2 * time.Hour),
			Steps: []StepResult{{
				ID:     "s1",
				Output: map[string]any{"usage": map[string]any{"prompt_tokens": 420, "completion_tokens": 185, "total_tokens": 605}},
			}},
		},
		{
			ID:        "0003",
			RuleID:    "r1",
			RuleName:  "AI Bot",
			EventID:   "e3",
			Status:    "ok",
			StartedAt: now.Add(-10 * time.Minute),
			Steps: []StepResult{
				{
					ID:     "s1",
					Output: map[string]any{"usage": map[string]any{"input_tokens": 300, "output_tokens": 90}},
				},
				{
					// Non-object usage must be ignored safely
					ID:     "s2",
					Output: map[string]any{"usage": "n/a"},
				},
			},
		},
		{
			ID:        "0004",
			RuleID:    "r1",
			RuleName:  "AI Bot",
			EventID:   "e3",
			Status:    "error",
			StartedAt: now.Add(-5 * time.Minute),
			Replay:    true,
			Steps: []StepResult{{
				ID:     "s1",
				Output: map[string]any{"usage": map[string]any{"prompt_tokens": 9999, "completion_tokens": 1}},
			}},
		},
		// r2: 1 error, 1 interrupted, 1 running, 1 cancelled within 24h
		{
			ID:        "0005",
			RuleID:    "r2",
			RuleName:  "Alerts",
			EventID:   "e4",
			Status:    "error",
			StartedAt: now.Add(-4 * time.Hour),
		},
		{
			ID:        "0006",
			RuleID:    "r2",
			RuleName:  "Alerts",
			EventID:   "e5",
			Status:    "interrupted",
			StartedAt: now.Add(-3 * time.Hour),
		},
		{
			ID:        "0007",
			RuleID:    "r2",
			RuleName:  "Alerts",
			EventID:   "e6",
			Status:    "cancelled",
			StartedAt: now.Add(-2 * time.Hour),
		},
		{
			ID:        "0008",
			RuleID:    "r2",
			RuleName:  "Alerts",
			EventID:   "e7",
			Status:    "running",
			StartedAt: now.Add(-1 * time.Minute),
		},
		// Draft run without RuleID should be ignored
		{
			ID:        "0009",
			RuleID:    "",
			RuleName:  "Draft",
			EventID:   "e8",
			Status:    "ok",
			StartedAt: now,
		},
	}
	for i := range runs {
		if err := st.SaveRun(&runs[i]); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RuleStats(since)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rules, want 2: %+v", len(got), got)
	}

	r1 := got["r1"]
	if r1.Runs24h != 2 || r1.OK24h != 2 || r1.Failed24h != 0 || r1.Running != 0 {
		t.Fatalf("r1 counts = %+v", r1)
	}
	if r1.TotalRuns != 3 {
		t.Fatalf("r1 total_runs = %d, want 3", r1.TotalRuns)
	}
	if r1.LastStatus != "ok" || r1.LastRunAt == nil || !r1.LastRunAt.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("r1 last = %v %q", r1.LastRunAt, r1.LastStatus)
	}
	if r1.Tokens24h.Prompt != 720 || r1.Tokens24h.Completion != 275 || r1.Tokens24h.Total != 995 {
		t.Fatalf("r1 tokens_24h = %+v, want {720 275 995}", r1.Tokens24h)
	}
	if r1.TotalTokens != 1145 {
		t.Fatalf("r1 total_tokens = %d, want 1145", r1.TotalTokens)
	}

	r2 := got["r2"]
	if r2.Runs24h != 4 || r2.OK24h != 0 || r2.Failed24h != 2 || r2.Running != 1 || r2.TotalRuns != 4 {
		t.Fatalf("r2 counts = %+v", r2)
	}
	if r2.LastStatus != "running" || r2.LastRunAt == nil || !r2.LastRunAt.Equal(now.Add(-1*time.Minute)) {
		t.Fatalf("r2 last = %v %q", r2.LastRunAt, r2.LastStatus)
	}
}
