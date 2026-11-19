package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dounx/cat/core/blob"
	"github.com/dounx/cat/core/builtin"
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
		} else if keep, err := e.Store.BlobPaths(); err == nil {
			blob.Sweep(e.BlobsDir(), keep, time.Hour)
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
		ok, err := r.Match(ev)
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
	return e.executeFrom(r, ev, replay, nil)
}

// TypingInterval is how often an active "typing" step is refreshed while a run
// continues working before replying. Tests shorten it.
var TypingInterval = 4 * time.Second

// CancelRun stops a running rule execution if it is still in flight.
func (e *Engine) CancelRun(runID string) bool {
	e.runsMu.Lock()
	cancel, ok := e.runs[runID]
	e.runsMu.Unlock()
	if !ok {
		return false
	}
	cancel()
	return true
}

// executeFrom runs a rule for an event, from its triggers or, with from set,
// from past a step that held an earlier run: that step stands in the new run
// as done.
func (e *Engine) executeFrom(r *rules.Compiled, ev *event.Event, replay bool, from *store.StepResult) *store.Run {
	run := &store.Run{
		ID: id.New(), RuleID: r.ID, RuleName: r.Name, EventID: ev.ID,
		Status: "running", StartedAt: time.Now(), Steps: []store.StepResult{}, Replay: replay,
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	e.runsMu.Lock()
	e.runs[run.ID] = cancelRun
	e.runsMu.Unlock()
	defer func() {
		cancelRun()
		e.runsMu.Lock()
		delete(e.runs, run.ID)
		e.runsMu.Unlock()
	}()
	typingStops := map[string]context.CancelFunc{}
	stopTyping := func(target string) {
		if cancel, ok := typingStops[target]; ok {
			cancel()
			delete(typingStops, target)
		}
	}
	defer func() {
		for k, cancel := range typingStops {
			cancel()
			delete(typingStops, k)
		}
	}()
	// A graph rule's run is recorded from its first action on: an event that
	// takes a branch leading nowhere leaves no run behind.
	recorded := r.Graph == nil
	snapshot := r.Rule
	run.Rule = &snapshot
	outputs := map[string]any{}
	if from != nil {
		recorded = true
		run.Resumed = from.ID
		run.Steps = append(run.Steps, *from)
		outputs[from.ID] = from.Output
	}
	if recorded {
		e.saveRun(run)
	}
	failed := false
	// act runs one step and reports whether what follows it may run.
	act := func(step rules.Step, env map[string]any) bool {
		if runCtx.Err() != nil {
			return false
		}
		if !recorded {
			recorded = true
			e.saveRun(run)
		}
		target := step.Instance
		if target == rules.SourceInstance {
			target = ev.Source
		}
		if step.Action == "reply" || step.Action == "send" {
			stopTyping(target)
		}
		res := e.runStep(runCtx, step, ev, r.ID, run.ID, env)
		run.Steps = append(run.Steps, res)
		if res.Output != nil {
			outputs[step.ID] = res.Output
		} else {
			outputs[step.ID] = map[string]any{}
		}
		if runCtx.Err() != nil {
			e.saveRun(run)
			return false
		}
		if res.Error == "" && !res.Held && step.Action == "typing" && TypingInterval > 0 {
			if _, active := typingStops[target]; !active {
				tctx, tcancel := context.WithCancel(runCtx)
				typingStops[target] = tcancel
				params := res.Params
				ictx := plugin.InvokeContext{Event: ev, RuleID: r.ID, RunID: run.ID, StepID: step.ID}
				go func(tgt string, p map[string]any) {
					tk := time.NewTicker(TypingInterval)
					defer tk.Stop()
					for {
						select {
						case <-tctx.Done():
							return
						case <-e.stopping:
							return
						case <-tk.C:
							cctx, ccancel := context.WithTimeout(tctx, 10*time.Second)
							_, _ = e.Invoke(cctx, tgt, "typing", p, ictx)
							ccancel()
						}
					}
				}(target, params)
			}
		}
		if res.Error != "" {
			failed = true
			if !step.ContinueOnError {
				return false
			}
		}
		e.saveRun(run)
		// a step that holds ends its branch here, without having failed
		return !res.Held
	}
	if r.Graph != nil {
		walk := r.Walk
		if from != nil {
			walk = func(ev *event.Event, outputs map[string]any, act func(rules.Step, map[string]any) bool, fail func(string, error), judge rules.Judge, note func(rules.Visit)) {
				r.WalkFrom(from.ID, ev, outputs, act, fail, judge, note)
			}
		}
		// A failed action ends its own branch; the other branches still run.
		walk(ev, outputs, act, func(nodeID string, err error) {
			if runCtx.Err() != nil {
				return
			}
			failed, recorded = true, true
			res := store.StepResult{ID: nodeID, Action: "condition", Error: "条件求值失败：" + err.Error()}
			if errors.Is(err, rules.ErrLoops) {
				res.Action, res.Error = "loop", err.Error()
			}
			run.Steps = append(run.Steps, res)
			e.saveRun(run)
		}, func(n *rules.Node, env map[string]any) (any, error) {
			if runCtx.Err() != nil {
				return nil, runCtx.Err()
			}
			// Asking is real work, so the run shows from here on, and what
			// the instance said is there for later nodes as steps.<id>.
			if !recorded {
				recorded = true
				e.saveRun(run)
			}
			res, verdict := e.judge(runCtx, n, ev, r.ID, run.ID, env)
			if runCtx.Err() != nil {
				run.Steps = append(run.Steps, res)
				e.saveRun(run)
				return nil, runCtx.Err()
			}
			if res.Error != "" {
				return nil, errors.New(res.Error)
			}
			run.Steps = append(run.Steps, res)
			outputs[n.ID] = res.Output
			e.saveRun(run)
			return verdict, nil
		}, func(v rules.Visit) { run.Trace = append(run.Trace, v) })
	} else {
		steps := r.Steps
		if from != nil { // the steps after the one that held
			for i, s := range steps {
				if s.ID == from.ID {
					steps = steps[i+1:]
					break
				}
			}
		}
		for _, step := range steps {
			if !act(step, rules.Env(ev, outputs)) {
				break
			}
		}
	}
	if !recorded {
		return nil
	}
	now := time.Now()
	run.FinishedAt = &now
	switch {
	case runCtx.Err() != nil:
		run.Status = "cancelled"
	case failed:
		run.Status = "error"
	default:
		run.Status = "ok"
	}
	e.saveRun(run)
	if failed && runCtx.Err() == nil && !replay {
		e.reportFailure(run, ev)
	}
	return run
}

// awaitStarted waits, for a while, for an instance that is still starting:
// an event that comes in while the hub starts up finds the instance its rule
// uses on the way up, not down.
func (e *Engine) awaitStarted(ctx context.Context, instanceID string) {
	deadline := time.After(startWait)
	for {
		e.mu.RLock()
		r, ok := e.running[instanceID]
		starting := ok && r.status == StatusStarting
		e.mu.RUnlock()
		if !starting {
			return
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			return
		case <-ctx.Done():
			return
		case <-e.stopping:
			return
		}
	}
}

// reportFailure emits a run.failed event for a run that ended in an error, so
// that a rule can tell someone. A run started by such an event is not
// reported in turn: a rule that fails to tell would otherwise go round.
func (e *Engine) reportFailure(run *store.Run, ev *event.Event) {
	if ev.Plugin == builtin.SelfID {
		return
	}
	var lines []string
	var errs []any
	for _, s := range run.Steps {
		if s.Error == "" {
			continue
		}
		what := s.ID
		if s.Instance != "" {
			name, action := e.describe(s.Instance, s.Action)
			what = fmt.Sprintf("%s（%s · %s）", s.ID, name, action)
		}
		lines = append(lines, what+"："+s.Error)
		errs = append(errs, map[string]any{"step": s.ID, "instance": s.Instance, "action": s.Action, "error": s.Error})
	}
	text := fmt.Sprintf("规则「%s」运行失败", run.RuleName)
	if run.Status == "interrupted" {
		text = fmt.Sprintf("规则「%s」的一次运行被打断：Cat 在它跑完之前退出了（%s 开始，做完了 %d 步）",
			run.RuleName, run.StartedAt.Local().Format("1月2日 15:04"), len(run.Steps))
	}
	if len(lines) > 0 {
		text += "\n" + strings.Join(lines, "\n")
	}
	payload := map[string]any{
		"text": text, "rule_id": run.RuleID, "rule_name": run.RuleName, "run_id": run.ID,
		"event_id": run.EventID, "errors": errs, "interrupted": run.Status == "interrupted",
	}
	if len(errs) > 0 {
		first := errs[0].(map[string]any)
		payload["step"], payload["error"] = first["step"], first["error"]
	}
	_, err := e.Emit(&event.Event{
		Source: builtin.SelfID, Plugin: builtin.SelfID, Type: builtin.RunFailed,
		Conversation: &event.Conversation{ID: run.RuleID, Name: run.RuleName},
		Payload:      payload,
	})
	if err != nil {
		e.log.Warn("report failed run", "run", run.ID, "err", err)
	}
}

// describe gives what an instance and one of its actions are called, or
// their ids when they are gone.
func (e *Engine) describe(instanceID, action string) (string, string) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.running[instanceID]
	if !ok {
		return instanceID, action
	}
	for _, a := range e.plugins[r.rec.Plugin].manifest.Actions {
		if a.Name == action && a.Title != "" {
			action = a.Title
		}
	}
	if r.rec.Name != "" {
		return r.rec.Name, action
	}
	return instanceID, action
}

