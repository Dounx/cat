// Package plugin defines the contract between the engine and anything that
// produces events or performs actions. Built-in capabilities implement these
// interfaces in-process; external plugins are adapted to them by pluginhost.
package plugin

import (
	"context"

	"github.com/dounx/cat/core/event"
)

const (
	CapSource = "source"
	CapAction = "action"
)

// A plugin's category is the role it plays for the user. It only affects how
// the plugin is presented; any plugin can be used in any rule step.
const (
	// CategoryChannel plugins connect to the outside world: where events
	// come from and where actions are sent.
	CategoryChannel = "channel"
	// CategoryProcessor plugins take an event and work out a result, e.g. an
	// AI agent, an auto-responder or a hand-off to a person.
	CategoryProcessor = "processor"
)

// NormalizeCategory maps the empty string to CategoryChannel and reports
// whether the category is a known one.
func NormalizeCategory(c string) (string, bool) {
	switch c {
	case "", CategoryChannel:
		return CategoryChannel, true
	case CategoryProcessor:
		return CategoryProcessor, true
	}
	return c, false
}

// ActionSpec describes one action a plugin can perform.
type ActionSpec struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Params      map[string]any `json:"params,omitempty"` // JSON Schema
}

// EventSpec describes one event type a plugin emits.
type EventSpec struct {
	Type  string `json:"type"`
	Title string `json:"title"`
}

// Manifest is the static description of a plugin (plugin.json for external ones).
type Manifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Icon        string `json:"icon,omitempty"`
	// Category is CategoryChannel (the default) or CategoryProcessor.
	Category     string         `json:"category"`
	Capabilities []string       `json:"capabilities"`
	ConfigSchema map[string]any `json:"config_schema,omitempty"` // JSON Schema
	Actions      []ActionSpec   `json:"actions,omitempty"`
	Events       []EventSpec    `json:"events,omitempty"`
	// Singleton plugins need no configuration; one instance whose id equals
	// the plugin id is created automatically.
	Singleton bool `json:"singleton,omitempty"`
	// Ingress plugins receive HTTP requests at /ingress/<instance id>.
	Ingress bool `json:"ingress,omitempty"`
	// Setup plugins offer an interactive onboarding wizard.
	Setup   bool `json:"setup,omitempty"`
	Builtin bool `json:"builtin"`
}

// IngressRequest is an HTTP request forwarded to an instance.
type IngressRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"` // path below /ingress/<id>
	Query   map[string][]string `json:"query,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// IngressResponse is what the instance wants returned to the HTTP caller.
type IngressResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// InvokeContext carries what triggered an action.
type InvokeContext struct {
	Event      *event.Event `json:"event,omitempty"`
	RuleID     string       `json:"rule_id,omitempty"`
	RunID      string       `json:"run_id,omitempty"`
	InstanceID string       `json:"instance_id,omitempty"`
}

// Host is what the engine offers to a running instance.
type Host interface {
	// Emit publishes an event; Source/Plugin/ID/Time are filled in by the host.
	Emit(ev *event.Event) (id string, err error)
	Log(level, msg string)
	StateGet(key string) (string, bool)
	StateSet(key, value string) error
	// DataDir is a private, persistent directory for this instance.
	DataDir() string
	InstanceID() string
	// SetStatus reports a status change after Start has returned.
	SetStatus(status, message string)
}

// Instance is one configured, running copy of a plugin.
type Instance interface {
	Start(ctx context.Context, host Host) error
	Stop() error
	Invoke(ctx context.Context, action string, params map[string]any, ictx InvokeContext) (map[string]any, error)
	HandleIngress(ctx context.Context, req IngressRequest) (IngressResponse, error)
}

// Factory builds an instance from its stored configuration.
type Factory func(config map[string]any) (Instance, error)

// Base provides no-op implementations so instances only override what they use.
type Base struct{}

func (Base) Start(context.Context, Host) error { return nil }
func (Base) Stop() error                       { return nil }
func (Base) Invoke(_ context.Context, action string, _ map[string]any, _ InvokeContext) (map[string]any, error) {
	return nil, ErrUnknownAction(action)
}
func (Base) HandleIngress(context.Context, IngressRequest) (IngressResponse, error) {
	return IngressResponse{Status: 404}, nil
}

// ErrUnknownAction is returned when an instance is asked for an action it lacks.
type ErrUnknownAction string

func (e ErrUnknownAction) Error() string { return "unknown action: " + string(e) }
