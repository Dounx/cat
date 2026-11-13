package rules

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/dounx/cat/core/event"
)

func action(id string) Node {
	return Node{Step: Step{ID: id, Instance: "i", Action: "a"}, Kind: KindAction}
}

func condition(id, src string) Node {
	return Node{Step: Step{ID: id}, Kind: KindCondition, Condition: src}
}

// ran walks the rule for an event and lists the actions run, in order. Each
// action's output is {"ok": true} unless it is named in failing, which also
// ends its branch.
func ran(t *testing.T, r Rule, ev *event.Event, failing ...string) []string {
	t.Helper()
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	outputs := map[string]any{}
	c.Walk(ev, outputs, func(s Step, _ map[string]any) bool {
		got = append(got, s.ID)
		for _, f := range failing {
			if f == s.ID {
				outputs[s.ID] = map[string]any{}
				return false
			}
		}
		outputs[s.ID] = map[string]any{"ok": true}
		return true
	}, func(id string, err error) { t.Fatalf("condition %s: %v", id, err) }, nil, nil)
	return got
}

func TestGraphBranches(t *testing.T) {
	// trigger → isDeploy ─┬─ yes → notify → log
	//                     │        └─ isProd (after isDeploy) → page
	//                     └─ else → ignore
	// trigger → always (an action straight off the trigger)
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			condition("isDeploy", `payload.text contains "deploy"`),
			condition("isProd", `payload.env == "prod" and steps.notify.ok`),
			action("notify"), action("log"), action("page"), action("ignore"), action("always"),
		},
		Edges: []Edge{
			{From: TriggerNode, To: "isDeploy"},
			{From: "isDeploy", To: "notify"},
			{From: "notify", To: "log"},
			{From: "notify", To: "isProd"},
			{From: "isProd", To: "page"},
			{From: "isDeploy", To: "ignore", Branch: BranchElse},
			{From: TriggerNode, To: "always"},
		},
	}}
	cases := []struct {
		payload map[string]any
		failing []string
		want    []string
	}{
		{map[string]any{"text": "deploy ok", "env": "prod"}, nil, []string{"notify", "log", "page", "always"}},
		{map[string]any{"text": "deploy ok", "env": "dev"}, nil, []string{"notify", "log", "always"}},
		{map[string]any{"text": "hello"}, nil, []string{"ignore", "always"}},
		// a failed action ends its own branch only
		{map[string]any{"text": "deploy ok", "env": "prod"}, []string{"notify"}, []string{"notify", "always"}},
	}
	for _, tc := range cases {
		got := ran(t, r, &event.Event{Payload: tc.payload}, tc.failing...)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("payload %v: ran %v, want %v", tc.payload, got, tc.want)
		}
	}
}

