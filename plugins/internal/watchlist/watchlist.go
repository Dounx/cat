// Package watchlist keeps what a monitor watches: the items listed in the
// instance's own settings together with those the rules ask for in their
// trigger nodes. Both are written the same way, as lines of text under the
// same name, and an item wanted from several sides is watched once. How to
// watch (how often, what to leave out) is said beside the list, and a rule
// that says nothing of it goes by the instance's settings.
package watchlist

import (
	"maps"
	"slices"
	"sync"

	cat "github.com/dounx/cat/sdk/go"
)

// List is the items an instance watches. T is a pointer to whatever the
// plugin keeps per item; an item that stays on the list keeps its value, and
// with it what the plugin remembers about it.
type List[T comparable] struct {
	// Field names the setting, and the trigger parameter, that lists items.
	Field string
	// Parse reads the items a set of parameters lists under Field, each
	// carrying what the other parameters say about how to watch it. No list
	// is no items, not an error.
	Parse func(params map[string]any) ([]T, error)
	// Key identifies an item: two that are written differently but mean the
	// same thing, watched the same way, have the same key.
	Key func(T) string
	// Merge, when set, is given an item on the list together with every way
	// it is wanted now, as freshly read, to settle what the key leaves out:
	// how often to look, say.
	Merge func(kept T, wanted []T)

	mu    sync.Mutex
	base  map[string]any      // the settings, without their own list
	fixed []T                 // from the settings
	items []T                 // everything watched, the settings' first
	asked map[string][]string // key → the triggers that ask for it
}

// Init reads the items in the instance's settings.
func (l *List[T]) Init(cfg map[string]any) error {
	base := maps.Clone(cfg)
	delete(base, l.Field)
	l.mu.Lock()
	l.base = base
	l.mu.Unlock()
	fixed, err := l.Parse(cfg)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.fixed, l.asked = fixed, nil
	l.items = l.join(nil, fixed)
	l.mu.Unlock()
	return nil
}

// join puts the wanted items on a list, each once, keeping those already
// there (have) as they are. The caller holds l.mu.
func (l *List[T]) join(have []T, wanted []T) []T {
	old := map[string]T{}
	for _, it := range have {
		old[l.Key(it)] = it
	}
	var items []T
	same := map[string][]T{}
	for _, it := range wanted {
		k := l.Key(it)
		if same[k] == nil {
			if o, ok := old[k]; ok {
				items = append(items, o)
			} else {
				items = append(items, it)
			}
		}
		same[k] = append(same[k], it)
	}
	if l.Merge != nil {
		for _, it := range items {
			l.Merge(it, same[l.Key(it)])
		}
	}
	return items
}

// read parses what the triggers ask for: the items (one for each time it is
// asked for), for each the triggers that want it, and for each trigger that
// cannot be read, why. What a trigger leaves unsaid is as in the settings.
func (l *List[T]) read(list []cat.Trigger) (items []T, by map[string][]string, errs map[string]string) {
	by, errs = map[string][]string{}, map[string]string{}
	l.mu.Lock()
	base := l.base
	l.mu.Unlock()
	for _, t := range list {
		params := maps.Clone(base)
		if params == nil {
			params = map[string]any{}
		}
		maps.Copy(params, t.Params)
		found, err := l.Parse(params)
		if err != nil {
			errs[t.ID] = err.Error()
			continue
		}
		for _, it := range found {
			k := l.Key(it)
			items = append(items, it)
			if !slices.Contains(by[k], t.ID) {
				by[k] = append(by[k], t.ID)
			}
		}
	}
	return items, by, errs
}

// Check says what is wrong with the triggers that cannot be read, by trigger
// id, without changing what is watched.
func (l *List[T]) Check(list []cat.Trigger) map[string]string {
	_, _, errs := l.read(list)
	return errs
}

// Ask sets what the rules ask for beside the settings' own items. It returns
// the items that are new on the list and what is wrong with the triggers
// that could not be read.
func (l *List[T]) Ask(list []cat.Trigger) (added []T, errs map[string]string) {
	found, by, errs := l.read(list)
	l.mu.Lock()
	defer l.mu.Unlock()
	have := map[string]bool{}
	for _, it := range l.items {
		have[l.Key(it)] = true
	}
	// an item in the settings and asked for by a rule is on the list once
	items := l.join(l.items, append(slices.Clone(l.fixed), found...))
	for _, it := range items {
		if !have[l.Key(it)] {
			added = append(added, it)
		}
	}
	l.items, l.asked = items, by
	return added, errs
}

// Items lists what is watched now.
func (l *List[T]) Items() []T {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.items)
}

// AskedBy lists the triggers that ask for an item: what goes in the Triggers
// of the events emitted for it.
func (l *List[T]) AskedBy(key string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked[key])
}
