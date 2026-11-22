package mcp

import (
	"encoding/json"
	"strings"

	"github.com/dounx/cat/core/engine"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

const (
	// PolicyScope and PolicyKey identify where the MCP permission policy is
	// persisted in Store.KV.
	PolicyScope = "settings"
	PolicyKey   = "mcp_policy"
)

// Policy controls what destructive or side-effecting operations MCP clients
// are allowed to invoke. Read-only inspection tools remain always enabled.
type Policy struct {
	AllowWriteRules    bool `json:"allow_write_rules"`
	AllowInvokeActions bool `json:"allow_invoke_actions"`
	AllowShell         bool `json:"allow_shell"`
}

// DefaultPolicy returns the default MCP policy: rule authoring and normal
// action invocation are enabled for convenience, while shell execution is
// disabled by default for safety.
func DefaultPolicy() Policy {
	return Policy{
		AllowWriteRules:    true,
		AllowInvokeActions: true,
		AllowShell:         false,
	}
}

// LoadPolicy reads the stored MCP policy from st, falling back to
// DefaultPolicy when unset or malformed.
func LoadPolicy(st *store.Store) Policy {
	p := DefaultPolicy()
	if st == nil {
		return p
	}
	raw, ok := st.KVGet(PolicyScope, PolicyKey)
	if !ok || strings.TrimSpace(raw) == "" {
		return p
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return DefaultPolicy()
	}
	return p
}

// SavePolicy persists p to st.
func SavePolicy(st *store.Store, p Policy) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return st.KVSet(PolicyScope, PolicyKey, string(b))
}

func isShellInstance(e *engine.Engine, instID string) bool {
	id := strings.TrimSpace(instID)
	if id == "shell" {
		return true
	}
	if e != nil && id != "" {
		if view, ok := e.Instance(id); ok && view.Plugin == "shell" {
			return true
		}
	}
	return false
}

func ruleUsesShell(e *engine.Engine, r rules.Rule) bool {
	for _, step := range r.Steps {
		if isShellInstance(e, step.Instance) {
			return true
		}
	}
	if r.Graph != nil {
		for _, n := range r.Graph.Nodes {
			if isShellInstance(e, n.Instance) {
				return true
			}
		}
	}
	return false
}
