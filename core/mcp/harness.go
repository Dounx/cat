package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A Harness is an AI application on this machine that the hub's MCP server
// can be installed into: afterwards the AI there has the hub's tools. Each
// keeps its MCP servers its own way, and is told through that.
type Harness struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// About says where the server ends up, in a few words.
	About string `json:"about"`

	find      func() (string, bool)
	installed func() (url string, ok bool)
	install   func(ctx context.Context, at, url string) error
	uninstall func(ctx context.Context, at string) error
}

// Status is a harness as it stands now.
type Status struct {
	Harness
	// Found reports whether the harness is on this machine.
	Found bool `json:"found"`
	// Installed reports whether the hub's server is in it, and URL where the
	// entry points: another address than this hub's when Stale.
	Installed bool   `json:"installed"`
	URL       string `json:"url,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
}

// Harnesses lists the harnesses the server can be installed into.
func Harnesses() []Harness {
	return []Harness{claude(), codex(), dsh()}
}

// StatusOf reports how a harness stands, given the address of this hub's
// MCP endpoint.
func StatusOf(h Harness, url string) Status {
	st := Status{Harness: h}
	_, st.Found = h.find()
	st.URL, st.Installed = h.installed()
	st.Stale = st.Installed && st.URL != url
	if !st.Installed {
		st.URL = ""
	}
	return st
}

// Install puts the hub's MCP server, reached at url, into the harness,
// replacing an entry left there before.
func Install(ctx context.Context, id, url string) error {
	h, at, err := lookup(id)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, ok := h.installed(); ok {
		if err := h.uninstall(ctx, at); err != nil {
			return err
		}
	}
	if err := h.install(ctx, at, url); err != nil {
		return err
	}
	if got, ok := h.installed(); !ok || got != url {
		return errors.New("装完后没有在 " + h.Name + " 的配置里找到它")
	}
	return nil
}

// Uninstall takes the hub's MCP server out of the harness.
func Uninstall(ctx context.Context, id string) error {
	h, at, err := lookup(id)
	if err != nil {
		return err
	}
	if _, ok := h.installed(); !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := h.uninstall(ctx, at); err != nil {
		return err
	}
	if _, ok := h.installed(); ok {
		return errors.New("卸载后它还在 " + h.Name + " 的配置里")
	}
	return nil
}

func lookup(id string) (Harness, string, error) {
	for _, h := range Harnesses() {
		if h.ID != id {
			continue
		}
		at, ok := h.find()
		if !ok {
			return h, "", errors.New("这台机器上没有找到 " + h.Name)
		}
		return h, at, nil
	}
	return Harness{}, "", errors.New("unknown harness: " + id)
}

// ---- Claude ----

// Claude Desktop's Code tab and the claude command share one configuration:
// the user's MCP servers are under "mcpServers" in ~/.claude.json, and the
// command is how they are changed.
func claude() Harness {
	config := func() string {
		if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
			return filepath.Join(dir, ".claude.json")
		}
		return filepath.Join(home(), ".claude.json")
	}
	return Harness{
		ID: "claude-desktop", Name: "Claude Desktop", About: "装进 claude 命令的用户级 MCP 服务，Claude Desktop 的 Code 和 Claude Code 都能用",
		find: func() (string, bool) { return findBin("claude") },
		installed: func() (string, bool) {
			var doc struct {
				Servers map[string]struct {
					URL string `json:"url"`
				} `json:"mcpServers"`
			}
			raw, err := os.ReadFile(config())
			if err != nil || json.Unmarshal(raw, &doc) != nil {
				return "", false
			}
			s, ok := doc.Servers[ServerName]
			return s.URL, ok
		},
		install: func(ctx context.Context, bin, url string) error {
			return run(ctx, bin, "mcp", "add", "--transport", "http", "--scope", "user", ServerName, url)
		},
		uninstall: func(ctx context.Context, bin string) error {
			return run(ctx, bin, "mcp", "remove", "--scope", "user", ServerName)
		},
	}
}

// ---- ChatGPT (Codex) ----

// The ChatGPT desktop app's Codex and the codex command share
// ~/.codex/config.toml, where each MCP server is a [mcp_servers.<name>]
// table. The hub's table is added and taken out as text. `codex mcp add`
// would do it too, but it writes the whole file anew, and the other entries
// come out changed.
func codex() Harness {
	config := func() string {
		if dir := os.Getenv("CODEX_HOME"); dir != "" {
			return filepath.Join(dir, "config.toml")
		}
		return filepath.Join(home(), ".codex", "config.toml")
	}
	read := func() string {
		raw, _ := os.ReadFile(config())
		return string(raw)
	}
	return Harness{
		ID: "chatgpt-desktop", Name: "ChatGPT Desktop", About: "写进 ~/.codex/config.toml，ChatGPT Desktop 的 Codex 和 codex 命令都能用",
		find: func() (string, bool) {
			if st, err := os.Stat(filepath.Dir(config())); err == nil && st.IsDir() {
				return config(), true
			}
			if _, ok := findBin("codex"); ok {
				return config(), true
			}
			_, ok := firstFile("/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex",
				filepath.Join(home(), "Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"))
			return config(), ok
		},
		installed: func() (string, bool) {
			_, url, ok := tomlCut(read(), "mcp_servers."+ServerName)
			return url, ok
		},
		install: func(_ context.Context, path, url string) error {
			doc, _, _ := tomlCut(read(), "mcp_servers."+ServerName)
			if doc != "" && !strings.HasSuffix(doc, "\n") {
				doc += "\n"
			}
			if doc != "" {
				doc += "\n"
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return writeKeeping(path, doc+"[mcp_servers."+ServerName+"]\nurl = "+strconv.Quote(url)+"\n")
		},
		uninstall: func(_ context.Context, path string) error {
			doc, _, _ := tomlCut(read(), "mcp_servers."+ServerName)
			return writeKeeping(path, doc)
		},
	}
}

// tomlCut takes a table, with the tables under it, out of a TOML document,
// and gives the rest, the table's url key and whether the table was there.
// It reads what it wrote and what the codex command makes of that: a [table]
// header on a line of its own, and the keys beneath it up to the next header.
func tomlCut(doc, table string) (rest, url string, found bool) {
	lines := strings.Split(doc, "\n")
	var out, blank []string // blank: empty lines met inside the table, not yet known whose
	in := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			name := strings.ReplaceAll(strings.Trim(t, "[] \t"), `"`, "")
			in = name == table || strings.HasPrefix(name, table+".")
			if in && name == table {
				found = true
				// the blank line that set the table apart goes with it
				if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
					out = out[:n-1]
				}
			}
			if !in { // the empty lines before another table are that table's
				out = append(out, blank...)
			}
			blank = nil
		}
		switch {
		case !in:
			out = append(out, line)
		case t == "":
			blank = append(blank, line)
		default:
			if key, value, ok := strings.Cut(t, "="); ok && strings.TrimSpace(key) == "url" {
				url = strings.Trim(strings.TrimSpace(value), `"'`)
			}
		}
	}
	out = append(out, blank...)
	if !found {
		return doc, "", false
	}
	rest = strings.Join(out, "\n")
	if strings.HasSuffix(doc, "\n") && rest != "" && !strings.HasSuffix(rest, "\n") {
		rest += "\n"
	}
	return rest, url, true
}

