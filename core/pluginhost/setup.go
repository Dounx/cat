package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/store"
	"github.com/dounx/cat/sdk/go/jsonrpc"
)

// Setup session states.
const (
	SetupRunning   = "running"   // the plugin is working; Step (if any) is informational
	SetupWaiting   = "waiting"   // Step needs the user's answer
	SetupDone      = "done"      // the instance has been saved
	SetupError     = "error"     // the wizard failed
	SetupCancelled = "cancelled" // the user closed the wizard
)

// SetupView is the state of an onboarding wizard as shown to the UI.
type SetupView struct {
	ID         string         `json:"id"`
	Plugin     string         `json:"plugin"`
	InstanceID string         `json:"instance_id"`
	State      string         `json:"state"`
	Step       map[string]any `json:"step,omitempty"`
	Error      string         `json:"error,omitempty"`
}

type setupSession struct {
	mu     sync.Mutex
	view   SetupView
	isNew  bool
	answer chan map[string]any // nil answer = cancelled
	cancel context.CancelFunc
}

func (s *setupSession) snapshot() SetupView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view
}

func (h *Host) update(s *setupSession, fn func(v *SetupView)) {
	s.mu.Lock()
	fn(&s.view)
	v := s.view
	s.mu.Unlock()
	h.eng.Publish("setup", v)
}

// ask shows a step that needs an answer and blocks until the user responds.
func (h *Host) ask(ctx context.Context, s *setupSession, step map[string]any) (map[string]any, error) {
	h.update(s, func(v *SetupView) { v.State, v.Step = SetupWaiting, step })
	select {
	case ans := <-s.answer:
		if ans == nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeCancelled, Message: "cancelled"}
		}
		h.update(s, func(v *SetupView) { v.State = SetupRunning })
		return ans, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// StartSetup launches a plugin's onboarding wizard. With instanceID set it
// reconfigures that instance; otherwise it creates a new one named name.
func (h *Host) StartSetup(pluginID, instanceID, name string) (SetupView, error) {
	m, ok := h.manifest(pluginID)
	if !ok {
		return SetupView{}, fmt.Errorf("unknown plugin %q", pluginID)
	}
	if !m.Setup {
		return SetupView{}, fmt.Errorf("插件 %s 没有接入向导", m.Name)
	}
	rec := store.Instance{Plugin: pluginID, Name: name, Enabled: true, Config: map[string]any{}}
	isNew := instanceID == ""
	if isNew {
		rec.ID = pluginID + "-" + id.Token(4)
		if rec.Name == "" {
			rec.Name = m.Name
		}
	} else {
		cur, ok := h.eng.Instance(instanceID)
		if !ok || cur.Plugin != pluginID {
			return SetupView{}, fmt.Errorf("instance %q not found", instanceID)
		}
		rec = cur.Instance
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &setupSession{
		view:   SetupView{ID: id.New(), Plugin: pluginID, InstanceID: rec.ID, State: SetupRunning},
		isNew:  isNew,
		answer: make(chan map[string]any),
		cancel: cancel,
	}
	h.mu.Lock()
	h.setups[s.view.ID] = s
	h.mu.Unlock()
	go h.runSetup(ctx, s, m, rec)
	return s.snapshot(), nil
}

func (h *Host) runSetup(ctx context.Context, s *setupSession, m *Manifest, rec store.Instance) {
	defer s.cancel()
	dataDir := h.eng.InstanceDir(rec.ID)
	original := rec
	finish := func(state, msg string) {
		if state != SetupDone {
			if s.isNew {
				_ = os.RemoveAll(dataDir)
			} else if _, err := h.eng.SaveInstance(original); err != nil { // put it back as it was
				msg += "；恢复原实例失败：" + err.Error()
			}
		}
		h.update(s, func(v *SetupView) { v.State, v.Error = state, msg })
		// Keep the final state readable for a while, then forget the session.
		time.AfterFunc(10*time.Minute, func() {
			h.mu.Lock()
			delete(h.setups, s.view.ID)
			h.mu.Unlock()
		})
	}
	fail := func(err error) {
		var rpcErr *jsonrpc.Error
		if ctx.Err() != nil || (errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeCancelled) {
			finish(SetupCancelled, "")
			return
		}
		finish(SetupError, err.Error())
	}

	// An existing instance must release its resources (ports, sessions)
	// before the wizard's process takes over.
	if !s.isNew {
		stopped := rec
		stopped.Enabled = false
		if _, err := h.eng.SaveInstance(stopped); err != nil {
			fail(err)
			return
		}
	}

	scope := "instance:" + rec.ID
	register := func(conn *jsonrpc.Conn) {
		conn.Handle("setup.show", func(_ context.Context, raw json.RawMessage) (any, error) {
			var step map[string]any
			if err := json.Unmarshal(raw, &step); err != nil {
				return nil, err
			}
			h.update(s, func(v *SetupView) { v.State, v.Step = SetupRunning, step })
			return nil, nil
		})
		conn.Handle("setup.ask", func(rctx context.Context, raw json.RawMessage) (any, error) {
			var step map[string]any
			if err := json.Unmarshal(raw, &step); err != nil {
				return nil, err
			}
			return h.ask(rctx, s, step)
		})
		conn.Handle("log", func(_ context.Context, raw json.RawMessage) (any, error) { return nil, nil })
		conn.Handle("status", func(_ context.Context, raw json.RawMessage) (any, error) { return nil, nil })
		conn.Handle("state.get", func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Key string }
			_ = json.Unmarshal(raw, &in)
			v, ok := h.eng.Store.KVGet(scope, in.Key)
			return map[string]any{"value": v, "found": ok}, nil
		})
		conn.Handle("state.set", func(_ context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Key, Value string }
			_ = json.Unmarshal(raw, &in)
			return nil, h.eng.Store.KVSet(scope, in.Key, in.Value)
		})
		conn.Handle("host.dependency", func(rctx context.Context, raw json.RawMessage) (any, error) {
			var in struct{ Name string }
			_ = json.Unmarshal(raw, &in)
			path, err := h.ensureDep(rctx, m, in.Name, s)
			return map[string]string{"path": path}, err
		})
	}
	p, err := h.spawn(m, register, func(string) {})
	if err != nil {
		fail(err)
		return
	}
	defer p.shutdown()

	ictx, icancel := context.WithTimeout(ctx, 15*time.Second)
	err = p.conn.Call(ictx, "initialize", map[string]any{
		"protocol": ProtocolVersion, "mode": "setup",
		"instance": map[string]string{"id": rec.ID},
		"config":   rec.Config, "data_dir": dataDir,
	}, nil)
	icancel()
	if err != nil {
		fail(fmt.Errorf("插件初始化失败：%w", err))
		return
	}
	var out struct {
		Config map[string]any `json:"config"`
	}
	if err := p.conn.Call(ctx, "setup.run", map[string]any{"existing": rec.Config}, &out); err != nil {
		fail(err)
		return
	}
	p.shutdown() // release whatever the wizard held before the instance starts
	rec.Config, rec.Enabled = out.Config, true
	if _, err := h.eng.SaveInstance(rec); err != nil {
		fail(err)
		return
	}
	finish(SetupDone, "")
}

