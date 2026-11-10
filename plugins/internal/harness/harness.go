// Package harness holds what the conversation plugins (agents and LLMs) share:
// the chat contract, session bookkeeping, the sender allow-list and a helper
// for running agent CLIs.
package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	cat "github.com/dounx/cat/sdk/go"
)

// Str returns a trimmed string from a config or params map.
func Str(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	if m[key] == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(m[key]))
}

// List splits a comma/space/newline separated config value.
func List(m map[string]any, key string) []string {
	return strings.FieldsFunc(Str(m, key), func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t' })
}

// CommaList splits on commas only, keeping spaces inside items — tool rules
// such as "Bash(git status)" contain them.
func CommaList(m map[string]any, key string) []string {
	var out []string
	for _, item := range strings.FieldsFunc(Str(m, key), func(r rune) bool { return r == ',' || r == '，' || r == '\n' }) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// SessionKey decides which conversation a chat call belongs to: an explicit
// session_key param, else the conversation the triggering event came from.
// Empty means "no continuity".
func SessionKey(p map[string]any, actx cat.ActionContext) string {
	if k := Str(p, "session_key"); k != "" {
		return k
	}
	if ev := actx.Event; ev != nil && ev.Conversation != nil && ev.Conversation.ID != "" {
		return ev.Source + ":" + ev.Conversation.ID
	}
	return ""
}

// Guard restricts who may trigger an agent. Chat platforms let anyone who can
// message the bot inject a prompt, so agents that can act on this machine
// refuse senders that are not listed, unless explicitly opened to everyone.
type Guard struct {
	AllowAnyone bool
	Allowed     map[string]bool
}

// NewGuard reads allow_anyone / allowed_actors from config. openByDefault
// applies when neither is set (true for tool-less LLM chat).
func NewGuard(cfg map[string]any, openByDefault bool) Guard {
	g := Guard{Allowed: map[string]bool{}}
	for _, id := range List(cfg, "allowed_actors") {
		g.Allowed[id] = true
	}
	if v, ok := cfg["allow_anyone"].(bool); ok {
		g.AllowAnyone = v
	} else {
		g.AllowAnyone = openByDefault && len(g.Allowed) == 0
	}
	return g
}

// Check rejects events from senders outside the allow-list. Events without a
// sender (webhooks, schedules) are not chat messages and pass.
func (g Guard) Check(actx cat.ActionContext) error {
	if g.AllowAnyone || actx.Event == nil || actx.Event.Actor == nil {
		return nil
	}
	id := actx.Event.Actor.ID
	if g.Allowed[id] {
		return nil
	}
	return fmt.Errorf("发送者 %s（%s）不在白名单中。要允许，请把这个 ID 加到该实例的「允许的发送者」", id, actx.Event.Actor.Name)
}

// GuardSchema is the config_schema fragment for the allow-list.
func GuardSchema() map[string]any {
	return map[string]any{
		"allowed_actors": map[string]any{"type": "string", "title": "允许的发送者 ID", "description": "逗号分隔。只有这些人发的消息能触发。发送者 ID 可在「事件」页的事件详情里看到。"},
		"allow_anyone":   map[string]any{"type": "boolean", "title": "允许任何人触发（有风险：任何能给机器人发消息的人都能驱动它）"},
	}
}

// Locks serialises work per key, so two messages in one conversation do not
// race on the same session.
type Locks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

func (l *Locks) Lock(key string) (unlock func()) {
	if key == "" {
		return func() {}
	}
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*sync.Mutex{}
	}
	if l.m[key] == nil {
		l.m[key] = &sync.Mutex{}
	}
	k := l.m[key]
	l.mu.Unlock()
	k.Lock()
	return k.Unlock
}

// FindBin locates a CLI. Desktop apps start with a minimal PATH, so the usual
// install locations are searched as well. explicit, when set, wins.
func FindBin(name, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("找不到 %s", explicit)
		}
		return explicit, nil
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	dirs := []string{
		filepath.Join(home, ".local", "bin"), filepath.Join(home, ".claude", "local"),
		filepath.Join(home, ".bun", "bin"), filepath.Join(home, ".npm-global", "bin"),
		"/opt/homebrew/bin", "/usr/local/bin",
	}
	if runtime.GOOS == "windows" {
		dirs = append(dirs, filepath.Join(os.Getenv("APPDATA"), "npm"))
		name += ".cmd"
	}
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("没有找到 %s 命令", strings.TrimSuffix(name, ".cmd"))
}

// PathEnv returns the environment with the usual install locations appended
// to PATH, so a CLI found by FindBin can in turn find node, git and friends.
func PathEnv(extra ...string) []string {
	home, _ := os.UserHomeDir()
	more := []string{"/opt/homebrew/bin", "/usr/local/bin", filepath.Join(home, ".local", "bin"), filepath.Join(home, ".bun", "bin")}
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = kv + string(os.PathListSeparator) + strings.Join(more, string(os.PathListSeparator))
		}
	}
	return append(env, extra...)
}

// RunCLI runs a command with stdin and returns its output. The command is
// killed when ctx ends.
func RunCLI(ctx context.Context, bin string, args []string, dir string, env []string, stdin string) (stdout, stderr []byte, err error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	if ctx.Err() != nil {
		err = errors.New("已超时或被取消")
	}
	return out.Bytes(), errb.Bytes(), err
}

// Tail returns the last n bytes of s, for error messages.
func Tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// EnsureDir resolves the working directory for an agent, creating the default
// (a "workspace" folder in the instance's data dir) when none is configured.
func EnsureDir(configured, dataDir string) (string, error) {
	dir := configured
	if strings.HasPrefix(dir, "~") {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, strings.TrimPrefix(dir, "~"))
	}
	if dir == "" {
		dir = filepath.Join(dataDir, "workspace")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("工作目录不存在：%s", dir)
	}
	return dir, nil
}
