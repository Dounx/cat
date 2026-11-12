package engine

import (
	"context"
	"maps"
	"slices"
	"sort"
	"time"

	"github.com/dounx/cat/core/plugin"
)

// A rule's trigger node can ask its sources for something of its own: a feed
// to watch, say. The engine gathers what the enabled rules ask of each
// instance and hands it over, before the instance starts and again whenever
// the rules change. The instance tags the events it emits for a trigger with
// the trigger's id, which is how the node that asked finds them.

// triggersFor lists what the enabled rules ask of an instance, each thing
// once, in a stable order. The caller holds e.mu.
func (e *Engine) triggersFor(instanceID string) []plugin.Trigger {
	var list []plugin.Trigger
	seen := map[string]bool{}
	for _, r := range e.rules {
		if !r.Enabled {
			continue
		}
		for _, a := range r.Asks() {
			if seen[a.ID] || !slices.Contains(a.Sources, instanceID) {
				continue
			}
			seen[a.ID] = true
			list = append(list, plugin.Trigger{ID: a.ID, Params: a.Params})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func sameTriggers(a, b []plugin.Trigger) bool {
	return slices.EqualFunc(a, b, func(x, y plugin.Trigger) bool { return x.ID == y.ID })
}

// giveTriggers hands an instance its triggers. The caller holds e.trigMu, so
// that two changes in a row reach the instance in order.
func (e *Engine) giveTriggers(r *running, inst plugin.Instance, list []plugin.Trigger) {
	e.mu.Lock()
	r.triggers = list
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := inst.SetTriggers(ctx, list); err != nil {
		e.log.Warn("set triggers", "id", r.rec.ID, "err", err)
	}
}

// syncTriggers tells every instance whose triggers changed what they are now.
func (e *Engine) syncTriggers() {
	e.trigMu.Lock()
	defer e.trigMu.Unlock()
	type change struct {
		r    *running
		inst plugin.Instance
		list []plugin.Trigger
	}
	var changes []change
	e.mu.RLock()
	for id, r := range e.running {
		if r.inst == nil {
			continue
		}
		if list := e.triggersFor(id); !sameTriggers(list, r.triggers) {
			changes = append(changes, change{r, r.inst, list})
		}
	}
	e.mu.RUnlock()
	for _, c := range changes {
		e.giveTriggers(c.r, c.inst, c.list)
	}
}

// triggerErrors records what an instance found wrong with its triggers, and
// lets the UI know that the rules have something new to show.
func (e *Engine) triggerErrors(instanceID string, errs map[string]string) {
	e.mu.Lock()
	same := maps.Equal(e.trigErrs[instanceID], errs)
	if len(errs) == 0 {
		delete(e.trigErrs, instanceID)
	} else {
		e.trigErrs[instanceID] = maps.Clone(errs)
	}
	e.mu.Unlock()
	if !same {
		e.publish(Message{Kind: "rules", Data: map[string]string{"instance": instanceID}})
	}
}