func TestGraphMatch(t *testing.T) {
	// conditions chained before the only action: both must hold to match
	r := Rule{Types: []string{"message.*"}, Graph: &Graph{
		Nodes: []Node{condition("a", `payload.n > 1`), condition("b", `payload.n < 5`), action("do")},
		Edges: []Edge{{From: TriggerNode, To: "a"}, {From: "a", To: "b"}, {From: "b", To: "do"}},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	for n, want := range map[int]bool{0: false, 3: true, 9: false} {
		got, err := c.Match(&event.Event{Type: "message.created", Payload: map[string]any{"n": n}})
		if err != nil || got != want {
			t.Errorf("n=%d: match %v (err %v), want %v", n, got, err, want)
		}
	}
	if ok, _ := c.Match(&event.Event{Type: "notice", Payload: map[string]any{"n": 3}}); ok {
		t.Error("an event outside the rule's types must not match")
	}
}

func TestGraphNodeRunsOnce(t *testing.T) {
	// two paths meet at "end": it runs once
	r := Rule{Graph: &Graph{
		Nodes: []Node{action("a"), action("b"), action("end")},
		Edges: []Edge{
			{From: TriggerNode, To: "a"}, {From: TriggerNode, To: "b"},
			{From: "a", To: "end"}, {From: "b", To: "end"},
		},
	}}
	if got, want := ran(t, r, &event.Event{}), []string{"a", "end", "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestGraphLoop(t *testing.T) {
	// trigger → draft ─┬─ good? ─ yes → send
	//             ↑    │         └ else ─┐
	//             │    └─ note           │
	//             └──────────────────────┘
	loop := func(cond string) Rule {
		return Rule{Graph: &Graph{
			Nodes: []Node{action("draft"), condition("good", cond), action("note"), action("send")},
			Edges: []Edge{
				{From: TriggerNode, To: "draft"},
				{From: "draft", To: "good"},
				{From: "draft", To: "note"},
				{From: "good", To: "send"},
				{From: "good", To: "draft", Branch: BranchElse},
			},
		}}
	}
	// Going back starts over from the node the wire leads to, once the rest
	// of the pass is done: what follows it runs again each time round.
	got := ran(t, loop(`loop >= 2`), &event.Event{})
	want := []string{"draft", "note", "draft", "note", "draft", "send", "note"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
	// a rule that loops still matches by its first pass
	c, err := Compile(loop(`loop >= 2`))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Match(&event.Event{}); !ok || err != nil {
		t.Errorf("match = %v, %v", ok, err)
	}

	// A loop nothing ends is stopped, and the walk says where.
	c, err = Compile(loop(`false`))
	if err != nil {
		t.Fatal(err)
	}
	drafts, stopped := 0, ""
	c.Walk(&event.Event{}, map[string]any{}, func(s Step, _ map[string]any) bool {
		if s.ID == "draft" {
			drafts++
		}
		return true
	}, func(id string, err error) {
		if !errors.Is(err, ErrLoops) {
			t.Fatalf("%s: %v", id, err)
		}
		stopped = id
	}, nil, nil)
	if drafts != MaxLoops+1 || stopped != "draft" {
		t.Errorf("drafted %d times and stopped at %q", drafts, stopped)
	}

	// a failed action ends the loop with its branch
	if got, want := ran(t, loop(`false`), &event.Event{}, "draft"), []string{"draft"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestGraphLoopsWithin(t *testing.T) {
	// A loop inside a loop, with two ways through the inner one that meet:
	// trigger → outer → a ─┬→ left ─┬→ meet → again? ─ yes → a
	//             ↑        └→ right ┘          └ else → more? ─ yes → outer
	//             └───────────────────────────────────────────┘  └ else → end
	// The inner loop goes round once for each pass of the outer one, which
	// goes round once itself.
	outputs := map[string]any{}
	n := map[string]int{}
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			action("outer"), action("a"), action("left"), action("right"), action("meet"), action("end"),
			condition("again", `steps.a.n % 2 == 1`), condition("more", `steps.outer.n < 2`),
		},
		Edges: []Edge{
			{From: TriggerNode, To: "outer"}, {From: "outer", To: "a"},
			{From: "a", To: "left"}, {From: "a", To: "right"},
			{From: "left", To: "meet"}, {From: "right", To: "meet"},
			{From: "meet", To: "again"},
			{From: "again", To: "a"},
			{From: "again", To: "more", Branch: BranchElse},
			{From: "more", To: "outer"},
			{From: "more", To: "end", Branch: BranchElse},
		},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	c.Walk(&event.Event{}, outputs, func(s Step, _ map[string]any) bool {
		got = append(got, s.ID)
		n[s.ID]++
		outputs[s.ID] = map[string]any{"n": n[s.ID]}
		return true
	}, func(id string, err error) { t.Fatalf("%s: %v", id, err) }, nil, nil)
	pass := []string{"a", "left", "meet", "right"}
	var want []string
	for i := 0; i < 2; i++ {
		want = append(want, "outer")
		want = append(want, pass...)
		want = append(want, pass...)
	}
	// the last time round, "end" comes as soon as the conditions let it
	want = append(want[:len(want)-1], "end", "right")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ran %v\nwant %v", got, want)
	}
}

func TestGraphLoopEntered(t *testing.T) {
	// Two triggers come into the same loop at different nodes; whichever way
	// the event came in, the wire that closes the round is the one that
	// leads back to a node it has been through.
	trigger := func(id, typ string) Node {
		return Node{Step: Step{ID: id}, Kind: KindTrigger, Types: []string{typ}}
	}
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			trigger("t1", "one"), trigger("t2", "two"),
			action("x"), action("y"), condition("on", `loop < 1`),
		},
		Edges: []Edge{
			{From: "t1", To: "x"}, {From: "t2", To: "y"},
			{From: "x", To: "y"}, {From: "y", To: "on"}, {From: "on", To: "x"},
		},
	}}
	for typ, want := range map[string][]string{
		"one": {"x", "y", "x", "y"},
		"two": {"y", "x", "y"},
	} {
		if got := ran(t, r, &event.Event{Type: typ}); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: ran %v, want %v", typ, got, want)
		}
	}
}

