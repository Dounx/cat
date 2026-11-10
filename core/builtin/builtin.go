// Package builtin holds the capabilities that ship inside the engine. They
// implement the same plugin interfaces external plugins are adapted to.
package builtin

import "github.com/dounx/cat/core/plugin"

// Entry pairs a manifest with the factory that builds its instances.
type Entry struct {
	Manifest plugin.Manifest
	Factory  plugin.Factory
}

// All returns every built-in plugin.
func All() []Entry {
	return []Entry{webhookEntry(), httpEntry(), shellEntry()}
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
