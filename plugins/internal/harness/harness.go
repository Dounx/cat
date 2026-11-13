// Package harness holds what the conversation plugins (harnesses and LLMs) share:
// the chat contract, session bookkeeping and a helper for running harness
// CLIs.
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

// Choices reads what a "judge" action may pick from: the list a rule's
// condition node sends, or, when the action is used as an ordinary step, a
// comma or newline separated string. Empty means the question is yes or no.
func Choices(p map[string]any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if list, ok := p["choices"].([]any); ok {
		for _, c := range list {
			add(fmt.Sprint(c))
		}
		return out
	}
	for _, c := range strings.FieldsFunc(Str(p, "choices"), func(r rune) bool { return r == ',' || r == '，' || r == '\n' }) {
		add(c)
	}
	return out
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

// EnsureDir resolves the working directory for a harness, creating the default
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
