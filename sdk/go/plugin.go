package cat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/dounx/cat/sdk/go/jsonrpc"
)

// ActionFunc performs one action. The returned value becomes the step output
// that later steps reference as steps.<id>.<field>, so return an object.
type ActionFunc func(ctx context.Context, h *Host, params map[string]any, actx ActionContext) (any, error)

// Plugin is what a plugin implements. Every field is optional.
type Plugin struct {
	// Start connects to the platform and begins emitting events. It should
	// return once the instance is ready (or failed); long-running work belongs
	// in goroutines bound to ctx, which is cancelled when the instance stops.
	Start func(ctx context.Context, h *Host) error
	// Stop releases resources before the process exits.
	Stop func()
	// Actions by name, as declared in plugin.json.
	Actions map[string]ActionFunc
	// Ingress handles HTTP requests for plugins with "ingress": true.
	Ingress func(ctx context.Context, h *Host, req IngressRequest) (IngressResponse, error)
	// Setup runs the onboarding wizard and returns the instance configuration.
	Setup func(ctx context.Context, s *Setup) (map[string]any, error)
	// Options lists what may go in a field marked "x-pick": "option" while
	// someone fills in a form: a parameter of the named action, or a
	// configuration field when action is "". It runs on a started instance.
	Options func(ctx context.Context, h *Host, action, field string) ([]Option, error)
	// Triggers is told what the enabled rules ask this instance to watch, for
	// plugins whose plugin.json has trigger_params: the whole list, each time
	// it changes. The first call comes before Start, so that Start can read
	// the list from Host.Triggers; a plugin that only does that needs no
	// function here. Return what is wrong with single triggers, by id (nil
	// when all are fine): it is shown on the rules that asked for them.
	Triggers func(ctx context.Context, h *Host, list []Trigger) map[string]string
}

// Option is one value offered for a field.
type Option struct {
	Value string `json:"value"`
	Title string `json:"title,omitempty"` // what people read; the value when empty
	Note  string `json:"note,omitempty"`  // a few words more, shown beside it
}

// Host is the plugin's handle on Cat.
type Host struct {
	conn *jsonrpc.Conn
	// Config is the instance configuration, shaped by plugin.json's config_schema.
	Config map[string]any
	// InstanceID identifies this instance; DataDir is its private storage.
	InstanceID string
	DataDir    string

	mu       sync.Mutex
	triggers []Trigger
}

// Triggers returns what the enabled rules ask this instance to watch, as of
// the latest update. It is already filled in when Start runs.
func (h *Host) Triggers() []Trigger {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.triggers
}

// Emit publishes an event and returns the id Cat assigned to it.
func (h *Host) Emit(ev Event) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := h.conn.Call(context.Background(), "event.emit", ev, &out)
	return out.ID, err
}

// Log writes a line to Cat's log for this instance. Level: debug|info|warn|error.
func (h *Host) Log(level, format string, args ...any) {
	_ = h.conn.Notify("log", map[string]string{"level": level, "message": fmt.Sprintf(format, args...)})
}

// SetStatus reports connection health after Start has returned, e.g.
// ("error", "disconnected, retrying") and later ("running", "").
func (h *Host) SetStatus(status, message string) {
	_ = h.conn.Notify("status", map[string]string{"status": status, "message": message})
}

// StateGet reads a value persisted with StateSet (survives restarts).
func (h *Host) StateGet(key string) (string, bool) {
	var out struct {
		Value string `json:"value"`
		Found bool   `json:"found"`
	}
	if err := h.conn.Call(context.Background(), "state.get", map[string]string{"key": key}, &out); err != nil {
		return "", false
	}
	return out.Value, out.Found
}

func (h *Host) StateSet(key, value string) error {
	return h.conn.Call(context.Background(), "state.set", map[string]string{"key": key, "value": value}, nil)
}

// String returns a string config value.
func (h *Host) String(key string) string {
	s, _ := h.Config[key].(string)
	return s
}

// Dependency asks Cat to make a dependency declared in plugin.json available
// (downloading and verifying it if needed) and returns its directory.
func (h *Host) Dependency(ctx context.Context, name string) (string, error) {
	var out struct {
		Path string `json:"path"`
	}
	err := h.conn.Call(ctx, "host.dependency", map[string]string{"name": name}, &out)
	return out.Path, err
}

// ErrCancelled is returned by Setup helpers when the user closes the wizard.
var ErrCancelled = errors.New("cancelled by user")

// Setup drives the onboarding wizard.
type Setup struct {
	*Host
	// Existing is the current configuration when re-running setup for an
	// existing instance; empty for a new one.
	Existing map[string]any
}

