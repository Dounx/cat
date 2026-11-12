// Package event defines the unified event envelope that flows through the hub.
package event

import (
	"encoding/json"
	"time"
)

// Conversation identifies where a message happened (a group, channel or DM).
type Conversation struct {
	ID   string `json:"id"`
	Kind string `json:"kind,omitempty"` // "private" | "group" | "channel"
	Name string `json:"name,omitempty"`
}

// Actor identifies who caused the event.
type Actor struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	IsBot bool   `json:"is_bot,omitempty"`
}

// Event is the envelope every source emits. Common chat fields are normalised
// into Payload (text, attachments, message_id, ...); everything else from the
// platform is kept untouched in Raw.
type Event struct {
	ID           string         `json:"id"`
	Source       string         `json:"source"` // instance id
	Plugin       string         `json:"plugin"`
	Type         string         `json:"type"` // e.g. "message.created"
	Time         time.Time      `json:"time"`
	Conversation *Conversation  `json:"conversation,omitempty"`
	Actor        *Actor         `json:"actor,omitempty"`
	Payload      map[string]any `json:"payload"`
	Raw          any            `json:"raw,omitempty"`
	// Triggers names the triggers the source emitted this event for: the ids
	// it was given with their parameters (see rules.TriggerID). Empty for an
	// event that no rule asked for in particular.
	Triggers []string `json:"triggers,omitempty"`
}

// Map converts the event into plain maps using its JSON field names, which is
// the shape rule expressions and templates see.
func (e *Event) Map() map[string]any {
	b, _ := json.Marshal(e)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	if m["payload"] == nil {
		m["payload"] = map[string]any{}
	}
	return m
}
