package store

import (
	"bytes"
	"encoding/json"
	"time"
)

// TokenUsage aggregates LLM token counts recorded in step outputs.
type TokenUsage struct {
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
	Total      int64 `json:"total"`
}

// RuleStats summarizes recent run health and token consumption for one rule.
type RuleStats struct {
	RuleID      string     `json:"rule_id"`
	Runs24h     int        `json:"runs_24h"`
	OK24h       int        `json:"ok_24h"`
	Failed24h   int        `json:"failed_24h"`
	Running     int        `json:"running"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	LastStatus  string     `json:"last_status,omitempty"`
	Tokens24h   TokenUsage `json:"tokens_24h"`
	TotalRuns   int        `json:"total_runs"`
	TotalTokens int64      `json:"total_tokens"`
}

type runMetricRow struct {
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	Replay    bool      `json:"replay"`
	Steps     []struct {
		Output struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"output"`
	} `json:"steps"`
}

// RuleStats aggregates per-rule health metrics and token usage across stored
// non-replay runs. Windowed counters (Runs24h, OK24h, Failed24h, Tokens24h)
// count runs started at or after since.
func (s *Store) RuleStats(since time.Time) (map[string]RuleStats, error) {
	rows, err := s.db.Query(`SELECT id, rule_id, data FROM runs WHERE rule_id != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]RuleStats{}
	lastIDs := map[string]string{}
	for rows.Next() {
		var id, ruleID, data string
		if err := rows.Scan(&id, &ruleID, &data); err != nil {
			return nil, err
		}
		var r runMetricRow
		if err := json.Unmarshal([]byte(data), &r); err != nil {
			continue
		}
		if r.Replay {
			continue
		}
		st := out[ruleID]
		st.RuleID = ruleID
		st.TotalRuns++
		if r.Status == "running" {
			st.Running++
		}
		if !r.StartedAt.IsZero() && (st.LastRunAt == nil || r.StartedAt.After(*st.LastRunAt) || (r.StartedAt.Equal(*st.LastRunAt) && id > lastIDs[ruleID])) {
			ts := r.StartedAt
			st.LastRunAt = &ts
			st.LastStatus = r.Status
			lastIDs[ruleID] = id
		}

		var runUsage TokenUsage
		for _, step := range r.Steps {
			u := parseTokenUsage(step.Output.Usage)
			runUsage.Prompt += u.Prompt
			runUsage.Completion += u.Completion
			runUsage.Total += u.Total
		}
		st.TotalTokens += runUsage.Total

		if !r.StartedAt.IsZero() && !r.StartedAt.Before(since) {
			st.Runs24h++
			switch r.Status {
			case "ok":
				st.OK24h++
			case "error", "interrupted":
				st.Failed24h++
			}
			st.Tokens24h.Prompt += runUsage.Prompt
			st.Tokens24h.Completion += runUsage.Completion
			st.Tokens24h.Total += runUsage.Total
		}
		out[ruleID] = st
	}
	return out, rows.Err()
}

// parseTokenUsage extracts prompt/completion/total token counts from a step's
// usage JSON object. It supports both OpenAI-style (prompt_tokens,
// completion_tokens, total_tokens) and Anthropic/Codex-style (input_tokens,
// output_tokens) field names.
func parseTokenUsage(raw json.RawMessage) TokenUsage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return TokenUsage{}
	}
	var m map[string]any
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return TokenUsage{}
	}
	prompt := int64Field(m, "prompt_tokens")
	if prompt == 0 {
		prompt = int64Field(m, "input_tokens")
	}
	completion := int64Field(m, "completion_tokens")
	if completion == 0 {
		completion = int64Field(m, "output_tokens")
	}
	total := int64Field(m, "total_tokens")
	if total == 0 {
		total = prompt + completion
	}
	return TokenUsage{Prompt: prompt, Completion: completion, Total: total}
}

func int64Field(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	var n int64
	switch x := v.(type) {
	case float64:
		n = int64(x)
	case int64:
		n = x
	case int:
		n = int64(x)
	case json.Number:
		if parsed, err := x.Int64(); err == nil {
			n = parsed
		}
	}
	if n < 0 {
		return 0
	}
	return n
}
