package rules

import (
	"fmt"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/dounx/cat/core/event"
)

// A rule may describe what it does as a graph instead of one condition and a
// list of steps: trigger, condition, throttle and action nodes wired together.
// A walk begins at the triggers the event falls within. Conditions may lead
// to conditions, actions to actions, and any node to several others.
const (
	KindTrigger   = "trigger"
	KindCondition = "condition"
	KindThrottle  = "throttle"
	KindAction    = "action"

	// TriggerNode is the id edges use for the rule's own trigger (its Sources
	// and Types) in a graph that has no trigger nodes.
	TriggerNode = "trigger"
	// BranchElse marks an edge leaving a condition that is taken when the
	// condition does not hold. Other edges are taken when it does.
	BranchElse = "else"
)

// Node is one trigger, condition or action of a graph. An action node is a
// Step, and later nodes refer to its output as steps.<id>. A trigger node
// names the events that start a walk there, as a rule's Sources and Types do.
// A throttle node lets a walk past once per ThrottleSeconds for each value of
// ThrottleKey (an expression; empty = one window for the node).
type Node struct {
	Step
	Kind      string   `json:"kind"`
	Condition string   `json:"condition,omitempty"`
	Sources   []string `json:"sources,omitempty"`
	Types     []string `json:"types,omitempty"`

	ThrottleSeconds int    `json:"throttle_seconds,omitempty"`
	ThrottleKey     string `json:"throttle_key,omitempty"`
	// Meta is opaque UI state (the node's place on the canvas, builder rows).
	Meta map[string]any `json:"meta,omitempty"`
}

// Edge leads from the trigger or a node to a node.
type Edge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Branch string `json:"branch,omitempty"`
}

type Graph struct {
	Nodes []Node `json:"nodes"`
	Edges []Edge `json:"edges"`
}

type compiledGraph struct {
	triggers []*Node // in the order listed; empty when the rule's own trigger is used
	nodes    map[string]*Node
	conds    map[string]*vm.Program
	next     map[string][]Edge // outgoing edges, in the order they were listed
}

func compileGraph(g *Graph) (*compiledGraph, error) {
	c := &compiledGraph{nodes: map[string]*Node{}, conds: map[string]*vm.Program{}, next: map[string][]Edge{}}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.ID == "" || n.ID == TriggerNode {
			return nil, fmt.Errorf("节点标识无效：%q", n.ID)
		}
		if c.nodes[n.ID] != nil {
			return nil, fmt.Errorf("节点标识重复：%s", n.ID)
		}
		switch n.Kind {
		case KindCondition:
			p, err := CompileCondition(n.Condition)
			if err != nil {
				return nil, fmt.Errorf("条件 %s: %w", n.ID, err)
			}
			c.conds[n.ID] = p
		case KindAction:
		case KindThrottle:
			if n.ThrottleKey != "" {
				if _, err := expr.Compile(n.ThrottleKey, compileOpts...); err != nil {
					return nil, fmt.Errorf("节流 %s: %w", n.ID, err)
				}
			}
		case KindTrigger:
			c.triggers = append(c.triggers, n)
		default:
			return nil, fmt.Errorf("节点 %s 的类型无效：%q", n.ID, n.Kind)
		}
		c.nodes[n.ID] = n
	}
	for _, e := range g.Edges {
		from := c.nodes[e.From]
		if from == nil && e.From != TriggerNode {
			return nil, fmt.Errorf("连线的起点不存在：%s", e.From)
		}
		if c.nodes[e.To] == nil {
			return nil, fmt.Errorf("连线的终点不存在：%s", e.To)
		}
		if c.nodes[e.To].Kind == KindTrigger {
			return nil, fmt.Errorf("连线不能指向触发节点：%s", e.To)
		}
		if e.Branch != "" && (e.Branch != BranchElse || from == nil || from.Kind != KindCondition) {
			return nil, fmt.Errorf("连线 %s → %s 的分支无效：%q", e.From, e.To, e.Branch)
		}
		c.next[e.From] = append(c.next[e.From], e)
	}
	// A walk visits each node once, so a loop could never run; say so up front.
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("连线形成了循环（经过 %s）", id)
		case done:
			return nil
		}
		state[id] = visiting
		for _, e := range c.next[id] {
			if err := visit(e.To); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}
	for id := range c.nodes {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// walk follows the edges leaving `from` on the given branch, depth first and
// in order. Each node is entered at most once. act runs an action and reports
// whether to go on past it; with act nil the walk only looks for an action
// and stops at the first one. fail is told of a condition that cannot be
// evaluated, and that branch ends there. gate decides whether a throttle node
// lets the walk past; with gate nil every one does.
func (c *compiledGraph) walk(from, branch string, env func() map[string]any, seen map[string]bool,
	act func(Step) bool, fail func(id string, err error), gate Gate) (found bool) {
	for _, e := range c.next[from] {
		if e.Branch != branch || seen[e.To] {
			continue
		}
		seen[e.To] = true
		n := c.nodes[e.To]
		if n.Kind == KindAction {
			if act == nil {
				return true
			}
			found = true
			if act(n.Step) {
				c.walk(n.ID, "", env, seen, act, fail, gate)
			}
			continue
		}
		holds := true
		if n.Kind == KindThrottle {
			if gate != nil && !gate(n, env()) {
				continue
			}
		} else if p := c.conds[n.ID]; p != nil {
			out, err := expr.Run(p, env())
			if err != nil {
				if fail != nil {
					fail(n.ID, err)
				}
				continue
			}
			holds, _ = out.(bool)
		}
		next := ""
		if !holds {
			next = BranchElse
		}
		if c.walk(n.ID, next, env, seen, act, fail, gate) {
			found = true
			if act == nil {
				return true
			}
		}
	}
	return found
}

// starts lists where a walk for the event begins: the trigger nodes it falls
// within, or the rule's own trigger when the graph has no trigger nodes (the
// rule's scope has been checked by then).
func (c *compiledGraph) starts(ev *event.Event) []string {
	if len(c.triggers) == 0 {
		return []string{TriggerNode}
	}
	var ids []string
	for _, t := range c.triggers {
		if inScope(t.Sources, t.Types, ev) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// Gate reports whether a throttle node lets a walk past; env is what the
// node's key expression is evaluated against.
type Gate func(n *Node, env map[string]any) bool

// reaches reports whether the event gets as far as an action, judging by the
// conditions and throttles that stand between the triggers and the first
// actions. Nothing runs.
func (c *compiledGraph) reaches(ev *event.Event, gate Gate) (bool, error) {
	env := Env(ev, nil)
	var first error
	seen := map[string]bool{}
	for _, start := range c.starts(ev) {
		ok := c.walk(start, "", func() map[string]any { return env }, seen, nil,
			func(id string, err error) {
				if first == nil {
					first = fmt.Errorf("条件 %s: %w", id, err)
				}
			}, gate)
		if ok {
			return true, first
		}
	}
	return false, first
}

// Walk runs a graph rule for an event. act is called for each action reached,
// in order; it runs the step, records its output in outputs (where later
// nodes read it as steps.<id>) and reports whether the walk may go on past
// it. fail is called for a condition that cannot be evaluated, and gate
// decides whether each throttle node reached lets the walk past.
func (c *Compiled) Walk(ev *event.Event, outputs map[string]any, act func(Step) bool, fail func(id string, err error), gate Gate) {
	if c.graph == nil {
		return
	}
	seen := map[string]bool{}
	for _, start := range c.graph.starts(ev) {
		c.graph.walk(start, "", func() map[string]any { return Env(ev, outputs) }, seen, act, fail, gate)
	}
}
