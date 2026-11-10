// Package cat is the Go SDK for writing Cat plugins.
//
// A plugin is an executable that Cat starts once per configured instance and
// talks to over stdin/stdout. Describe it in plugin.json, fill in a Plugin and
// call Run from main. See docs/plugin-protocol.md for the wire protocol.
package cat

import "time"

// ProtocolVersion is the plugin protocol this SDK speaks.
const ProtocolVersion = 1

// Conversation identifies where a message happened.
type Conversation struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"` // "private" | "group" | "channel"
	Name string `json:"name,omitempty"`
}

// Actor identifies who caused an event.
type Actor struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	IsBot bool   `json:"is_bot,omitempty"`
}

// Event is what a plugin emits. For chat messages use Type "message.created"
// and put the text in Payload["text"] and the platform message id in
// Payload["message_id"], so rules work the same across platforms.
type Event struct {
	ID           string         `json:"id,omitempty"`
	Source       string         `json:"source,omitempty"`
	Plugin       string         `json:"plugin,omitempty"`
	Type         string         `json:"type"`
	Time         *time.Time     `json:"time,omitempty"`
	Conversation *Conversation  `json:"conversation,omitempty"`
	Actor        *Actor         `json:"actor,omitempty"`
	Payload      map[string]any `json:"payload,omitempty"`
	Raw          any            `json:"raw,omitempty"`
}

// ActionContext says what triggered an action.
type ActionContext struct {
	Event  *Event `json:"event,omitempty"` // the event that matched the rule
	RuleID string `json:"rule_id,omitempty"`
	RunID  string `json:"run_id,omitempty"`
}

// IngressRequest is an HTTP request received at /ingress/<instance id>.
type IngressRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   map[string][]string `json:"query,omitempty"`
	Headers map[string]string   `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"` // base64 on the wire
}

// IngressResponse is returned to the HTTP caller.
type IngressResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// Step kinds for the onboarding wizard.
const (
	StepInfo     = "info"     // text only
	StepProgress = "progress" // Progress in 0..1, or negative for indeterminate
	StepQRCode   = "qrcode"   // Text is encoded as a QR code, or Image is shown
	StepLink     = "link"     // URL the user should open
	StepForm     = "form"     // Schema rendered as a form (use with Ask)
	StepConfirm  = "confirm"  // yes/no (use with Ask)
)

// Step is one screen of the onboarding wizard, rendered by Cat's own UI.
type Step struct {
	Kind        string         `json:"kind"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Text        string         `json:"text,omitempty"`
	Image       string         `json:"image,omitempty"` // data: URL
	URL         string         `json:"url,omitempty"`
	Progress    float64        `json:"progress,omitempty"`
	Schema      map[string]any `json:"schema,omitempty"`
	Values      map[string]any `json:"values,omitempty"` // initial form values
	Danger      bool           `json:"danger,omitempty"` // confirm is risky
}