func TestGraphRejected(t *testing.T) {
	cases := map[string]Graph{
		// a loop of actions alone could never end
		"循环": {Nodes: []Node{action("a"), action("b")}, Edges: []Edge{{From: "a", To: "b"}, {From: "b", To: "a"}}},
		"重复": {Nodes: []Node{action("a"), action("a")}},
		"终点": {Nodes: []Node{action("a")}, Edges: []Edge{{From: TriggerNode, To: "gone"}}},
		"分支": {Nodes: []Node{action("a"), action("b")}, Edges: []Edge{{From: "a", To: "b", Branch: BranchElse}}},
		"类型": {Nodes: []Node{{Step: Step{ID: "x"}, Kind: "loop"}}},
		"条件": {Nodes: []Node{condition("c", `payload.text +`)}},
	}
	for want, g := range cases {
		_, err := Compile(Rule{Graph: &g})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want an error mentioning %q, got %v", want, err)
		}
	}
}

func TestGraphTriggers(t *testing.T) {
	// Two triggers with their own events and their own way on; "both" is
	// wired from each and runs once however the event came in.
	trigger := func(id, source, typ string) Node {
		return Node{Step: Step{ID: id}, Kind: KindTrigger, Sources: []string{source}, Types: []string{typ}}
	}
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			trigger("chat", "qq", "message.*"), trigger("feed", "rss", "feed.item"),
			action("reply"), action("post"), action("both"),
		},
		Edges: []Edge{
			{From: "chat", To: "reply"}, {From: "feed", To: "post"},
			{From: "chat", To: "both"}, {From: "feed", To: "both"},
		},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		source, typ string
		want        []string
	}{
		{"qq", "message.created", []string{"reply", "both"}},
		{"rss", "feed.item", []string{"post", "both"}},
		{"rss", "message.created", nil}, // right type, wrong source for it
		{"tg", "message.created", nil},
	}
	for _, tc := range cases {
		ev := &event.Event{Source: tc.source, Type: tc.typ}
		if got := ran(t, r, ev); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s %s: ran %v, want %v", tc.source, tc.typ, got, tc.want)
		}
		if ok, _ := c.Match(ev); ok != (tc.want != nil) {
			t.Errorf("%s %s: match %v", tc.source, tc.typ, ok)
		}
	}
	if _, err := Compile(Rule{Graph: &Graph{
		Nodes: []Node{trigger("a", "x", "y"), action("b")},
		Edges: []Edge{{From: "b", To: "a"}},
	}}); err == nil || !strings.Contains(err.Error(), "触发") {
		t.Errorf("a wire into a trigger should be rejected, got %v", err)
	}
}