// Show displays a step and returns immediately.
func (s *Setup) Show(step Step) {
	_ = s.conn.Call(context.Background(), "setup.show", step, nil)
}

// Ask displays a form or confirm step and waits for the user. For forms the
// result holds the entered values; for confirms, {"confirmed": bool}.
func (s *Setup) Ask(ctx context.Context, step Step) (map[string]any, error) {
	var out map[string]any
	err := s.conn.Call(ctx, "setup.ask", step, &out)
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeCancelled {
		return nil, ErrCancelled
	}
	return out, err
}

// Progress shows a progress bar; pass a negative fraction for "working…".
func (s *Setup) Progress(title string, fraction float64) {
	s.Show(Step{Kind: StepProgress, Title: title, Progress: fraction})
}

// Confirm asks a yes/no question.
func (s *Setup) Confirm(ctx context.Context, title, description string) (bool, error) {
	out, err := s.Ask(ctx, Step{Kind: StepConfirm, Title: title, Description: description})
	ok, _ := out["confirmed"].(bool)
	return ok, err
}

// Run serves the plugin over stdin/stdout until Cat closes the connection.
func Run(p Plugin) {
	if err := Serve(p, os.Stdin, os.Stdout); err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "plugin:", err)
		os.Exit(1)
	}
}

// Serve is Run over arbitrary streams (useful in tests).
func Serve(p Plugin, r io.Reader, w io.Writer) error {
	conn := jsonrpc.New(r, w)
	host := &Host{conn: conn}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if p.Stop != nil {
				p.Stop()
			}
		})
	}

	conn.Handle("initialize", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Instance struct {
				ID string `json:"id"`
			} `json:"instance"`
			Config  map[string]any `json:"config"`
			DataDir string         `json:"data_dir"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		host.Config, host.InstanceID, host.DataDir = in.Config, in.Instance.ID, in.DataDir
		if host.Config == nil {
			host.Config = map[string]any{}
		}
		return map[string]any{"protocol": ProtocolVersion}, nil
	})
	conn.Handle("start", func(context.Context, json.RawMessage) (any, error) {
		if p.Start == nil {
			return nil, nil
		}
		return nil, p.Start(ctx, host)
	})
	conn.Handle("stop", func(context.Context, json.RawMessage) (any, error) {
		stop()
		return nil, nil
	})
	conn.Handle("action.invoke", func(rctx context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Action  string         `json:"action"`
			Params  map[string]any `json:"params"`
			Context ActionContext  `json:"context"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		fn := p.Actions[in.Action]
		if fn == nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "unknown action: " + in.Action}
		}
		if in.Params == nil {
			in.Params = map[string]any{}
		}
		return fn(rctx, host, in.Params, in.Context)
	})
	conn.Handle("triggers.set", func(_ context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Triggers []Trigger `json:"triggers"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		host.mu.Lock()
		host.triggers = in.Triggers
		host.mu.Unlock()
		errs := map[string]string{}
		if p.Triggers != nil {
			// bound to the instance, not to this request: what it sets going keeps going
			for id, msg := range p.Triggers(ctx, host, in.Triggers) {
				errs[id] = msg
			}
		}
		return map[string]any{"errors": errs}, nil
	})
	conn.Handle("options.list", func(rctx context.Context, raw json.RawMessage) (any, error) {
		var in struct {
			Action string `json:"action"`
			Field  string `json:"field"`
		}
		_ = json.Unmarshal(raw, &in)
		opts := []Option{}
		if p.Options != nil {
			found, err := p.Options(rctx, host, in.Action, in.Field)
			if err != nil {
				return nil, err
			}
			opts = append(opts, found...)
		}
		return map[string]any{"options": opts}, nil
	})
	conn.Handle("ingress.request", func(rctx context.Context, raw json.RawMessage) (any, error) {
		if p.Ingress == nil {
			return IngressResponse{Status: 404}, nil
		}
		var req IngressRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		return p.Ingress(rctx, host, req)
	})
	conn.Handle("setup.run", func(rctx context.Context, raw json.RawMessage) (any, error) {
		if p.Setup == nil {
			return nil, errors.New("this plugin has no setup wizard")
		}
		var in struct {
			Existing map[string]any `json:"existing"`
		}
		_ = json.Unmarshal(raw, &in)
		if in.Existing == nil {
			in.Existing = map[string]any{}
		}
		cfg, err := p.Setup(rctx, &Setup{Host: host, Existing: in.Existing})
		if errors.Is(err, ErrCancelled) {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeCancelled, Message: "cancelled"}
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"config": cfg}, nil
	})

	err := conn.Serve()
	stop()
	return err
}