// Resume runs what follows a step of a rule as a new run, the step standing
// as done with the given output: how an action that held a run (see
// plugin.ErrHeld) lets it go on later. instanceID is the instance the step
// was carried out by.
func (e *Engine) Resume(ruleID, stepID, eventID string, output map[string]any, instanceID string) error {
	e.mu.RLock()
	var r *rules.Compiled
	for _, c := range e.rules {
		if c.ID == ruleID {
			r = c
		}
	}
	e.mu.RUnlock()
	if r == nil {
		return plugin.ErrGone
	}
	var step *rules.Step
	for i := range r.Steps {
		if r.Steps[i].ID == stepID {
			step = &r.Steps[i]
		}
	}
	if r.Graph != nil {
		for i := range r.Graph.Nodes {
			if n := &r.Graph.Nodes[i]; n.ID == stepID && n.Kind == rules.KindAction {
				step = &n.Step
			}
		}
	}
	if step == nil {
		return plugin.ErrGone
	}
	if !r.Enabled {
		return fmt.Errorf("规则已停用：%s", r.Name)
	}
	ev, err := e.Store.GetEvent(eventID)
	if err != nil { // pruned by now: the run still needs one to be for
		ev = &event.Event{ID: eventID, Source: instanceID, Plugin: instanceID, Type: "resumed", Time: time.Now(), Payload: map[string]any{}}
	}
	from := &store.StepResult{ID: stepID, Instance: instanceID, Action: step.Action, Output: output}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.sem <- struct{}{}
		defer func() { <-e.sem }()
		e.executeFrom(r, ev, false, from)
	}()
	return nil
}

