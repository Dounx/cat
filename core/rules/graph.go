package rules

import (
	"fmt"
	"strings"

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
	// condition does not hold, or when it comes to none of its choices. An
	// edge with no branch is taken when a yes-or-no condition holds; one
	// leaving a condition with choices names the choice it is taken for.
	BranchElse = "else"

	// JudgeAction is the action a condition node asks of its instance when
	// it names none.
	JudgeAction = "judge"
	// JudgeYes and JudgeNo are the choices a yes-or-no condition puts to
	// the instance that judges it.
	JudgeYes = "yes"
	JudgeNo  = "no"
)

// Node is one trigger, condition or action of a graph. An action node is a
// Step, and later nodes refer to its output as steps.<id>. A trigger node
// names the events that start a walk there, as a rule's Sources and Types do.
// With Params set, a trigger node asks its sources for something of its own
// (a feed to watch, say) and takes only the events they emit for it; see
// Trigger. A throttle node lets a walk past once per ThrottleSeconds for each
// value of ThrottleKey (an expression; empty = one window for the node).
//
// A condition node decides by its Condition expression or, with Instance
// set, by asking that instance (a model, say) through Action and Params, as
// a Step would. Either way the outcome is yes or no, or, with Choices set,
// one of them: the expression's value, or the instance's pick, selects the
// edges whose branch is that choice, and anything else the "else" ones.
type Node struct {
	Step
	Kind      string   `json:"kind"`
	Condition string   `json:"condition,omitempty"`
	Choices   []string `json:"choices,omitempty"`
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
	triggers []*Node           // in the order listed; empty when the rule's own trigger is used
	asks     map[string]string // trigger node → the id of what it asks, if anything
	nodes    map[string]*Node
	conds    map[string]*vm.Program
	next     map[string][]Edge // outgoing edges, in the order they were listed
}