// Setup returns a wizard's current state.
func (h *Host) Setup(sessionID string) (SetupView, bool) {
	h.mu.Lock()
	s := h.setups[sessionID]
	h.mu.Unlock()
	if s == nil {
		return SetupView{}, false
	}
	return s.snapshot(), true
}

// AnswerSetup delivers the user's answer to the step being waited on.
func (h *Host) AnswerSetup(sessionID string, values map[string]any) error {
	h.mu.Lock()
	s := h.setups[sessionID]
	h.mu.Unlock()
	if s == nil {
		return errors.New("setup session not found")
	}
	if values == nil {
		values = map[string]any{}
	}
	select {
	case s.answer <- values:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("向导当前没有在等待输入")
	}
}

// CancelSetup aborts a wizard.
func (h *Host) CancelSetup(sessionID string) {
	h.mu.Lock()
	s := h.setups[sessionID]
	h.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

// ensureDep makes a declared dependency available. Downloading needs the
// user's consent, given once per plugin+dependency+version: inside a wizard
// the host asks; outside one, a missing consent is an error telling the user
// to run the wizard.
func (h *Host) ensureDep(ctx context.Context, m *Manifest, name string, s *setupSession) (string, error) {
	var dep *Dependency
	for i := range m.Dependencies {
		if m.Dependencies[i].Name == name {
			dep = &m.Dependencies[i]
		}
	}
	if dep == nil {
		return "", fmt.Errorf("plugin.json 未声明依赖 %q", name)
	}
	if h.Deps.Installed(m.ID, *dep) {
		return h.Deps.Path(m.ID, *dep), nil
	}
	art, err := dep.Pick()
	if err != nil {
		return "", err
	}
	title := dep.Title
	if title == "" {
		title = dep.Name
	}
	consentKey := m.ID + "/" + dep.Name + "@" + dep.Version + "#" + art.SHA256
	if _, ok := h.eng.Store.KVGet("deps-consent", consentKey); !ok {
		if s == nil {
			return "", fmt.Errorf("需要下载 %s %s，请在「接入」页重新运行它的接入向导以确认", title, dep.Version)
		}
		desc := fmt.Sprintf("%s 需要 %s（版本 %s）。\n\n将从以下地址下载并校验 SHA-256：\n%s", m.Name, title, dep.Version, art.URL)
		if dep.Notice != "" {
			desc += "\n\n" + dep.Notice
		}
		ans, err := h.ask(ctx, s, map[string]any{"kind": "confirm", "title": "下载 " + title + "？", "description": desc})
		if err != nil {
			return "", err
		}
		if ok, _ := ans["confirmed"].(bool); !ok {
			return "", &jsonrpc.Error{Code: jsonrpc.CodeCancelled, Message: "cancelled"}
		}
		if err := h.eng.Store.KVSet("deps-consent", consentKey, time.Now().Format(time.RFC3339)); err != nil {
			return "", err
		}
	}
	var last time.Time
	progress := func(done, total int64) {
		if s == nil || time.Since(last) < 200*time.Millisecond {
			return
		}
		last = time.Now()
		frac := -1.0
		if total > 0 {
			frac = float64(done) / float64(total)
		}
		h.update(s, func(v *SetupView) {
			v.State = SetupRunning
			v.Step = map[string]any{"kind": "progress", "title": "正在下载 " + title, "progress": frac,
				"description": fmt.Sprintf("%.1f MB", float64(done)/1e6)}
		})
	}
	return h.Deps.Install(ctx, m.ID, *dep, progress)
}