// TryRule runs a rule, saved or not, for a stored event and waits for the
// outcome. The actions are really carried out. It returns nil when the event starts nothing in the rule: no trigger
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
func (e *Engine) judge(runCtx context.Context, n *rules.Node, ev *event.Event, ruleID, runID string, env map[string]any) (store.StepResult, any) {
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
	res := e.runStep(runCtx, step, ev, ruleID, runID, env)
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
	copied.Trace = append([]rules.Visit(nil), run.Trace...)
	copied.Rule = nil
	e.publish(Message{Kind: "run", Data: copied})
}

// runStep carries out one step, its parameters rendered with env.
func (e *Engine) runStep(runCtx context.Context, step rules.Step, ev *event.Event, ruleID, runID string, env map[string]any) store.StepResult {
	target := step.Instance
	if target == rules.SourceInstance {
		target = ev.Source
	}
	res := store.StepResult{ID: step.ID, Instance: target, Action: step.Action}
	start := time.Now()
	defer func() { res.DurationMS = time.Since(start).Milliseconds() }()

	rendered, err := rules.Render(map[string]any(step.Params), env)
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
	wait := defaultRetryWait
	if step.RetryWaitSeconds > 0 {
		wait = time.Duration(step.RetryWaitSeconds) * time.Second
	}
	e.awaitStarted(runCtx, target)
	if runCtx.Err() != nil {
		res.Error = "已手动停止"
		return res
	}
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(runCtx, timeout)
		out, err := e.Invoke(ctx, target, step.Action, params, plugin.InvokeContext{Event: ev, RuleID: ruleID, RunID: runID, StepID: step.ID})
		cancel()
		res.Output, res.Error = out, ""
		if attempt > 1 {
			res.Attempts = attempt
		}
		if runCtx.Err() != nil {
			res.Error = "已手动停止"
			break
		}
		if errors.Is(err, plugin.ErrHeld) {
			res.Held = true
			break
		}
		if err == nil {
			break
		}
		res.Error = err.Error()
		if attempt > step.Retries {
			break
		}
		e.log.Info("step failed, trying again", "rule", ruleID, "step", step.ID, "attempt", attempt, "wait", wait, "err", err)
		select {
		case <-time.After(wait):
		case <-runCtx.Done():
			res.Error = "已手动停止"
			return res
		case <-e.stopping:
			return res
		}
		if wait *= 2; wait > maxRetryWait {
			wait = maxRetryWait
		}
	}
	return res
}
