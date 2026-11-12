package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dounx/cat/plugins/internal/watchlist"
	cat "github.com/dounx/cat/sdk/go"
)

// Replaced in tests.
var (
	now = time.Now
	// tick bounds how long the loop sleeps. Timers stand still while the
	// machine is asleep, so the wall clock is looked at this often instead of
	// trusting one long timer.
	tick = 20 * time.Second
)

// grace is how late a schedule may fire and still count as on time. Anything
// later was missed (the machine was asleep, or Cat was not running).
const grace = 90 * time.Second

var weekdays = [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}

// entry is one line of the "schedules" setting.
type entry struct {
	Line string // the line as written: the conversation id and state key
	When string
	Text string
	spec spec

	// How it is kept, as said beside it: by the rule that asks for it, or by
	// the instance's settings.
	loc     *time.Location
	catchUp bool
	key     string // Line and the above: the same line in another time zone is another time

	cursor time.Time // the schedule has been dealt with up to here
}

type scheduler struct {
	// the schedules in the settings and those rules ask for in their triggers
	entries watchlist.List[*entry]

	mu sync.Mutex // guards the cursors
}

// firing is one occasion on which an entry fires.
type firing struct {
	e      *entry
	at     time.Time // the time it was scheduled for
	missed bool      // found out too late
	manual bool      // asked for by the "fire" action
}

// parseSchedules reads the times a rule's trigger (or the instance's
// settings) lists under "schedules", with what it says beside them: the time
// zone, and whether a missed time is made up for. Each line is a time,
// optionally followed by the text to send along.
func parseSchedules(params map[string]any) ([]*entry, error) {
	loc, err := location(params)
	if err != nil {
		return nil, err
	}
	catchUp, _ := params["catch_up"].(bool)
	how := ""
	if loc != time.Local {
		how = "\x00" + loc.String()
	}
	text, _ := params["schedules"].(string)
	var out []*entry
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		s, used, err := parseWhen(fields)
		if err != nil {
			return nil, fmt.Errorf("看不懂的定时「%s」：%v", strings.TrimSpace(line), err)
		}
		// Cut the text out of the line itself, so the spaces inside it survive.
		rest := line
		for range used {
			rest = strings.TrimLeft(rest, " \t")
			rest = rest[strings.IndexAny(rest+" ", " \t"):]
		}
		e := &entry{When: strings.Join(fields[:used], " "), Text: strings.TrimSpace(rest), spec: s, loc: loc, catchUp: catchUp}
		e.Line = strings.TrimSpace(e.When + " " + e.Text)
		if e.key = e.Line + how; catchUp {
			e.key += "\x00catch-up"
		}
		if s.next(now(), loc).IsZero() {
			return nil, fmt.Errorf("定时「%s」永远不会触发", e.When)
		}
		if seen[e.Line] {
			continue
		}
		seen[e.Line] = true
		out = append(out, e)
	}
	return out, nil
}

func entryList() watchlist.List[*entry] {
	return watchlist.List[*entry]{Field: "schedules", Parse: parseSchedules, Key: func(e *entry) string { return e.key }}
}

// location reads the "timezone" setting; the machine's own when it is empty.
func location(cfg map[string]any) (*time.Location, error) {
	tz, _ := cfg["timezone"].(string)
	if tz = strings.TrimSpace(tz); tz == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("不认识的时区「%s」：应填 Asia/Shanghai 这样的名字", tz)
	}
	return loc, nil
}

func newScheduler(cfg map[string]any) (*scheduler, error) {
	s := &scheduler{entries: entryList()}
	return s, s.entries.Init(cfg)
}

func stateKey(line string) string { return "last:" + hash(line) }

// state is where an entry's cursor is kept: under its line, and its time
// zone when it has one of its own.
func (e *entry) state() string {
	if e.loc != time.Local {
		return stateKey(e.Line + "\x00" + e.loc.String())
	}
	return stateKey(e.Line)
}

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// resume decides where each schedule picks up. Normally that is now; with
// catch-up it is where the previous run left off, so times that passed while
// Cat was not running are noticed. A schedule a rule has just asked for
// (fresh) always counts from now: what passed before it was asked for was
// not missed.
func (s *scheduler) resume(h *cat.Host, entries []*entry, fresh bool) {
	t := now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		e.cursor = t
		if !e.catchUp {
			continue
		}
		raw, _ := h.StateGet(e.state())
		if last, err := time.Parse(time.RFC3339, raw); err == nil && last.Before(t) && !fresh {
			e.cursor = last
		} else {
			s.save(h, e)
		}
	}
}

func (s *scheduler) save(h *cat.Host, e *entry) {
	if err := h.StateSet(e.state(), e.cursor.Format(time.RFC3339)); err != nil {
		h.Log("error", "%s：保存状态失败：%v", e.Line, err)
	}
}

// step returns what is due at t and moves the cursors past it. An entry fires
// at most once per step, however many of its times have gone by.
func (s *scheduler) step(t time.Time) []firing {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []firing
	for _, e := range s.entries.Items() {
		if e.cursor.IsZero() {
			continue // just asked for: not counting yet
		}
		due := e.spec.next(e.cursor, e.loc)
		if due.IsZero() || due.After(t) {
			continue
		}
		// Several times may have passed: the latest one is the one that counts.
		for range 100000 {
			n := e.spec.next(due, e.loc)
			if n.IsZero() || n.After(t) {
				break
			}
			due = n
		}
		e.cursor = t
		out = append(out, firing{e: e, at: due, missed: t.Sub(due) > grace})
	}
	return out
}

