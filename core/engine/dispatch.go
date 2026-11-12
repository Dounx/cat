package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/id"
	"github.com/dounx/cat/core/plugin"
	"github.com/dounx/cat/core/rules"
	"github.com/dounx/cat/core/store"
)

// ---- rules ----

func (e *Engine) reloadRules() error {
	list, err := e.Store.ListRules()
	if err != nil {
		return err
	}
	compiled := make([]*rules.Compiled, 0, len(list))
	errs := map[string]string{}
	for _, r := range list {
		c, err := rules.Compile(r)
		if err != nil {
			errs[r.ID] = err.Error()
			c = nil
		}
		if c != nil {
			compiled = append(compiled, c)
		}
	}
	e.mu.Lock()
	e.rules, e.ruleErrs = compiled, errs
	e.mu.Unlock()
	e.syncTriggers()
	return nil
}

func (e *Engine) Rules() ([]RuleView, error) {
	list, err := e.Store.ListRules()
	if err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]RuleView, len(list))
	for i, r := range list {
		out[i] = RuleView{Rule: r, Error: e.ruleErrs[r.ID]}
		if out[i].Error != "" || !r.Enabled {
			continue
		}
		// a trigger its source could not make sense of
	asks:
		for _, a := range r.Asks() {
			for _, src := range a.Sources {
				if msg := e.trigErrs[src][a.ID]; msg != "" {
					out[i].Error = msg
					break asks
				}
			}
		}
	}
	return out, nil
}

// SaveRule validates and stores a rule. Invalid expressions are rejected.
func (e *Engine) SaveRule(r rules.Rule) (rules.Rule, error) {
	if r.Name == "" {
		return r, fmt.Errorf("规则名称不能为空")
	}
	if r.Graph != nil {
		// a graph stands in for the single condition and the list of steps
		r.Condition, r.Steps = "", []rules.Step{}
		for _, n := range r.Graph.Nodes {
			if n.Kind == rules.KindAction && (n.Instance == "" || n.Action == "") {
				return r, fmt.Errorf("动作 %s 缺少目标或动作", n.ID)
			}
			if n.Kind == rules.KindCondition && n.Instance == "" && n.Action != "" {
				return r, fmt.Errorf("条件 %s 没有选由谁来判断", n.ID)
			}
		}
	}
	seen := map[string]bool{}
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.ID == "" {
			s.ID = fmt.Sprintf("s%d", i+1)
		}
		if seen[s.ID] {
			return r, fmt.Errorf("步骤标识重复：%s", s.ID)
		}
		seen[s.ID] = true
		if s.Instance == "" || s.Action == "" {
			return r, fmt.Errorf("步骤 %s 缺少目标或动作", s.ID)
		}
	}
	if _, err := rules.Compile(r); err != nil {
		return r, err
	}
	if r.ID == "" {
		r.ID = id.New()
	}
	if err := e.Store.SaveRule(&r); err != nil {
		return r, err
	}
	return r, e.reloadRules()
}

func (e *Engine) DeleteRule(ruleID string) error {
	if err := e.Store.DeleteRule(ruleID); err != nil {
		return err
	}
	return e.reloadRules()
}

// ---- events ----

