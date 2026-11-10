// Package claudecode runs Claude Code in headless mode. It is shared by the
// Claude plugin and by any plugin that points Claude Code at a compatible
// endpoint.
package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dounx/cat/plugins/internal/harness"
)

type Options struct {
	Bin            string // path to the claude executable
	Cwd            string
	PermissionMode string   // dontAsk | acceptEdits | auto | bypassPermissions
	AllowedTools   []string // tools usable without asking
	Model          string
	SystemPrompt   string   // appended to Claude Code's own
	Env            []string // extra KEY=VALUE entries
}

type Result struct {
	Text       string  `json:"text"`
	SessionID  string  `json:"session_id"`
	CostUSD    float64 `json:"cost_usd"`
	Turns      int     `json:"turns"`
	DurationMS int64   `json:"duration_ms"`
}

// Args builds the command line. The prompt is passed on stdin, never as an
// argument, so message text can not be mistaken for a flag.
func Args(o Options, resume string) []string {
	args := []string{"-p", "--output-format", "json"}
	if len(o.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(o.AllowedTools, ","))
	}
	mode := o.PermissionMode
	if mode == "" {
		mode = "dontAsk"
	}
	args = append(args, "--permission-mode", mode)
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", o.SystemPrompt)
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	return args
}

// Run sends one prompt. With resume set it continues that session; if the
// session can no longer be found it starts a fresh one instead.
func Run(ctx context.Context, o Options, prompt, resume string) (Result, error) {
	res, err := run(ctx, o, prompt, resume)
	if err != nil && resume != "" && strings.Contains(strings.ToLower(err.Error()), "no conversation found") {
		return run(ctx, o, prompt, "")
	}
	return res, err
}

func run(ctx context.Context, o Options, prompt, resume string) (Result, error) {
	stdout, stderr, runErr := harness.RunCLI(ctx, o.Bin, Args(o, resume), o.Cwd, harness.PathEnv(o.Env...), prompt)
	var out struct {
		Type       string  `json:"type"`
		IsError    bool    `json:"is_error"`
		Result     string  `json:"result"`
		SessionID  string  `json:"session_id"`
		TotalCost  float64 `json:"total_cost_usd"`
		NumTurns   int     `json:"num_turns"`
		DurationMS int64   `json:"duration_ms"`
		Subtype    string  `json:"subtype"`
	}
	if err := json.Unmarshal(lastJSONLine(stdout), &out); err != nil || out.Type != "result" {
		if runErr != nil {
			detail := harness.Tail(string(stderr), 400)
			if detail == "" {
				detail = harness.Tail(string(stdout), 400)
			}
			return Result{}, fmt.Errorf("Claude Code 运行失败：%v %s", runErr, detail)
		}
		return Result{}, errors.New("无法解析 Claude Code 的输出：" + harness.Tail(string(stdout), 300))
	}
	res := Result{Text: out.Result, SessionID: out.SessionID, CostUSD: out.TotalCost, Turns: out.NumTurns, DurationMS: out.DurationMS}
	if out.IsError {
		msg := out.Result
		if msg == "" {
			msg = out.Subtype
		}
		return res, errors.New("Claude Code：" + msg)
	}
	return res, nil
}

// lastJSONLine tolerates warnings printed before the result object.
func lastJSONLine(b []byte) []byte {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); strings.HasPrefix(l, "{") {
			return []byte(l)
		}
	}
	return b
}

// Version returns `claude --version`, used to check the CLI works.
func Version(ctx context.Context, bin string) (string, error) {
	out, _, err := harness.RunCLI(ctx, bin, []string{"--version"}, "", harness.PathEnv(), "")
	return strings.TrimSpace(string(out)), err
}