// soonest is the next time anything is due.
func (s *scheduler) soonest() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first time.Time
	for _, e := range s.entries.Items() {
		if e.cursor.IsZero() {
			continue
		}
		if n := e.spec.next(e.cursor, e.loc); !n.IsZero() && (first.IsZero() || n.Before(first)) {
			first = n.In(e.loc)
		}
	}
	return first
}

func (s *scheduler) event(f firing) cat.Event {
	at := f.at.In(f.e.loc)
	name := f.e.Text
	if name == "" {
		name = f.e.When
	}
	if r := []rune(name); len(r) > 40 {
		name = string(r[:40]) + "…"
	}
	return cat.Event{
		Type:         "schedule.fired",
		Conversation: &cat.Conversation{ID: f.e.Line, Kind: "channel", Name: name},
		Triggers:     s.entries.AskedBy(f.e.key), // the rules that asked for this time take it as theirs
		Payload: map[string]any{
			"message_id": fmt.Sprintf("%s-%d", hash(f.e.Line), at.Unix()),
			"text":       f.e.Text,
			"schedule":   f.e.When,
			"scheduled":  at.Format(time.RFC3339),
			"date":       at.Format("2006-01-02"),
			"time":       at.Format("15:04"),
			"weekday":    weekdays[at.Weekday()],
			"missed":     f.missed,
			"manual":     f.manual,
		},
	}
}

// fire emits the event for f, or drops it when it was missed and catching up
// is off.
func (s *scheduler) fire(h *cat.Host, f firing) error {
	if f.missed && !f.e.catchUp {
		h.Log("info", "「%s」错过了 %s 的触发，已跳过", f.e.Line, f.at.In(f.e.loc).Format("01-02 15:04"))
		return nil
	}
	if _, err := h.Emit(s.event(f)); err != nil {
		h.Log("error", "「%s」发出事件失败：%v", f.e.Line, err)
		return err
	}
	if f.missed {
		h.Log("info", "「%s」错过了 %s 的触发，已补发", f.e.Line, f.at.In(f.e.loc).Format("01-02 15:04"))
	}
	return nil
}

func (s *scheduler) run(ctx context.Context, h *cat.Host) {
	for {
		for _, f := range s.step(now()) {
			// Saved first: a crash in between loses one event rather than
			// repeating it on every start.
			if f.e.catchUp {
				s.save(h, f.e)
			}
			_ = s.fire(h, f)
		}
		wait := tick
		if n := s.soonest(); !n.IsZero() {
			wait = min(wait, max(n.Sub(now()), 20*time.Millisecond))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// describe says in the log what is scheduled.
func (s *scheduler) describe(h *cat.Host) {
	n := len(s.entries.Items())
	if n == 0 {
		h.Log("info", "还没有定时：在规则的触发节点里选「定时触发」后填写")
		return
	}
	h.Log("info", "%d 个定时，下一次触发：%s", n, s.soonest().Format("2006-01-02 15:04 MST"))
}

// New builds the plugin. When to fire is said in the trigger nodes of rules;
// an instance with settings of its own may list times there too, under the
// same names.
func New() cat.Plugin {
	var (
		mu sync.Mutex
		s  *scheduler // set once the instance has started
	)
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			ns, err := newScheduler(h.Config)
			if err != nil {
				return err
			}
			mu.Lock()
			s = ns
			ns.entries.Ask(h.Triggers()) // under mu: a change of triggers comes after, not in between
			ns.resume(h, ns.entries.Items(), false)
			mu.Unlock()
			ns.describe(h)
			go ns.run(ctx, h)
			return nil
		},
		// The rules changed what they ask for. Before the instance has started
		// there is only the reading of it to check; Start picks up the rest.
		Triggers: func(_ context.Context, h *cat.Host, list []cat.Trigger) map[string]string {
			mu.Lock()
			defer mu.Unlock()
			if s == nil {
				probe := entryList()
				if probe.Init(h.Config) != nil {
					return nil // Start says so
				}
				return probe.Check(list)
			}
			before := len(s.entries.Items())
			added, errs := s.entries.Ask(list)
			s.resume(h, added, true)
			if len(added) > 0 || len(s.entries.Items()) != before {
				s.describe(h)
			}
			return errs
		},
		Actions: map[string]cat.ActionFunc{
			"fire": func(_ context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				mu.Lock()
				s := s
				mu.Unlock()
				if s == nil {
					return nil, errors.New("实例尚未启动")
				}
				entries := s.entries.Items()
				if want, _ := p["schedule"].(string); strings.TrimSpace(want) != "" {
					want = strings.Join(strings.Fields(want), " ")
					all := entries
					entries = nil
					for _, e := range all {
						if want == e.Line || want == e.When || want == e.Text || want == strings.Join(strings.Fields(e.Line), " ") {
							entries = append(entries, e)
						}
					}
					if entries == nil {
						return nil, fmt.Errorf("没有这个定时：%s", want)
					}
				}
				fired := 0
				var last error
				for _, e := range entries {
					if err := s.fire(h, firing{e: e, at: now(), manual: true}); err != nil {
						last = err
						continue
					}
					fired++
				}
				return map[string]any{"fired": fired}, last
			},
		},
	}
}
