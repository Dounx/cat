// Package watchlist keeps what a monitor watches: the items listed in the
// instance's own settings together with those the rules ask for in their
// trigger nodes. Both are written the same way, as lines of text under the
// same name, and an item wanted from several sides is watched once.
package watchlist

import (
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
	// Parse reads such a list. Empty text is no items, not an error.
	Parse func(text string) ([]T, error)
	// Key identifies an item: two that are written differently but mean the
	// same thing have the same key.
	Key func(T) string

	mu    sync.Mutex
	fixed []T                 // from the settings
	items []T                 // everything watched, the settings' first
	asked map[string][]string // key → the triggers that ask for it
}

// Init reads the items in the instance's settings.
func (l *List[T]) Init(cfg map[string]any) error {
	text, _ := cfg[l.Field].(string)
	fixed, err := l.Parse(text)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.fixed, l.items, l.asked = fixed, slices.Clone(fixed), nil
	l.mu.Unlock()
	return nil
}

// read parses what the triggers ask for: the items, for each the triggers
// that want it, and for each trigger that cannot be read, why.
func (l *List[T]) read(list []cat.Trigger) (items []T, by map[string][]string, errs map[string]string) {
	by, errs = map[string][]string{}, map[string]string{}
	for _, t := range list {
		text, _ := t.Params[l.Field].(string)
		found, err := l.Parse(text)
		if err != nil {
			errs[t.ID] = err.Error()
			continue
		}
		for _, it := range found {
			k := l.Key(it)
			if by[k] == nil {
				items = append(items, it)
			}
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
	have := map[string]T{}
	for _, it := range l.items {
		have[l.Key(it)] = it
	}
	var items []T
	on := map[string]bool{}
	for _, it := range append(slices.Clone(l.fixed), found...) {
		k := l.Key(it)
		if on[k] {
			continue // in the settings and asked for by a rule
		}
		on[k] = true
		if old, ok := have[k]; ok {
			it = old
		} else {
			added = append(added, it)
		}
		items = append(items, it)
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
