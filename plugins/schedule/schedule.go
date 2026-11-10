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

	cursor time.Time // the schedule has been dealt with up to here
}

type scheduler struct {
	entries []*entry
	loc     *time.Location
	catchUp bool

	mu sync.Mutex // guards the cursors
}

// firing is one occasion on which an entry fires.
type firing struct {
	e      *entry
	at     time.Time // the time it was scheduled for
	missed bool      // found out too late
	manual bool      // asked for by the "fire" action
}

// parseSchedules turns the "schedules" setting into entries. Each line is a
// time, optionally followed by the text to send along.
func parseSchedules(text string, loc *time.Location) ([]*entry, error) {
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
		e := &entry{When: strings.Join(fields[:used], " "), Text: strings.TrimSpace(rest), spec: s}
		e.Line = strings.TrimSpace(e.When + " " + e.Text)
		if s.next(now(), loc).IsZero() {
			return nil, fmt.Errorf("定时「%s」永远不会触发", e.When)
		}
		if seen[e.Line] {
			continue
		}
		seen[e.Line] = true
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, errors.New("还没有填定时")
	}
	return out, nil
}

func newScheduler(cfg map[string]any) (*scheduler, error) {
	str := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }
	s := &scheduler{loc: time.Local}
	if tz := str("timezone"); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf("不认识的时区「%s」：应填 Asia/Shanghai 这样的名字", tz)
		}
		s.loc = loc
	}
	s.catchUp, _ = cfg["catch_up"].(bool)
	var err error
	s.entries, err = parseSchedules(str("schedules"), s.loc)
	return s, err
}

func stateKey(line string) string { return "last:" + hash(line) }

func hash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// resume decides where each schedule picks up. Normally that is now; with
// catch-up it is where the previous run left off, so times that passed while
// Cat was not running are noticed.
func (s *scheduler) resume(h *cat.Host) {
	t := now()
	for _, e := range s.entries {
		e.cursor = t
		if !s.catchUp {
			continue
		}
		raw, _ := h.StateGet(stateKey(e.Line))
		if last, err := time.Parse(time.RFC3339, raw); err == nil && last.Before(t) {
			e.cursor = last
		} else {
			s.save(h, e)
		}
	}
}

func (s *scheduler) save(h *cat.Host, e *entry) {
	if err := h.StateSet(stateKey(e.Line), e.cursor.Format(time.RFC3339)); err != nil {
		h.Log("error", "%s：保存状态失败：%v", e.Line, err)
	}
}

// step returns what is due at t and moves the cursors past it. An entry fires
// at most once per step, however many of its times have gone by.
func (s *scheduler) step(t time.Time) []firing {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []firing
	for _, e := range s.entries {
		due := e.spec.next(e.cursor, s.loc)
		if due.IsZero() || due.After(t) {
			continue
		}
		// Several times may have passed: the latest one is the one that counts.
		for range 100000 {
			n := e.spec.next(due, s.loc)
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
	for _, e := range s.entries {
		if n := e.spec.next(e.cursor, s.loc); !n.IsZero() && (first.IsZero() || n.Before(first)) {
			first = n
		}
	}
	return first
}

func (s *scheduler) event(f firing) cat.Event {
	at := f.at.In(s.loc)
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
	if f.missed && !s.catchUp {
		h.Log("info", "「%s」错过了 %s 的触发，已跳过", f.e.Line, f.at.In(s.loc).Format("01-02 15:04"))
		return nil
	}
	if _, err := h.Emit(s.event(f)); err != nil {
		h.Log("error", "「%s」发出事件失败：%v", f.e.Line, err)
		return err
	}
	if f.missed {
		h.Log("info", "「%s」错过了 %s 的触发，已补发", f.e.Line, f.at.In(s.loc).Format("01-02 15:04"))
	}
	return nil
}

func (s *scheduler) run(ctx context.Context, h *cat.Host) {
	for {
		for _, f := range s.step(now()) {
			// Saved first: a crash in between loses one event rather than
			// repeating it on every start.
			if s.catchUp {
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

// New builds the plugin.
func New() cat.Plugin {
	var s *scheduler
	return cat.Plugin{
		Start: func(ctx context.Context, h *cat.Host) error {
			var err error
			if s, err = newScheduler(h.Config); err != nil {
				return err
			}
			s.resume(h)
			h.Log("info", "%d 个定时，时区 %s，下一次触发：%s", len(s.entries), s.loc, s.soonest().In(s.loc).Format("2006-01-02 15:04"))
			go s.run(ctx, h)
			return nil
		},
		Actions: map[string]cat.ActionFunc{
			"fire": func(_ context.Context, h *cat.Host, p map[string]any, _ cat.ActionContext) (any, error) {
				if s == nil {
					return nil, errors.New("实例尚未启动")
				}
				entries := s.entries
				if want, _ := p["schedule"].(string); strings.TrimSpace(want) != "" {
					want = strings.Join(strings.Fields(want), " ")
					entries = nil
					for _, e := range s.entries {
						if want == e.Line || want == e.When || want == e.Text || want == strings.Join(strings.Fields(e.Line), " ") {
							entries = append(entries, e)
						}
					}
					if entries == nil {
						return nil, fmt.Errorf("配置里没有这个定时：%s", want)
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
