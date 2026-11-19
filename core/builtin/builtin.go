// Package builtin holds the capabilities that ship inside the engine. They
// implement the same plugin interfaces external plugins are adapted to.
package builtin

import (
	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/plugin"
)

// Entry pairs a manifest with the factory that builds its instances.
type Entry struct {
	Manifest plugin.Manifest
	Factory  plugin.Factory
}

// All returns every built-in plugin.
func All() []Entry {
	return []Entry{webhookEntry(), websocketEntry(), httpEntry(), shellEntry(), collectEntry(), cooldownEntry(), notifyEntry(), selfEntry()}
}

const (
	// SelfID is the plugin, and the one instance, that stands for the hub
	// itself as a source of events.
	SelfID = event.SelfSource
	// RunFailed is the event the hub emits when a run of a rule ends in an
	// error.
	RunFailed = "run.failed"
)

// selfEntry is the hub as a source: it does nothing of its own, the engine
// emits its events.
func selfEntry() Entry {
	return Entry{
		Manifest: plugin.Manifest{
			ID: SelfID, Name: "Cat", Version: "1.0.0", Builtin: true, Singleton: true, Icon: "cat",
			Category:     plugin.CategoryMonitor,
			Description:  "Cat 自己的事：某条规则运行失败了。",
			Capabilities: []string{plugin.CapSource},
			Events:       []plugin.EventSpec{{Type: RunFailed, Title: "规则运行失败"}},
		},
		Factory: func(map[string]any) (plugin.Instance, error) { return plugin.Base{}, nil },
	}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string, def float64) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return def
}
