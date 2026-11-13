// Command cat-plugin-codex-desktop hands events to the Codex CLI running on this machine.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/dounx/cat/plugins/internal/harness"
	cat "github.com/dounx/cat/sdk/go"
)

func main() { cat.Run(New()) }

type options struct {
	bin, cwd, sandbox, model, systemPrompt string
}

type result struct {
	Text      string         `json:"text"`
	SessionID string         `json:"session_id"`
	Usage     map[string]any `json:"usage,omitempty"`
}

// args builds the command line. The prompt is read from stdin ("-").
func args(o options, resume, lastMessageFile string) []string {
	a := []string{"exec"}
	if resume != "" {
		a = append(a, "resume")
	}
	a = append(a, "--json", "--skip-git-repo-check", "-c", `sandbox_mode="`+o.sandbox+`"`, "-o", lastMessageFile)
	if o.model != "" {
		a = append(a, "-m", o.model)
	}
	if resume != "" {
		a = append(a, resume)
	}
	return append(a, "-")
}

// parse reads Codex's JSONL event stream.
func parse(stdout []byte) (res result, failure string) {
	sc := bufio.NewScanner(bytes.NewReader(stdout))
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var ev struct {
			Type     string         `json:"type"`
			ThreadID string         `json:"thread_id"`
			Message  string         `json:"message"`
			Usage    map[string]any `json:"usage"`
			Error    struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			res.SessionID = ev.ThreadID
		case "item.completed":
			// Items of type "error" are warnings (e.g. config notices), not failures.
			if ev.Item.Type == "agent_message" {
				res.Text = ev.Item.Text
			}
		case "turn.completed":
			res.Usage = ev.Usage
		case "turn.failed":
			failure = ev.Error.Message
		case "error":
			failure = ev.Message
		}
	}
	return res, failure
}

func run(ctx context.Context, o options, prompt, resume string) (result, error) {
	tmp, err := os.CreateTemp("", "cat-codex-*.txt")
	if err != nil {
		return result{}, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	stdout, stderr, runErr := harness.RunCLI(ctx, o.bin, args(o, resume, tmp.Name()), o.cwd, harness.PathEnv(), prompt)
	res, failure := parse(stdout)
	if resume != "" && res.SessionID == "" {
		res.SessionID = resume
	}
	// The file holds the final message verbatim; prefer it over the stream.
	if b, err := os.ReadFile(tmp.Name()); err == nil && len(bytes.TrimSpace(b)) > 0 {
		res.Text = string(b)
	}
	if failure != "" {
		return res, errors.New("Codex：" + failure)
	}
	if runErr != nil {
		return res, errors.New("Codex 运行失败：" + runErr.Error() + " " + harness.Tail(string(stderr), 400))
	}
	if strings.TrimSpace(res.Text) == "" {
		return res, errors.New("Codex 没有返回内容")
	}
	return res, nil
}

func load(h *cat.Host) (options, error) {
	bin, err := harness.FindBin("codex", harness.Str(h.Config, "bin"))
	if err != nil {
		return options{}, errors.New(err.Error() + "。请先安装 Codex CLI：https://developers.openai.com/codex/cli")
	}
	cwd, err := harness.EnsureDir(harness.Str(h.Config, "cwd"), h.DataDir)
	if err != nil {
		return options{}, err
	}
	sandbox := harness.Str(h.Config, "sandbox")
	switch sandbox {
	case "":
		sandbox = "read-only"
	case "read-only", "workspace-write", "danger-full-access":
	default:
		return options{}, errors.New("未知的沙箱模式：" + sandbox)
	}
	return options{bin: bin, cwd: cwd, sandbox: sandbox, model: harness.Str(h.Config, "model"), systemPrompt: harness.Str(h.Config, "system_prompt")}, nil
}

func version(ctx context.Context, bin string) (string, error) {
	vctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, _, err := harness.RunCLI(vctx, bin, []string{"--version"}, "", harness.PathEnv(), "")
	return strings.TrimSpace(string(out)), err
}

func New() cat.Plugin {
	var (
		opts  options
		guard harness.Guard
		locks harness.Locks
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) (err error) {
			if opts, err = load(h); err != nil {
				return err
			}
			guard = harness.NewGuard(h.Config, false)
			v, err := version(ctx, opts.bin)
			if err != nil {
				return errors.New("无法运行 codex 命令：" + err.Error())
			}
			h.Log("info", "%s，工作目录 %s", v, opts.cwd)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"chat": func(ctx context.Context, h *cat.Host, p map[string]any, actx cat.ActionContext) (any, error) {
				if err := guard.Check(actx); err != nil {
					return nil, err
				}
				prompt := harness.Str(p, "prompt")
				if prompt == "" {
					return nil, errors.New("提示词为空")
				}
				key := harness.SessionKey(p, actx)
				defer locks.Lock(key)()
				resume := ""
				if key != "" && p["new_session"] != true {
					resume, _ = h.StateGet("session:" + key)
				}
				// Codex has no system prompt flag; instructions open the conversation instead.
				first := prompt
				if sys := strings.TrimSpace(opts.systemPrompt + "\n\n" + harness.Str(p, "system_prompt")); sys != "" {
					first = sys + "\n\n" + prompt
				}
				var res result
				var err error
				if resume != "" {
					if res, err = run(ctx, opts, prompt, resume); err != nil && ctx.Err() == nil {
						h.Log("warn", "续接会话 %s 失败（%v），改为新开会话", resume, err)
						res, err = run(ctx, opts, first, "")
					}
				} else {
					res, err = run(ctx, opts, first, "")
				}
				if res.SessionID != "" && key != "" {
					_ = h.StateSet("session:"+key, res.SessionID)
				}
				return res, err
			},
		},
		Setup: setup,
	}
}