// Emit stores an event, pushes it to subscribers and dispatches it to rules.
func (e *Engine) Emit(ev *event.Event) (string, error) {
	if ev.ID == "" {
		ev.ID = id.New()
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	if ev.Payload == nil {
		ev.Payload = map[string]any{}
	}
	if err := e.Store.SaveEvent(ev); err != nil {
		return "", err
	}
	e.publish(Message{Kind: "event", Data: ev})
	e.dispatch(ev, "", false)

	e.mu.Lock()
	e.emitted++
	prune := e.emitted%500 == 0
	e.mu.Unlock()
	if prune {
		if err := e.Store.PruneEvents(keepEvents); err != nil {
			e.log.Warn("prune events", "err", err)
		}
	}
	return ev.ID, nil
}

// Replay re-dispatches a stored event. With ruleID set only that rule is
// considered. Throttling is skipped. It returns how many rules matched.
func (e *Engine) Replay(eventID, ruleID string) (int, error) {
	ev, err := e.Store.GetEvent(eventID)
	if err != nil {
		return 0, err
	}
	return e.dispatch(ev, ruleID, true), nil
}

func (e *Engine) dispatch(ev *event.Event, onlyRule string, replay bool) int {
	e.mu.RLock()
	list := e.rules
	e.mu.RUnlock()
	matched := 0
	for _, r := range list {
		if onlyRule != "" && r.ID != onlyRule {
			continue
		}
		if !r.Enabled && !(replay && onlyRule != "") {
			continue
		}
		// Replays skip throttling. Otherwise a throttle node that would hold
		// the event back is looked at here, and counted when the rule runs.
		var look rules.Gate
		if !replay {
			look = func(n *rules.Node, env map[string]any) bool { return !e.held(r.ID, n, env, false) }
		}
		ok, err := r.MatchPast(ev, look)
		e.mu.Lock()
		if err != nil {
			e.ruleErrs[r.ID] = "条件求值失败：" + err.Error()
		} else if r.Scope(ev) {
			delete(e.ruleErrs, r.ID)
		}
		e.mu.Unlock()
		if !ok {
			continue
		}
		if !replay && e.throttled(r, ev) {
			continue
		}
		matched++
		e.wg.Add(1)
		go func(r *rules.Compiled) {
			defer e.wg.Done()
			e.sem <- struct{}{}
			defer func() { <-e.sem }()
			e.execute(r, ev, replay)
		}(r)
	}
	return matched
}

func (e *Engine) throttled(r *rules.Compiled, ev *event.Event) bool {
	if r.ThrottleSeconds <= 0 {
		return false
	}
	key := r.ID
	if r.ThrottleKey != "" {
		if v, err := rules.Eval(r.ThrottleKey, rules.Env(ev, nil)); err == nil {
			key += "\x00" + fmt.Sprint(v)
		}
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.throttle[key]; ok && now.Sub(last) < time.Duration(r.ThrottleSeconds)*time.Second {
		return true
	}
	e.throttle[key] = now
	return false
}

// held reports whether a throttle node holds a walk back: whether it let one
// past, for the same key, less than its window ago. With count set, a walk
// that is not held starts a new window.
func (e *Engine) held(ruleID string, n *rules.Node, env map[string]any, count bool) bool {
	if n.ThrottleSeconds <= 0 {
		return false
	}
	key := ruleID + "\x00" + n.ID
	if n.ThrottleKey != "" {
		if v, err := rules.Eval(n.ThrottleKey, env); err == nil {
			key += "\x00" + fmt.Sprint(v)
		}
	}
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if last, ok := e.throttle[key]; ok && now.Sub(last) < time.Duration(n.ThrottleSeconds)*time.Second {
		return true
	}
	if count {
		e.throttle[key] = now
	}
	return false
}

// TestCondition evaluates a condition against a stored event without side effects.
func (e *Engine) TestCondition(condition, eventID string) (bool, error) {
	ev, err := e.Store.GetEvent(eventID)
	if err != nil {
		return false, err
	}
	c, err := rules.Compile(rules.Rule{Condition: condition})
	if err != nil {
		return false, err
	}
	return c.Match(ev)
}

// ---- pipeline ----

func (e *Engine) execute(r *rules.Compiled, ev *event.Event, replay bool) *store.Run {
	run := &store.Run{
		ID: id.New(), RuleID: r.ID, RuleName: r.Name, EventID: ev.ID,
		Status: "running", StartedAt: time.Now(), Steps: []store.StepResult{}, Replay: replay,
	}
	// A graph rule's run is recorded from its first action on: an event that
	// a throttle holds back after all (another got past it in the meantime),
	// or that takes a branch leading nowhere, leaves no run behind.
	recorded := r.Graph == nil
	if recorded {
		e.saveRun(run)
	}
	outputs := map[string]any{}
	failed := false
	// act runs one step and reports whether what follows it may run.
	act := func(step rules.Step) bool {
		if !recorded {
			recorded = true
			e.saveRun(run)
		}
		res := e.runStep(step, ev, r.ID, run.ID, outputs)
		run.Steps = append(run.Steps, res)
		if res.Output != nil {
			outputs[step.ID] = res.Output
		} else {
			outputs[step.ID] = map[string]any{}
		}
		if res.Error != "" {
			failed = true
			if !step.ContinueOnError {
				return false
			}
		}
		e.saveRun(run)
		return true
	}
	if r.Graph != nil {
		// A failed action ends its own branch; the other branches still run.
		r.Walk(ev, outputs, act, func(nodeID string, err error) {
			failed, recorded = true, true
			run.Steps = append(run.Steps, store.StepResult{ID: nodeID, Action: "condition", Error: "条件求值失败：" + err.Error()})
			e.saveRun(run)
		}, func(n *rules.Node, env map[string]any) bool {
			return replay || !e.held(r.ID, n, env, true)
		}, func(n *rules.Node, _ map[string]any) (any, error) {
			// Asking is real work, so the run shows from here on, and what
			// the instance said is there for later nodes as steps.<id>.
			recorded = true
			res, verdict := e.judge(n, ev, r.ID, run.ID, outputs)
			if res.Error != "" {
				return nil, errors.New(res.Error)
			}
			run.Steps = append(run.Steps, res)
			outputs[n.ID] = res.Output
			e.saveRun(run)
			return verdict, nil
		})
	} else {
		for _, step := range r.Steps {
			if !act(step) {
				break
			}
		}
	}
	if !recorded {
		return nil
	}
	now := time.Now()
	run.FinishedAt = &now
	run.Status = "ok"
	if failed {
		run.Status = "error"
	}
	e.saveRun(run)
	return run
}

// TryRule runs a rule, saved or not, for a stored event and waits for the
// outcome. The actions are really carried out; throttles are skipped, as in a
// replay. It returns nil when the event starts nothing in the rule: no trigger
// takes it, or every path ends before an action.
func (e *Engine) TryRule(r rules.Rule, eventID string) (*store.Run, error) {
	ev, err := e.Store.GetEvent(eventID)
	if err != nil {
		return nil, err
	}
	if r.Name == "" {
		r.Name = "未命名规则"
	}
	c, err := rules.Compile(r)
	if err != nil {
		return nil, err
	}
	if c.Graph == nil {
		if ok, err := c.Match(ev); err != nil || !ok {
			return nil, err
		}
	}
	return e.execute(c, ev, true), nil
}

// judge puts a condition node to its instance: the node's parameters and the
// choices to pick from (yes or no when the node lists none). The instance
// answers with the one it picked under "choice". The verdict is that choice,
// or for a yes-or-no condition whether it was yes.
func (e *Engine) judge(n *rules.Node, ev *event.Event, ruleID, runID string, outputs map[string]any) (store.StepResult, any) {
	step := n.Step
	if step.Action == "" {
		step.Action = rules.JudgeAction
	}
	choices := n.Choices
	if len(choices) == 0 {
		choices = []string{rules.JudgeYes, rules.JudgeNo}
	}
	step.Params = map[string]any{}
	for k, v := range n.Params {
		step.Params[k] = v
	}
	list := make([]any, len(choices))
	for i, c := range choices {
		list[i] = c
	}
	step.Params["choices"] = list
	res := e.runStep(step, ev, ruleID, runID, outputs)
	if res.Error != "" {
		return res, nil
	}
	choice, ok := res.Output["choice"].(string)
	if !ok {
		res.Error = "没有给出判断结果（输出里缺少 choice）"
		return res, nil
	}
	if len(n.Choices) == 0 {
		return res, choice == rules.JudgeYes
	}
	return res, choice
}

func (e *Engine) saveRun(run *store.Run) {
	if err := e.Store.SaveRun(run); err != nil {
		e.log.Error("save run", "err", err)
	}
	copied := *run
	copied.Steps = append([]store.StepResult(nil), run.Steps...)
	e.publish(Message{Kind: "run", Data: copied})
}

func (e *Engine) runStep(step rules.Step, ev *event.Event, ruleID, runID string, outputs map[string]any) store.StepResult {
	target := step.Instance
	if target == rules.SourceInstance {
		target = ev.Source
	}
	res := store.StepResult{ID: step.ID, Instance: target, Action: step.Action}
	start := time.Now()
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	rendered, err := rules.Render(map[string]any(step.Params), rules.Env(ev, outputs))
	if err != nil {
		res.Error = "参数模板错误：" + err.Error()
		res.DurationMS = time.Since(start).Milliseconds()
		return res
	}
	params, _ := rendered.(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	res.Params = params

	timeout := defaultStepTimeout
	if step.TimeoutSeconds > 0 {
		timeout = time.Duration(step.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := e.Invoke(ctx, target, step.Action, params, plugin.InvokeContext{Event: ev, RuleID: ruleID, RunID: runID})
	res.Output = out
	if err != nil {
		res.Error = err.Error()
	}
	res.DurationMS = time.Since(start).Milliseconds()
	return res
}
