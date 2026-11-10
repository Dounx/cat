// Package rules defines rules, evaluates their conditions and renders the
// templated parameters of pipeline steps.
package rules

import (
	"fmt"
	"path"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/dounx/cat/core/event"
)

// SourceInstance as a step target means "the instance that emitted the event".
const SourceInstance = "$source"

// Step is one action in a rule's pipeline.
type Step struct {
	ID              string         `json:"id"`       // referenced by later steps as steps.<id>
	Instance        string         `json:"instance"` // instance id or "$source"
	Action          string         `json:"action"`
	Params          map[string]any `json:"params,omitempty"`
	ContinueOnError bool           `json:"continue_on_error,omitempty"`
	TimeoutSeconds  int            `json:"timeout_seconds,omitempty"`
}

// Rule matches events and runs a pipeline of steps for each match.
type Rule struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Enabled   bool     `json:"enabled"`
	Sources   []string `json:"sources,omitempty"` // instance ids; empty = any
	Types     []string `json:"types,omitempty"`   // globs such as "message.*"; empty = any
	Condition string   `json:"condition,omitempty"`
	Steps     []Step   `json:"steps"`
	// ThrottleSeconds drops matches arriving within this window of the last
	// accepted one sharing the same ThrottleKey (an expression; empty = rule-wide).
	ThrottleSeconds int    `json:"throttle_seconds,omitempty"`
	ThrottleKey     string `json:"throttle_key,omitempty"`
	// Meta is opaque UI state (for example the visual condition builder's rows).
	Meta map[string]any `json:"meta,omitempty"`
}

// Env builds the variables visible to expressions: the event under "event"
// plus its top-level fields as shortcuts, and step outputs under "steps".
func Env(ev *event.Event, steps map[string]any) map[string]any {
	m := ev.Map()
	env := map[string]any{"event": m, "steps": steps}
	if steps == nil {
		env["steps"] = map[string]any{}
	}
	for _, k := range []string{"id", "source", "plugin", "type", "time", "conversation", "actor", "payload", "raw"} {
		env[k] = m[k]
	}
	return env
}

var compileOpts = []expr.Option{expr.AllowUndefinedVariables()}

// CompileCondition compiles a boolean expression. An empty condition is valid.
func CompileCondition(src string) (*vm.Program, error) {
	if strings.TrimSpace(src) == "" {
		return nil, nil
	}
	return expr.Compile(src, append(compileOpts, expr.AsBool())...)
}

// Eval evaluates an arbitrary expression against env.
func Eval(src string, env map[string]any) (any, error) {
	p, err := expr.Compile(src, compileOpts...)
	if err != nil {
		return nil, err
	}
	return expr.Run(p, env)
}

// Compiled is a rule with its condition ready to run.
type Compiled struct {
	Rule
	cond *vm.Program
}

func Compile(r Rule) (*Compiled, error) {
	p, err := CompileCondition(r.Condition)
	if err != nil {
		return nil, fmt.Errorf("condition: %w", err)
	}
	if r.ThrottleKey != "" {
		if _, err := expr.Compile(r.ThrottleKey, compileOpts...); err != nil {
			return nil, fmt.Errorf("throttle_key: %w", err)
		}
	}
	return &Compiled{Rule: r, cond: p}, nil
}

// Scope reports whether the event falls within the rule's sources and types.
func (c *Compiled) Scope(ev *event.Event) bool {
	if len(c.Sources) > 0 && !contains(c.Sources, ev.Source) {
		return false
	}
	if len(c.Types) == 0 {
		return true
	}
	for _, t := range c.Types {
		if ok, _ := path.Match(t, ev.Type); ok || t == ev.Type {
			return true
		}
	}
	return false
}

// Match reports whether the rule applies to the event. A condition that
// fails to evaluate (e.g. a missing field) is returned as an error.
func (c *Compiled) Match(ev *event.Event) (bool, error) {
	if !c.Scope(ev) {
		return false, nil
	}
	if c.cond == nil {
		return true, nil
	}
	out, err := expr.Run(c.cond, Env(ev, nil))
	if err != nil {
		return false, err
	}
	b, _ := out.(bool)
	return b, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Render resolves {{ expression }} placeholders inside v, recursing through
// maps and slices. A string that is exactly one placeholder keeps the
// expression's type (so objects and numbers pass through); otherwise results
// are interpolated as text.
func Render(v any, env map[string]any) (any, error) {
	switch t := v.(type) {
	case string:
		return renderString(t, env)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			r, err := Render(val, env)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			r, err := Render(val, env)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

func renderString(s string, env map[string]any) (any, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") && strings.Count(trimmed, "{{") == 1 {
		return Eval(strings.TrimSpace(trimmed[2:len(trimmed)-2]), env)
	}
	var b strings.Builder
	rest := s
	for {
		i := strings.Index(rest, "{{")
		if i < 0 {
			b.WriteString(rest)
			break
		}
		j := strings.Index(rest[i:], "}}")
		if j < 0 {
			return nil, fmt.Errorf("unclosed {{ in template")
		}
		b.WriteString(rest[:i])
		val, err := Eval(strings.TrimSpace(rest[i+2:i+j]), env)
		if err != nil {
			return nil, err
		}
		if val != nil {
			fmt.Fprint(&b, val)
		}
		rest = rest[i+j+2:]
	}
	return b.String(), nil
}
