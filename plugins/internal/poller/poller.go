// Package poller looks at the things a monitor watches, each as often as it
// is asked to: pages, APIs, feeds. What is watched and how one of them is
// checked is the plugin's own; the pacing, the few checks at a time, what
// goes in the log and when the instance counts as failing are the same for
// all of them, and are here.
package poller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	cat "github.com/dounx/cat/sdk/go"
)

const parallel = 4

// Item is one watched thing: a pointer to whatever the plugin keeps for it.
type Item interface {
	comparable
	// Label names it in the log and in errors. It must not give away a
	// secret the address may carry.
	Label() string
	// Interval is the time between two looks at it.
	Interval() time.Duration
}

// Words are what a plugin calls a look gone wrong.
type Words struct {
	Failed    string // one item, in the log: "检查失败"
	AllFailed string // every item, as the instance's status: "网页全部检查失败"
}

// Poller checks the items of one instance.
type Poller[T Item] struct {
	items func() []T
	check func(context.Context, *cat.Host, T) (int, error)
	words Words
	wake  chan struct{} // what is watched has changed

	mu     sync.Mutex
	failed map[T]string // why the last look at an item went wrong
}

// New makes a poller for what items lists at the time. check looks at one
// item and says how many events it emitted; two looks at the same item may
// overlap (the loop and a check someone asks for), which is check's to sort
// out.
func New[T Item](items func() []T, check func(context.Context, *cat.Host, T) (int, error), words Words) *Poller[T] {
	return &Poller[T]{items: items, check: check, words: words, wake: make(chan struct{}, 1), failed: map[T]string{}}
}

// Result is the outcome of a round.
type Result struct {
	Checked int
	Emitted int      // events emitted, over all items
	Errors  []string // "label：reason", in the order the items were given
}

// Output is a round as the result of a "check" action, the events counted
// under the name the plugin gives them. A round in which nothing could be
// checked is an error.
func (r Result) Output(emitted string) (map[string]any, error) {
	errs := r.Errors
	if errs == nil {
		errs = []string{}
	}
	out := map[string]any{"checked": r.Checked, emitted: r.Emitted, "failed": len(errs), "errors": errs}
	if r.Checked > 0 && len(errs) == r.Checked {
		return out, errors.New(strings.Join(errs, "；"))
	}
	return out, nil
}

// Round checks the given items, a few at a time.
func (p *Poller[T]) Round(ctx context.Context, h *cat.Host, items []T) Result {
	errs := make([]string, len(items))
	counts := make([]int, len(items))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, it := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			n, err := p.check(ctx, h, it)
			counts[i] = n
			if ctx.Err() != nil {
				return // stopped half way: says nothing about the item
			}
			msg := ""
			if err != nil {
				msg = err.Error()
				errs[i] = it.Label() + "：" + msg
			}
			// Log transitions only, so an item that stays down doesn't flood the log.
			p.mu.Lock()
			flipped := msg != p.failed[it]
			if msg != "" {
				p.failed[it] = msg
			} else {
				delete(p.failed, it)
			}
			p.mu.Unlock()
			if flipped && msg != "" {
				h.Log("warn", "%s %s：%s", it.Label(), p.words.Failed, msg)
			} else if flipped {
				h.Log("info", "%s 已恢复", it.Label())
			}
		}()
	}
	wg.Wait()
	res := Result{Checked: len(items)}
	for i := range items {
		res.Emitted += counts[i]
		if errs[i] != "" {
			res.Errors = append(res.Errors, errs[i])
		}
	}
	return res
}

// status says what is wrong when no item can be checked, and forgets the
// items that are no longer watched. The instance is only "in error" when
// nothing works; a single broken item is reported in the log and shouldn't
// hide the healthy ones.
func (p *Poller[T]) status(items []T) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	watched := make(map[T]bool, len(items))
	first, failing := "", 0
	for _, it := range items {
		watched[it] = true
		if msg := p.failed[it]; msg != "" {
			if failing++; first == "" {
				first = p.words.AllFailed + "，将继续重试：" + it.Label() + "：" + msg
			}
		}
	}
	for it := range p.failed {
		if !watched[it] {
			delete(p.failed, it)
		}
	}
	if failing < len(items) {
		return ""
	}
	return first
}

// Run looks at each item as often as it is asked to, starting with a look at
// once: on start, and at an item that has just been added. It returns when
// ctx is done.
func (p *Poller[T]) Run(ctx context.Context, h *cat.Host) {
	status := ""
	last := map[T]time.Time{}
	for {
		items := p.items()
		now := time.Now()
		var due []T
		seen := make(map[T]time.Time, len(items))
		for _, it := range items {
			if t, ok := last[it]; ok && now.Sub(t) < it.Interval() {
				seen[it] = t
				continue
			}
			seen[it] = now
			due = append(due, it)
		}
		last = seen // items no longer watched are forgotten
		if len(due) > 0 {
			p.Round(ctx, h, due)
		}
		if ctx.Err() != nil {
			return
		}
		if next := p.status(items); next != status {
			if next != "" {
				h.SetStatus("error", next)
			} else {
				h.SetStatus("running", "")
			}
			status = next
		}
		wait := time.Hour
		for _, it := range items {
			wait = min(wait, time.Until(last[it].Add(it.Interval())))
		}
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-time.After(max(wait, time.Second)):
		}
	}
}

// Wake has Run look again now: a new item is checked at once, and a new pace
// is kept from now.
func (p *Poller[T]) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}
