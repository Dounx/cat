package rules

import (
	"fmt"
	"strings"

	"github.com/expr-lang/expr"
	"github.com/expr-lang/expr/vm"

	"github.com/dounx/cat/core/event"
)

// A rule may describe what it does as a graph instead of one condition and a
// list of steps: trigger, condition and action nodes wired together.
// A walk begins at the triggers the event falls within. Conditions may lead
// to conditions, actions to actions, and any node to several others. A wire
// may lead back to a node the walk has come through, which makes a loop: the
// walk starts over from that node, as long as a condition on the way round
// can end it.
const (
	KindTrigger   = "trigger"
	KindCondition = "condition"
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

	// MaxLoops is how many times one walk may go back round its loops before
	// it is stopped.
	MaxLoops = 100
)

// ErrLoops is what a walk fails with when it has gone round MaxLoops times.
var ErrLoops = fmt.Errorf("循环超过 %d 次，已停下", MaxLoops)

// Node is one trigger, condition or action of a graph. An action node is a
// Step, and later nodes refer to its output as steps.<id>. A trigger node
// names the events that start a walk there, as a rule's Sources and Types do.
// With Params set, a trigger node asks its sources for something of its own
// (a feed to watch, say) and takes only the events they emit for it; see
// Trigger.
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
	// A loop ends when a condition in it sends the walk another way, so one
	// made of actions alone would never end; say so up front.
	const visiting, done = 1, 2
	state := map[string]int{}
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("循环里需要一个条件来决定何时停下（经过 %s）", id)
		case done:
			return nil
		}
		state[id] = visiting
		for _, e := range c.next[id] {
			if c.nodes[e.To].Kind == KindCondition {
				continue
			}
			if err := visit(e.To); err != nil {
				return err
			}
		}
		state[id] = done
		return nil
	}
	for i := range g.Nodes {
		if n := &g.Nodes[i]; n.Kind != KindCondition {
			if err := visit(n.ID); err != nil {
				return nil, err
			}
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

// Visit is one node a walk came to: by which wire, and for a condition what
// it came to and which way it left. The visits of a walk, in order, are the
// path the event took through the graph.
type Visit struct {
	Node string `json:"node"`
	// From is the node whose wire led here, and Via the branch of that wire;
	// a walk starts at a trigger, which nothing leads to.
	From string `json:"from,omitempty"`
	Via  string `json:"via,omitempty"`
	// Branch is the way a condition was left by ("" when it held, "else" or
	// the choice picked) and Value what it came to: true or false, or what
	// its expression gave or its instance picked.
	Branch string `json:"branch,omitempty"`
	Value  any    `json:"value,omitempty"`
	// Error is why a condition could not be decided; the walk ends there.
	Error string `json:"error,omitempty"`
}

// Judge asks a condition node's instance for its verdict: true or false for
// a yes-or-no condition, else the choice picked.
type Judge func(n *Node, env map[string]any) (any, error)

// decide gives the branch a walk leaves a condition by, and what the
// condition came to.
func (c *compiledGraph) decide(n *Node, env map[string]any, judge Judge) (string, any, error) {
	var out any = true // no expression: always holds
	var err error
	if n.Judged() {
		out, err = judge(n, env)
	} else if p := c.conds[n.ID]; p != nil {
		out, err = expr.Run(p, env)
	} else if len(n.Choices) > 0 {
		return BranchElse, nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if len(n.Choices) == 0 {
		holds, _ := out.(bool)
		if holds {
			return "", true, nil
		}
		return BranchElse, false, nil
	}
	if s := fmt.Sprint(out); out != nil && contains(n.Choices, s) {
		return s, out, nil
	}
	return BranchElse, out, nil
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

// A walker carries one walk of a graph for an event. act runs an action,
// given the variables its parameters may refer to, and reports whether to go
// on past it; with act nil the walk only looks for an
// action and stops at the first one. fail is told of a condition that cannot
// be evaluated, and that branch ends there. judge decides the conditions that
// are put to an instance; with judge nil nothing is asked, and the walk goes
// on by every way out of such a condition. note is told of each node the walk
// comes to.
type walker struct {
	c     *compiledGraph
	env   func() map[string]any
	act   func(Step, map[string]any) bool
	fail  func(id string, err error)
	judge Judge
	note  func(Visit)

	seen  map[string]bool // nodes entered since the walk last started over from them
	path  []string        // the nodes the walk is on its way out of, outermost first
	jumps []jump          // wires back that were taken, waiting for the pass to end
	loops int             // how many times the walk has gone back so far
}

// A jump is a wire back to a node the walk came through, and the way it had
// come to that node.
type jump struct {
	wire Edge
	path []string
}

// follow takes the edges leaving `from` on the given branch, depth first and
// in order, and reports whether an action was reached. A node is entered at
// most once in a pass: where two ways meet, the second stops. A wire back to
// a node the walk is still on its way out of is a loop, taken once the rest
// of the pass is done.
func (w *walker) follow(from, branch string) (found bool) {
	for _, e := range w.c.next[from] {
		if e.Branch != branch {
			continue
		}
		if w.seen[e.To] {
			if w.act != nil {
				w.back(e)
			}
			continue
		}
		w.seen[e.To] = true
		if w.enter(e) {
			found = true
			if w.act == nil {
				return true
			}
		}
	}
	return found
}

// back notes a wire that leads back to a node, if the walk came through it.
func (w *walker) back(e Edge) {
	for i, at := range w.path {
		if at != e.To {
			continue
		}
		for _, j := range w.jumps {
			if j.wire.To == e.To {
				return
			}
		}
		w.jumps = append(w.jumps, jump{wire: e, path: append([]string(nil), w.path[:i]...)})
		return
	}
}

// visit tells of a node the walk has come to.
func (w *walker) visit(v Visit) {
	if w.note != nil {
		w.note(v)
	}
}

// enter runs an action, or decides a condition, at the end of the wire that
// led there, and goes on from it.
func (w *walker) enter(by Edge) (found bool) {
	id := by.To
	w.path = append(w.path, id)
	defer func() { w.path = w.path[:len(w.path)-1] }()
	n := w.c.nodes[id]
	at := Visit{Node: id, From: by.From, Via: by.Branch}
	if n.Kind == KindAction {
		if w.act == nil {
			return true
		}
		w.visit(at)
		if w.act(n.Step, w.vars()) {
			w.follow(id, "")
		}
		return true
	}
	next := []string{""}
	switch {
	case n.Kind != KindCondition:
	case n.Judged() && w.judge == nil:
		next = w.c.branches(id)
	default:
		b, value, err := w.c.decide(n, w.vars(), w.judge)
		if err != nil {
			at.Error = err.Error()
			w.visit(at)
			if w.fail != nil {
				w.fail(id, err)
			}
			return false
		}
		next[0] = b
		at.Branch, at.Value = b, value
		w.visit(at)
	}
	for _, b := range next {
		if w.follow(id, b) {
			found = true
			if w.act == nil {
				return true
			}
		}
	}
	return found
}

// vars gives the variables as they stand: the event, what the nodes run so
// far have left, and how many times the walk has gone back.
func (w *walker) vars() map[string]any {
	env := w.env()
	env["loop"] = w.loops
	return env
}

// run walks from each of the starts, then goes back round the loops that
// were taken: each starts the walk over from the node its wire leads to, so
// that node and whatever follows it may run again.
func (w *walker) run(starts []string) (found bool) {
	for _, start := range starts {
		w.visit(Visit{Node: start})
		if w.follow(start, "") {
			found = true
			if w.act == nil {
				return true
			}
		}
	}
	for len(w.jumps) > 0 {
		j := w.jumps[0]
		w.jumps = w.jumps[1:]
		if w.loops >= MaxLoops {
			if w.fail != nil {
				w.fail(j.wire.To, ErrLoops)
			}
			return found
		}
		w.loops++
		w.forget(j.wire.To)
		// what the walk is still on its way out of stays entered, so that a
		// wire back to it is seen for the loop it is
		for _, id := range j.path {
			w.seen[id] = true
		}
		w.seen[j.wire.To] = true
		w.path = j.path
		w.enter(j.wire)
	}
	return found
}

// forget lets the nodes that follow id be entered again.
func (w *walker) forget(id string) {
	todo := []string{id}
	gone := map[string]bool{id: true}
	for len(todo) > 0 {
		at := todo[len(todo)-1]
		todo = todo[:len(todo)-1]
		delete(w.seen, at)
		for _, e := range w.c.next[at] {
			if !gone[e.To] {
				gone[e.To] = true
				todo = append(todo, e.To)
			}
		}
	}
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

// reaches reports whether the event gets as far as an action, judging by the
// conditions that stand between the triggers and the first actions. Nothing runs, and no instance is asked: a condition put to one
// counts as letting the event out by any of its ways.
func (c *compiledGraph) reaches(ev *event.Event) (bool, error) {
	env := Env(ev, nil)
	var first error
	w := &walker{c: c, env: func() map[string]any { return env }, seen: map[string]bool{},
		fail: func(id string, err error) {
			if first == nil {
				first = fmt.Errorf("条件 %s: %w", id, err)
			}
		}}
	return w.run(c.starts(ev)), first
}

// Walk runs a graph rule for an event. act is called for each action reached,
// in order, with the variables its parameters may refer to; it runs the
// step, records its output in outputs (where later nodes read it as
// steps.<id>) and reports whether the walk may go on past it. fail is called for a condition that cannot be evaluated, or with
// ErrLoops for the node a loop was stopped at, and judge answers the
// conditions that are put to an instance. Among the variables, "loop" is how
// many times the walk has gone back round a loop so far. note, when given, is
// told of every node the walk comes to, the trigger it starts at included
// (TriggerNode for the rule's own), before the node's action runs and once
// its condition is decided.
func (c *Compiled) Walk(ev *event.Event, outputs map[string]any, act func(Step, map[string]any) bool, fail func(id string, err error), judge Judge, note func(Visit)) {
	if c.graph == nil {
		return
	}
	w := &walker{c: c.graph, env: func() map[string]any { return Env(ev, outputs) }, seen: map[string]bool{},
		act: act, fail: fail, judge: judge, note: note}
	w.run(c.graph.starts(ev))
}
