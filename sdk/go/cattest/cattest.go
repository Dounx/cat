// Package cattest runs a plugin in-process against a fake Cat host, so plugin
// authors can test without starting Cat.
package cattest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/jsonrpc"
)

// Harness is a fake host connected to a running plugin.
type Harness struct {
	t    testing.TB
	conn *jsonrpc.Conn

	// Events receives every event the plugin emits.
	Events chan cat.Event
	// Steps receives every wizard step the plugin shows or asks.
	Steps chan cat.Step
	// Answers supplies responses to Ask steps, in order. A nil map cancels.
	Answers chan map[string]any

	mu       sync.Mutex
	state    map[string]string
	statuses []string
}

// Options configure the fake host.
type Options struct {
	Config  map[string]any
	DataDir string
	State   map[string]string
}

// New connects a plugin to a fake host and initializes it (without starting).
func New(t testing.TB, p cat.Plugin, opts Options) *Harness {
	t.Helper()
	hostR, pluginW := io.Pipe()
	pluginR, hostW := io.Pipe()
	h := &Harness{
		t: t, conn: jsonrpc.New(hostR, hostW),
		Events: make(chan cat.Event, 256), Steps: make(chan cat.Step, 256), Answers: make(chan map[string]any, 16),
		state: map[string]string{},
	}
	for k, v := range opts.State {
		h.state[k] = v
	}
	n := 0
	h.conn.Handle("event.emit", func(_ context.Context, raw json.RawMessage) (any, error) {
		var ev cat.Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, err
		}
		h.mu.Lock()
		n++
		id := "ev" + string(rune('0'+n%10))
		h.mu.Unlock()
		h.Events <- ev
		return map[string]string{"id": id}, nil
	})
	h.conn.Handle("log", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Level, Message string }
		_ = json.Unmarshal(raw, &in)
		t.Logf("[plugin %s] %s", in.Level, in.Message)
		return nil, nil
	})
	h.conn.Handle("status", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Status, Message string }
		_ = json.Unmarshal(raw, &in)
		h.mu.Lock()
		h.statuses = append(h.statuses, in.Status)
		h.mu.Unlock()
		return nil, nil
	})
	h.conn.Handle("state.get", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Key string }
		_ = json.Unmarshal(raw, &in)
		h.mu.Lock()
		defer h.mu.Unlock()
		v, ok := h.state[in.Key]
		return map[string]any{"value": v, "found": ok}, nil
	})
	h.conn.Handle("state.set", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct{ Key, Value string }
		_ = json.Unmarshal(raw, &in)
		h.mu.Lock()
		defer h.mu.Unlock()
		h.state[in.Key] = in.Value
		return nil, nil
	})
	h.conn.Handle("setup.show", func(_ context.Context, raw json.RawMessage) (any, error) {
		var s cat.Step
		_ = json.Unmarshal(raw, &s)
		h.Steps <- s
		return nil, nil
	})
	h.conn.Handle("setup.ask", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var s cat.Step
		_ = json.Unmarshal(raw, &s)
		h.Steps <- s
		select {
		case ans := <-h.Answers:
			if ans == nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeCancelled, Message: "cancelled"}
			}
			return ans, nil
		case <-time.After(10 * time.Second):
			return nil, errors.New("cattest: no answer queued for step " + s.Title)
		}
	})
	go func() { _ = h.conn.Serve() }()
	go func() { _ = cat.Serve(p, pluginR, pluginW) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = h.conn.Call(ctx, "stop", nil, nil)
		cancel()
		hostW.Close()
		pluginW.Close()
	})
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	if opts.Config == nil {
		opts.Config = map[string]any{}
	}
	err := h.call("initialize", map[string]any{
		"protocol": cat.ProtocolVersion, "instance": map[string]string{"id": "test-instance"},
		"config": opts.Config, "data_dir": opts.DataDir,
	}, nil)
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return h
}

func (h *Harness) call(method string, params, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return h.conn.Call(ctx, method, params, out)
}

// Start calls the plugin's Start.
func (h *Harness) Start() error { return h.call("start", nil, nil) }

// Invoke calls an action as a rule step would.
func (h *Harness) Invoke(action string, params map[string]any, trigger *cat.Event) (map[string]any, error) {
	var out map[string]any
	err := h.call("action.invoke", map[string]any{"action": action, "params": params, "context": cat.ActionContext{Event: trigger}}, &out)
	return out, err
}

// Ingress delivers an HTTP request to the plugin.
func (h *Harness) Ingress(req cat.IngressRequest) (cat.IngressResponse, error) {
	var out cat.IngressResponse
	err := h.call("ingress.request", req, &out)
	return out, err
}

// RunSetup runs the wizard to completion and returns the resulting config.
// Queue the answers on h.Answers first.
func (h *Harness) RunSetup(existing map[string]any) (map[string]any, error) {
	var out struct {
		Config map[string]any `json:"config"`
	}
	err := h.call("setup.run", map[string]any{"existing": existing}, &out)
	return out.Config, err
}

// NextEvent waits for the next emitted event.
func (h *Harness) NextEvent() cat.Event {
	h.t.Helper()
	select {
	case ev := <-h.Events:
		return ev
	case <-time.After(10 * time.Second):
		h.t.Fatal("cattest: no event emitted")
		return cat.Event{}
	}
}

// State returns a persisted value.
func (h *Harness) State(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state[key]
}
