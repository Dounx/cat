package pluginhost

import (
	"encoding/json"
	"errors"

	"github.com/dounx/cat/core/event"
)

// decodeEvent parses an event emitted by a plugin. Identity fields are
// cleared: the host, not the plugin, decides where an event came from.
func decodeEvent(raw json.RawMessage) (*event.Event, error) {
	var ev event.Event
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	if ev.Type == "" {
		return nil, errors.New("event type is required")
	}
	ev.ID, ev.Source, ev.Plugin = "", "", ""
	return &ev, nil
}
