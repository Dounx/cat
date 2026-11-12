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
	// CategoryChannel plugins are two-way connections to the outside world,
	// typically a chat platform: messages come in as events and replies and
	// notifications go out as actions.
	CategoryChannel = "channel"
	// CategoryMonitor plugins watch something and emit an event when it
	// changes, e.g. a feed or a web page. They have nowhere to send a
	// message: telling someone is left to a rule and a channel.
	CategoryMonitor = "monitor"
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
	case CategoryMonitor, CategoryProcessor:
		return c, true
	}
	return c, false
}

// Categories lists the known categories in display order.
func Categories() []string {
	return []string{CategoryChannel, CategoryMonitor, CategoryProcessor}
}

// ActionSpec describes one action a plugin can perform.
type ActionSpec struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Params      map[string]any `json:"params,omitempty"` // JSON Schema
}

// Trigger is something a rule asks an instance to watch: the parameters
// filled in on a trigger node, shaped by the manifest's TriggerParams, under
// an id the instance puts on the events it emits for it.
type Trigger struct {
	ID     string         `json:"id"`
	Params map[string]any `json:"params"`
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
	// Category is one of Categories(); empty means CategoryChannel.
	Category     string         `json:"category"`
	Capabilities []string       `json:"capabilities"`
	ConfigSchema map[string]any `json:"config_schema,omitempty"` // JSON Schema
	Actions      []ActionSpec   `json:"actions,omitempty"`
	Events       []EventSpec    `json:"events,omitempty"`
	// TriggerParams (a JSON Schema) is what a rule's trigger node can ask of
	// an instance: which feeds to watch, say. Plugins that set it are told
	// what the enabled rules ask through Instance.SetTriggers.
	TriggerParams map[string]any `json:"trigger_params,omitempty"`
	// Singleton plugins need no configuration; one instance whose id equals
	// the plugin id is created automatically.
	Singleton bool `json:"singleton,omitempty"`
	// DefaultInstance plugins work without being set up: the first time one
	// is seen, an instance is created for it, so that rules can use it right
	// away. Unlike a singleton's it is an instance like any other: it can be
	// configured or deleted, and there may be more beside it.
	DefaultInstance bool `json:"default_instance,omitempty"`
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

// Option is one value an instance offers for a field marked "x-pick": "option".
type Option struct {
	Value string `json:"value"`
	Title string `json:"title,omitempty"` // what people read; the value when empty
	Note  string `json:"note,omitempty"`  // a few words more, shown beside it
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
	// TriggerErrors reports what is wrong with the triggers last given to the
	// instance, by trigger id; empty when all of them are fine.
	TriggerErrors(errs map[string]string)
}

// Instance is one configured, running copy of a plugin.
type Instance interface {
	Start(ctx context.Context, host Host) error
	Stop() error
	Invoke(ctx context.Context, action string, params map[string]any, ictx InvokeContext) (map[string]any, error)
	HandleIngress(ctx context.Context, req IngressRequest) (IngressResponse, error)
	// Options lists what may go in a field while someone fills in a form: a
	// parameter of the named action, or a configuration field when action is "".
	Options(ctx context.Context, action, field string) ([]Option, error)
	// SetTriggers gives the instance everything the enabled rules ask it to
	// watch, replacing what it was given before. It is called before Start
	// and again whenever the list changes. Problems with single triggers
	// are reported through Host.TriggerErrors.
	SetTriggers(ctx context.Context, list []Trigger) error
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

func (Base) Options(context.Context, string, string) ([]Option, error) { return nil, nil }
func (Base) SetTriggers(context.Context, []Trigger) error              { return nil }

// ErrUnknownAction is returned when an instance is asked for an action it lacks.
type ErrUnknownAction string

func (e ErrUnknownAction) Error() string { return "unknown action: " + string(e) }