// ---- DeepSeek Harness ----

// DeepSeek Harness keeps a patch file per profile (desktop, web, headless…):
// a YAML list, in which an MCP server is an entry that inserts the MCP client
// plugin. The hub's entry is known by the id it gives that plugin. The
// Harness writes these files anew now and then, and comments standing on
// lines of their own do not survive that, so nothing else marks the entry.
const dshEntry = "cat-mcp"

var dshID = regexp.MustCompile(`^\s+(-\s+)?id:\s*["']?` + dshEntry + `["']?\s*$`)

func dsh() Harness {
	patches := func() []string {
		root := os.Getenv("DSH_HOME")
		if root == "" {
			root = filepath.Join(home(), ".dsh")
		}
		list, _ := filepath.Glob(filepath.Join(root, "profiles", "*", "cordis.patch.yml"))
		return list
	}
	return Harness{
		ID: "deepseek-harness", Name: "DeepSeek Harness", About: "写进 DeepSeek Harness 每个档案（桌面、网页、headless…）的补丁文件，重开后生效",
		find: func() (string, bool) {
			list := patches()
			if len(list) == 0 {
				return "", false
			}
			return filepath.Dir(filepath.Dir(list[0])), true
		},
		// in any profile counts: installing again brings it to the others
		installed: func() (string, bool) {
			url, found := "", false
			for _, p := range patches() {
				raw, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				if _, u, ok := dshCut(string(raw)); ok {
					url, found = u, true
				}
			}
			return url, found
		},
		install: func(_ context.Context, _, url string) error {
			block := "- insert: # cat: added by Cat; uninstall it there, under 接入\n" +
				"    - id: " + dshEntry + "\n" +
				"      name: \"@deepseek-ai/dsh-mcp-client\"\n" +
				"      config:\n" +
				"        serverName: \"" + ServerName + "\"\n" +
				"        transport: \"streamable-http\"\n" +
				"        url: " + strconv.Quote(url) + "\n"
			for _, p := range patches() {
				raw, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				doc, _, _ := dshCut(string(raw))
				if doc != "" && !strings.HasSuffix(doc, "\n") {
					doc += "\n"
				}
				if err := writeKeeping(p, doc+block); err != nil {
					return err
				}
			}
			return nil
		},
		uninstall: func(context.Context, string) error {
			for _, p := range patches() {
				raw, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				if doc, _, ok := dshCut(string(raw)); ok {
					if err := writeKeeping(p, doc); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
}

// dshCut takes the hub's entry out of a patch file: the item of the top-level
// list that holds the plugin with the hub's id. It gives the rest, the url
// the entry had and whether it was there.
func dshCut(doc string) (rest, url string, found bool) {
	lines := strings.Split(doc, "\n")
	at := slices.IndexFunc(lines, dshID.MatchString)
	if at < 0 {
		return doc, "", false
	}
	// an item of the top-level list begins in the first column
	top := func(line string) bool { return line != "" && line[0] != ' ' && line[0] != '\t' }
	from, to := at, at+1
	for from > 0 && !top(lines[from]) {
		from--
	}
	for to < len(lines) && !top(lines[to]) {
		to++
	}
	// blank lines after the entry stay where they are
	for to > at+1 && strings.TrimSpace(lines[to-1]) == "" {
		to--
	}
	for _, line := range lines[from:to] {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), ":"); ok && key == "url" {
			url = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return strings.Join(slices.Concat(lines[:from], lines[to:]), "\n"), url, true
}

// writeKeeping replaces a file's content, keeping its permissions, by way of
// a file beside it: a crash half way leaves the old one whole.
func writeKeeping(path, content string) error {
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	tmp := path + ".cat-tmp"
	if err := os.WriteFile(tmp, []byte(content), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---- finding and running commands ----

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

// moreDirs is where commands installed by the usual package managers live.
// A hub started from the desktop has a PATH without them.
func moreDirs() []string {
	h := home()
	return []string{filepath.Join(h, ".local", "bin"), filepath.Join(h, ".claude", "local"), filepath.Join(h, ".bun", "bin"),
		filepath.Join(h, ".npm-global", "bin"), "/opt/homebrew/bin", "/usr/local/bin"}
}

func findBin(name string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	dirs := moreDirs()
	if runtime.GOOS == "windows" {
		dirs = append(dirs, filepath.Join(os.Getenv("APPDATA"), "npm"))
		name += ".cmd"
	}
	var paths []string
	for _, d := range dirs {
		paths = append(paths, filepath.Join(d, name))
	}
	return firstFile(paths...)
}

func firstFile(paths ...string) (string, bool) {
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	return "", false
}

// run carries out a harness's own command, and gives what it said when it
// fails.
func run(ctx context.Context, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") { // the command may be a script that needs node
			env[i] = kv + string(os.PathListSeparator) + strings.Join(moreDirs(), string(os.PathListSeparator))
		}
	}
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("超时了")
		}
		said := strings.TrimSpace(out.String())
		if len(said) > 400 {
			said = said[len(said)-400:]
		}
		return fmt.Errorf("%s %s 失败：%v %s", filepath.Base(bin), strings.Join(args[:2], " "), err, said)
	}
	return nil
}