func compileGraph(g *Graph) (*compiledGraph, error) {
	c := &compiledGraph{nodes: map[string]*Node{}, conds: map[string]*vm.Program{}, next: map[string][]Edge{}, asks: map[string]string{}}
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
			for i, ch := range n.Choices {
				if strings.TrimSpace(ch) == "" || ch == BranchElse || contains(n.Choices[:i], ch) {
					return nil, fmt.Errorf("条件 %s 的选项无效：%q", n.ID, ch)
				}
			}
			if n.Judged() {
				break
			}
			// with choices the expression gives one of them, not yes or no
			var p *vm.Program
			var err error
			if len(n.Choices) > 0 && strings.TrimSpace(n.Condition) != "" {
				p, err = expr.Compile(n.Condition, compileOpts...)
			} else {
				p, err = CompileCondition(n.Condition)
			}
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
			if id := n.TriggerID(); id != "" {
				if len(n.Sources) == 0 {
					return nil, fmt.Errorf("触发 %s 填了要订阅的内容，需要选定来源", n.ID)
				}
				c.asks[n.ID] = id
			}
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
		if !validBranch(from, e.Branch) {
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

// Judged reports whether a condition node is decided by asking an instance
// rather than by an expression.
func (n *Node) Judged() bool { return n.Kind == KindCondition && n.Instance != "" }

// TriggerID is the id of what a trigger node asks of its sources, or "" when
// it asks nothing and takes whatever they emit.
func (n *Node) TriggerID() string {
	if n.Kind != KindTrigger {
		return ""
	}
	return TriggerID(n.Params)
}

// validBranch reports whether an edge leaving from may carry the branch:
// none, unless from is a condition; then "else", and either no branch (yes
// or no) or one of the condition's choices.
func validBranch(from *Node, branch string) bool {
	if from == nil || from.Kind != KindCondition {
		return branch == ""
	}
	if branch == BranchElse {
		return true
	}
	if len(from.Choices) == 0 {
		return branch == ""
	}
	return contains(from.Choices, branch)
}

// Judge asks a condition node's instance for its verdict: true or false for
// a yes-or-no condition, else the choice picked.
type Judge func(n *Node, env map[string]any) (any, error)

// decide gives the branch a walk leaves a condition by.
func (c *compiledGraph) decide(n *Node, env map[string]any, judge Judge) (string, error) {
	var out any = true // no expression: always holds
	var err error
	if n.Judged() {
		out, err = judge(n, env)
	} else if p := c.conds[n.ID]; p != nil {
		out, err = expr.Run(p, env)
	} else if len(n.Choices) > 0 {
		return BranchElse, nil
	}
	if err != nil {
		return "", err
	}
	if len(n.Choices) == 0 {
		if holds, _ := out.(bool); holds {
			return "", nil
		}
		return BranchElse, nil
	}
	if s := fmt.Sprint(out); out != nil && contains(n.Choices, s) {
		return s, nil
	}
	return BranchElse, nil
}

// branches lists the branches of the edges leaving a node, each once.
func (c *compiledGraph) branches(id string) []string {
	var list []string
	for _, e := range c.next[id] {
		if !contains(list, e.Branch) {
			list = append(list, e.Branch)
		}
	}
	return list
}

// walk follows the edges leaving `from` on the given branch, depth first and
// in order. Each node is entered at most once. act runs an action and reports
// whether to go on past it; with act nil the walk only looks for an action
// and stops at the first one. fail is told of a condition that cannot be
// evaluated, and that branch ends there. gate decides whether a throttle node
// lets the walk past; with gate nil every one does. judge decides the
// conditions that are put to an instance; with judge nil nothing is asked,
// and the walk goes on by every way out of such a condition.
func (c *compiledGraph) walk(from, branch string, env func() map[string]any, seen map[string]bool,
	act func(Step) bool, fail func(id string, err error), gate Gate, judge Judge) (found bool) {
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
				c.walk(n.ID, "", env, seen, act, fail, gate, judge)
			}
			continue
		}
		next := []string{""}
		switch {
		case n.Kind == KindThrottle:
			if gate != nil && !gate(n, env()) {
				continue
			}
		case n.Kind != KindCondition:
		case n.Judged() && judge == nil:
			next = c.branches(n.ID)
		default:
			b, err := c.decide(n, env(), judge)
			if err != nil {
				if fail != nil {
					fail(n.ID, err)
				}
				continue
			}
			next[0] = b
		}
		for _, b := range next {
			if c.walk(n.ID, b, env, seen, act, fail, gate, judge) {
				found = true
				if act == nil {
					return true
				}
			}
		}
	}
	return found
}

// starts lists where a walk for the event begins: the trigger nodes it falls
// within (and, for those that ask their sources for something, was emitted
// for), or the rule's own trigger when the graph has no trigger nodes (the
// rule's scope has been checked by then).
func (c *compiledGraph) starts(ev *event.Event) []string {
	if len(c.triggers) == 0 {
		return []string{TriggerNode}
	}
	var ids []string
	for _, t := range c.triggers {
		if !inScope(t.Sources, t.Types, ev) {
			continue
		}
		// a trigger that asked for something takes only what was emitted for it
		if ask := c.asks[t.ID]; ask == "" || contains(ev.Triggers, ask) {
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
// actions. Nothing runs, and no instance is asked: a condition put to one
// counts as letting the event out by any of its ways.
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
			}, gate, nil)
		if ok {
			return true, first
		}
	}
	return false, first
}

// Walk runs a graph rule for an event. act is called for each action reached,
// in order; it runs the step, records its output in outputs (where later
// nodes read it as steps.<id>) and reports whether the walk may go on past
// it. fail is called for a condition that cannot be evaluated, gate decides
// whether each throttle node reached lets the walk past, and judge answers
// the conditions that are put to an instance.
func (c *Compiled) Walk(ev *event.Event, outputs map[string]any, act func(Step) bool, fail func(id string, err error), gate Gate, judge Judge) {
	if c.graph == nil {
		return
	}
	seen := map[string]bool{}
	for _, start := range c.graph.starts(ev) {
		c.graph.walk(start, "", func() map[string]any { return Env(ev, outputs) }, seen, act, fail, gate, judge)
	}
}
