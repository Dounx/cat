// Package pluginhost runs external plugins: executables described by a
// plugin.json that speak JSON-RPC over stdio. It adapts them to the engine's
// plugin interfaces, supervises their processes and drives setup wizards.
package pluginhost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/sdk/go/jsonrpc"
)

// ProtocolVersion is the plugin protocol this host speaks.
const ProtocolVersion = 1

// Manifest is plugin.json.
type Manifest struct {
	plugin.Manifest
	Protocol int `json:"protocol"`
	// Command starts the plugin. A first element beginning with "./" is
	// resolved against the plugin's directory; anything else through PATH.
	Command      []string     `json:"command"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
	Dir          string       `json:"-"`
}

// Host loads plugin manifests and runs plugin processes.
type Host struct {
	eng  *engine.Engine
	dirs []string
	Deps *Deps

	mu        sync.Mutex
	manifests map[string]*Manifest
	setups    map[string]*setupSession
}

// New creates a host that looks for plugins in each of dirs (every
// subdirectory containing a plugin.json is one plugin).
func New(eng *engine.Engine, dirs ...string) *Host {
	h := &Host{eng: eng, dirs: dirs, manifests: map[string]*Manifest{}, setups: map[string]*setupSession{}}
	h.Deps = &Deps{Dir: filepath.Join(eng.DataDir(), "deps")}
	return h
}

// LoadManifest reads and validates dir/plugin.json.
func LoadManifest(dir string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("plugin.json: %w", err)
	}
	if m.ID == "" || len(m.Command) == 0 {
		return nil, errors.New("plugin.json: id and command are required")
	}
	if strings.ContainsAny(m.ID, `/\. `) {
		return nil, fmt.Errorf("plugin.json: invalid id %q", m.ID)
	}
	if m.Protocol > ProtocolVersion {
		return nil, fmt.Errorf("plugin %s needs protocol %d; this host speaks %d", m.ID, m.Protocol, ProtocolVersion)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	m.Builtin, m.Singleton = false, false
	m.Dir, _ = filepath.Abs(dir)
	return &m, nil
}

// Scan (re)loads manifests from the plugin directories and registers them
// with the engine. Earlier directories win when ids collide.
func (h *Host) Scan() []error {
	found := map[string]*Manifest{}
	var errs []error
	for _, root := range h.dirs {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			dir := filepath.Join(root, ent.Name())
			if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err != nil {
				continue
			}
			m, err := LoadManifest(dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", dir, err))
				continue
			}
			if _, dup := found[m.ID]; !dup {
				found[m.ID] = m
			}
		}
	}
	h.mu.Lock()
	old := h.manifests
	h.manifests = found
	h.mu.Unlock()
	for id := range old {
		if _, still := found[id]; !still {
			h.eng.UnregisterPlugin(id)
		}
	}
	for _, m := range found {
		m := m
		h.eng.RegisterPlugin(m.Manifest, func(cfg map[string]any) (plugin.Instance, error) {
			return &instance{ph: h, m: m, cfg: cfg}, nil
		})
	}
	return errs
}

func (h *Host) manifest(id string) (*Manifest, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.manifests[id]
	return m, ok
}

// Manifests returns the loaded external plugins.
func (h *Host) Manifests() []*Manifest {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Manifest, 0, len(h.manifests))
	for _, m := range h.manifests {
		out = append(out, m)
	}
	return out
}

// ---- processes ----

type proc struct {
	cmd  *exec.Cmd
	conn *jsonrpc.Conn
	in   io.WriteCloser
	done chan struct{} // closed when the process has exited
}

// spawn starts the plugin process. register installs the host-side handlers
// before any message is read. logf receives the plugin's stderr lines.
func (h *Host) spawn(m *Manifest, register func(*jsonrpc.Conn), logf func(string)) (*proc, error) {
	name := m.Command[0]
	if strings.HasPrefix(name, "./") || strings.HasPrefix(name, `.\`) {
		name = filepath.Join(m.Dir, name)
	}
	cmd := exec.Command(name, m.Command[1:]...)
	cmd.Dir = m.Dir
	isolate(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动插件失败：%w", err)
	}
	p := &proc{cmd: cmd, conn: jsonrpc.New(stdout, stdin), in: stdin, done: make(chan struct{})}
	register(p.conn)
	var pipes sync.WaitGroup
	pipes.Add(2)
	go func() {
		defer pipes.Done()
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			logf(sc.Text())
		}
	}()
	go func() {
		defer pipes.Done()
		_ = p.conn.Serve()
	}()
	go func() {
		pipes.Wait()
		_ = cmd.Wait()
		p.conn.Close()
		close(p.done)
	}()
	return p, nil
}

// shutdown asks the plugin to stop, then makes sure its process tree is gone.
func (p *proc) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = p.conn.Call(ctx, "stop", nil, nil)
	cancel()
	_ = p.in.Close()
	select {
	case <-p.done:
	case <-time.After(3 * time.Second):
	}
	killTree(p.cmd)
}

// ---- instances ----

type instance struct {
	ph  *Host
	m   *Manifest
	cfg map[string]any

	host plugin.Host

	mu      sync.Mutex
	p       *proc
	stopped bool
}

func (i *instance) handlers(conn *jsonrpc.Conn) {
	conn.Handle("event.emit", func(_ context.Context, raw json.RawMessage) (any, error) {
		ev, err := decodeEvent(raw)
		if err != nil {
			return nil, err
		}
		id, err := i.host.Emit(ev)
		return map[string]string{"id": id}, err
	})
	conn.Handle("log", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Level, Message string }
		_ = json.Unmarshal(raw, &in)
		i.host.Log(in.Level, in.Message)
		return nil, nil
	})
	conn.Handle("status", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Status, Message string }
		_ = json.Unmarshal(raw, &in)
		if in.Status == engine.StatusRunning || in.Status == engine.StatusError {
			i.host.SetStatus(in.Status, in.Message)
		}
		return nil, nil
	})
	conn.Handle("state.get", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Key string }
		_ = json.Unmarshal(raw, &in)
		v, ok := i.host.StateGet(in.Key)
		return map[string]any{"value": v, "found": ok}, nil
	})
	conn.Handle("state.set", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Key, Value string }
		_ = json.Unmarshal(raw, &in)
		return nil, i.host.StateSet(in.Key, in.Value)
	})
	conn.Handle("host.dependency", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Name string }
		_ = json.Unmarshal(raw, &in)
		path, err := i.ph.ensureDep(ctx, i.m, in.Name, nil)
		return map[string]string{"path": path}, err
	})
}

func (i *instance) boot() error {
	p, err := i.ph.spawn(i.m, i.handlers, func(line string) { i.host.Log("info", line) })
	if err != nil {
		return err
	}
	fail := func(err error) error {
		p.shutdown()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err = p.conn.Call(ctx, "initialize", map[string]any{
		"protocol": ProtocolVersion, "mode": "run",
		"instance": map[string]string{"id": i.host.InstanceID()},
		"config":   i.cfg, "data_dir": i.host.DataDir(),
	}, nil)
	cancel()
	if err != nil {
		return fail(fmt.Errorf("插件初始化失败：%w", err))
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
	err = p.conn.Call(ctx, "start", nil, nil)
	cancel()
	if err != nil {
		return fail(err)
	}
	i.mu.Lock()
	stopped := i.stopped
	if !stopped {
		i.p = p
	}
	i.mu.Unlock()
	if stopped { // stopped while (re)starting
		p.shutdown()
		return errors.New("实例已停止")
	}
	return nil
}

func (i *instance) Start(ctx context.Context, host plugin.Host) error {
	i.host = host
	if err := i.boot(); err != nil {
		return err
	}
	go i.supervise(ctx)
	return nil
}

// supervise restarts the plugin process, with backoff, whenever it dies.
func (i *instance) supervise(ctx context.Context) {
	backoff := time.Second
	for {
		i.mu.Lock()
		p := i.p
		i.mu.Unlock()
		started := time.Now()
		select {
		case <-ctx.Done():
			return
		case <-p.done:
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		for {
			i.mu.Lock()
			stopped := i.stopped
			i.mu.Unlock()
			if stopped {
				return
			}
			i.host.SetStatus(engine.StatusError, fmt.Sprintf("插件进程已退出，%s 后重启", backoff))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < time.Minute {
				backoff *= 2
			}
			if err := i.boot(); err != nil {
				i.host.SetStatus(engine.StatusError, "重启失败："+err.Error())
				continue
			}
			i.host.SetStatus(engine.StatusRunning, "")
			break
		}
	}
}

func (i *instance) Stop() error {
	i.mu.Lock()
	i.stopped = true
	p := i.p
	i.mu.Unlock()
	if p != nil {
		p.shutdown()
	}
	return nil
}

func (i *instance) conn() (*jsonrpc.Conn, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.p == nil {
		return nil, errors.New("插件未运行")
	}
	select {
	case <-i.p.done:
		return nil, errors.New("插件进程已退出")
	default:
	}
	return i.p.conn, nil
}

func (i *instance) Invoke(ctx context.Context, action string, params map[string]any, ictx plugin.InvokeContext) (map[string]any, error) {
	conn, err := i.conn()
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = conn.Call(ctx, "action.invoke", map[string]any{
		"action": action, "params": params,
		"context": map[string]any{"event": ictx.Event, "rule_id": ictx.RuleID, "run_id": ictx.RunID},
	}, &out)
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) && len(rpcErr.Data) > 0 {
		_ = json.Unmarshal(rpcErr.Data, &out) // partial output of a failed action
	}
	return out, err
}

func (i *instance) HandleIngress(ctx context.Context, req plugin.IngressRequest) (plugin.IngressResponse, error) {
	conn, err := i.conn()
	if err != nil {
		return plugin.IngressResponse{Status: 503}, nil
	}
	var resp plugin.IngressResponse
	err = conn.Call(ctx, "ingress.request", req, &resp)
	return resp, err
}