func TestGraphChoices(t *testing.T) {
	// trigger → level (an expression giving one of three) ─ high → page
	//                                                      ├ low → log
	//                                                      └ else → ignore
	// trigger → kind (asked of an instance) ─ bug → fix
	//                                        └ else → skip
	// trigger → spam? (asked, yes or no) ─ yes → drop
	//                                     └ else → keep
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			{Step: Step{ID: "level"}, Kind: KindCondition, Condition: `payload.level`, Choices: []string{"high", "low", "3"}},
			{Step: Step{ID: "kind", Instance: "llm"}, Kind: KindCondition, Choices: []string{"bug", "question"}},
			{Step: Step{ID: "spam", Instance: "llm"}, Kind: KindCondition},
			action("page"), action("log"), action("three"), action("ignore"),
			action("fix"), action("skip"), action("drop"), action("keep"),
		},
		Edges: []Edge{
			{From: TriggerNode, To: "level"},
			{From: "level", To: "page", Branch: "high"},
			{From: "level", To: "log", Branch: "low"},
			{From: "level", To: "three", Branch: "3"},
			{From: "level", To: "ignore", Branch: BranchElse},
			{From: TriggerNode, To: "kind"},
			{From: "kind", To: "fix", Branch: "bug"},
			{From: "kind", To: "skip", Branch: BranchElse},
			{From: TriggerNode, To: "spam"},
			{From: "spam", To: "drop"},
			{From: "spam", To: "keep", Branch: BranchElse},
		},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		level any
		kind  any
		spam  any
		want  []string
	}{
		{"high", "bug", true, []string{"page", "fix", "drop"}},
		// a choice with nothing wired to it leads nowhere, not to else
		{"low", "question", false, []string{"log", "keep"}},
		// a number is told apart by how it prints; nothing listed goes to else
		{3, "other", false, []string{"three", "skip", "keep"}},
		{nil, "bug", true, []string{"ignore", "fix", "drop"}},
	}
	for _, tc := range cases {
		ev := &event.Event{Type: "x", Payload: map[string]any{"level": tc.level}}
		var got []string
		c.Walk(ev, map[string]any{}, func(s Step, _ map[string]any) bool {
			got = append(got, s.ID)
			return true
		}, func(id string, err error) { t.Fatalf("condition %s: %v", id, err) },
			func(n *Node, _ map[string]any) (any, error) {
				if n.ID == "kind" {
					return tc.kind, nil
				}
				return tc.spam, nil
			}, nil)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v: ran %v, want %v", tc, got, tc.want)
		}
	}

	// Matching asks no one: a condition put to an instance may come out any
	// way, so the event matches as long as one of those ways leads somewhere.
	asked := Rule{Graph: &Graph{
		Nodes: []Node{{Step: Step{ID: "kind", Instance: "llm"}, Kind: KindCondition, Choices: []string{"bug", "question"}}, action("fix")},
		Edges: []Edge{{From: TriggerNode, To: "kind"}, {From: "kind", To: "fix", Branch: "question"}},
	}}
	c, err = Compile(asked)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := c.Match(&event.Event{Type: "x"}); !ok || err != nil {
		t.Fatalf("match = %v, %v", ok, err)
	}

	for name, g := range map[string]*Graph{
		"a branch that is not a choice": {
			Nodes: []Node{{Step: Step{ID: "c"}, Kind: KindCondition, Condition: "type", Choices: []string{"a", "b"}}, action("x")},
			Edges: []Edge{{From: "c", To: "x", Branch: "z"}},
		},
		"no branch out of a condition with choices": {
			Nodes: []Node{{Step: Step{ID: "c"}, Kind: KindCondition, Condition: "type", Choices: []string{"a", "b"}}, action("x")},
			Edges: []Edge{{From: "c", To: "x"}},
		},
		"a choice named else":   {Nodes: []Node{{Step: Step{ID: "c"}, Kind: KindCondition, Choices: []string{"a", BranchElse}}}},
		"a choice listed twice": {Nodes: []Node{{Step: Step{ID: "c"}, Kind: KindCondition, Choices: []string{"a", "a"}}}},
	} {
		if _, err := Compile(Rule{Graph: g}); err == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

// A walk tells of every node it comes to: by which wire, and how each
// condition came out. A loop comes to its nodes again, by the wire back.
func TestGraphWalkNotesItsPath(t *testing.T) {
	outputs := map[string]any{}
	r := Rule{Graph: &Graph{
		Nodes: []Node{
			{Step: Step{ID: "t"}, Kind: KindTrigger},
			action("a"), condition("again", "steps.a.n < 2"), action("end"),
			{Step: Step{ID: "bad"}, Kind: KindCondition, Condition: "payload.x.y > 1"},
		},
		Edges: []Edge{
			{From: "t", To: "a"}, {From: "a", To: "again"},
			{From: "again", To: "a"},
			{From: "again", To: "end", Branch: BranchElse},
			{From: "end", To: "bad"},
		},
	}}
	c, err := Compile(r)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	var got []Visit
	c.Walk(&event.Event{}, outputs, func(s Step, _ map[string]any) bool {
		n++
		outputs[s.ID] = map[string]any{"n": n}
		return true
	}, func(string, error) {}, nil, func(v Visit) {
		if v.Error != "" {
			v.Error = "failed"
		}
		got = append(got, v)
	})
	want := []Visit{
		{Node: "t"},
		{Node: "a", From: "t"},
		{Node: "again", From: "a", Value: true},
		{Node: "a", From: "again"},
		{Node: "again", From: "a", Branch: BranchElse, Value: false},
		{Node: "end", From: "again", Via: BranchElse},
		{Node: "bad", From: "end", Error: "failed"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("visits:\n got %+v\nwant %+v", got, want)
	}
}