func setup(ctx context.Context, s *cat.Setup) (map[string]any, error) {
	cfg := map[string]any{}
	for k, v := range s.Existing {
		cfg[k] = v
	}
	for {
		s.Progress("正在检测 Codex CLI…", -1)
		bin, err := harness.FindBin("codex", harness.Str(cfg, "bin"))
		if err == nil {
			v, verr := version(ctx, bin)
			if verr == nil {
				s.Show(cat.Step{Kind: cat.StepInfo, Title: "已找到 " + v})
				break
			}
			err = verr
		}
		ok, aerr := s.Ask(ctx, cat.Step{
			Kind: cat.StepConfirm, Title: "没有找到 Codex CLI", URL: "https://developers.openai.com/codex/cli",
			Description: err.Error() + "\n\n请先安装并在终端里运行一次 codex 完成登录，然后点确认重新检测。",
		})
		if aerr != nil {
			return nil, aerr
		}
		if c, _ := ok["confirmed"].(bool); !c {
			return nil, cat.ErrCancelled
		}
	}
	values := map[string]any{"sandbox": "read-only"}
	for _, k := range []string{"cwd", "sandbox", "allowed_actors", "allow_anyone"} {
		if v, ok := cfg[k]; ok {
			values[k] = v
		}
	}
	guardSchema := harness.GuardSchema()
	out, err := s.Ask(ctx, cat.Step{
		Kind: cat.StepForm, Title: "它能做什么、谁能用它",
		Description: "聊天消息会原样成为给 Agent 的指令，所以默认使用只读沙箱，并且只接受白名单里的发送者。",
		Values:      values,
		Schema: map[string]any{
			"type": "object", "x-order": []string{"cwd", "sandbox", "allowed_actors", "allow_anyone"},
			"properties": map[string]any{
				"cwd":            map[string]any{"type": "string", "title": "工作目录", "description": "留空使用实例自己的空目录。"},
				"sandbox":        map[string]any{"type": "string", "title": "沙箱", "enum": []string{"read-only", "workspace-write", "danger-full-access"}, "x-enum-titles": []string{"只读（推荐）", "可以改工作目录里的文件", "不做任何限制（危险）"}, "default": "read-only"},
				"allowed_actors": guardSchema["allowed_actors"],
				"allow_anyone":   guardSchema["allow_anyone"],
			},
		},
	})
	if err != nil {
		return nil, err
	}
	for k, v := range out {
		cfg[k] = v
	}
	if _, err := harness.EnsureDir(harness.Str(cfg, "cwd"), s.DataDir); err != nil {
		return nil, err
	}
	return cfg, nil
}
