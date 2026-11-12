package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	cat "github.com/dounx/cat/sdk/go"
	"github.com/dounx/cat/sdk/go/cattest"
)

var shanghai = mustLoad("Asia/Shanghai")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

func at(loc *time.Location, s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		panic(err)
	}
	return t
}

// clock is a wall clock the test moves by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// The plugin runs on one fake clock for the whole test binary: loops left over
// from earlier tests keep reading it, so it is never swapped out.
var fake = &clock{}

func init() { now, tick = fake.now, 5*time.Millisecond }

// useClock sets the fake clock to start.
func useClock(_ *testing.T, start time.Time) *clock {
	fake.set(start)
	return fake
}

func start(t *testing.T, cfg map[string]any, state map[string]string) *cattest.Harness {
	t.Helper()
	cfg["timezone"] = "Asia/Shanghai"
	h := cattest.New(t, New(), cattest.Options{Config: cfg, State: state})
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	return h
}

func quiet(t *testing.T, h *cattest.Harness) {
	t.Helper()
	select {
	case ev := <-h.Events:
		t.Fatalf("unexpected event: %v", ev.Payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNext(t *testing.T) {
	from := "2026-11-10 08:59:30" // a Tuesday
	cases := []struct{ when, want string }{
		{"9:00", "2026-11-10 09:00:00"},
		{"8：30", "2026-11-11 08:30:00"},
		{"0 9 * * *", "2026-11-10 09:00:00"},
		{"*/15 * * * *", "2026-11-10 09:00:00"},
		{"59 8 * * *", "2026-11-11 08:59:00"}, // this minute has already begun
		{"0 9 * * 1-5", "2026-11-10 09:00:00"},
		{"0 9 * * sat,sun", "2026-11-14 09:00:00"},
		{"0 9 * * 7", "2026-11-15 09:00:00"},
		{"30 18 1 * *", "2026-12-01 18:30:00"},
		{"0 0 1 jan *", "2027-01-01 00:00:00"},
		{"0 9 13 * 5", "2026-11-13 09:00:00"}, // day-of-month or day-of-week
		{"0 9 15 * 5", "2026-11-13 09:00:00"},
		{"0 0 29 2 *", "2028-02-29 00:00:00"},
		{"0 8-18/5 * * *", "2026-11-10 13:00:00"},
		{"@every 30m", "2026-11-10 09:00:00"},
		{"@every 2h", "2026-11-10 10:00:00"}, // on round times, not from when Cat started
		{"@every 48h", "2026-11-11 00:00:00"},
		{"@daily", "2026-11-11 00:00:00"},
		{"@hourly", "2026-11-10 09:00:00"},
	}
	for _, c := range cases {
		s, used, err := parseWhen(strings.Fields(c.when + " 内容"))
		if err != nil {
			t.Errorf("%s: %v", c.when, err)
			continue
		}
		if want := len(strings.Fields(c.when)); used != want {
			t.Errorf("%s: used %d fields, want %d", c.when, used, want)
		}
		if got := s.next(at(shanghai, from), shanghai).Format("2006-01-02 15:04:05"); got != c.want {
			t.Errorf("%s: next = %s, want %s", c.when, got, c.want)
		}
	}
}

// Clocks that jump must not stall or loop the search.
func TestNextAcrossClockChange(t *testing.T) {
	la := mustLoad("America/Los_Angeles")
	s, _, err := parseWhen(strings.Fields("30 2 * * *"))
	if err != nil {
		t.Fatal(err)
	}
	// 02:30 does not exist on 2026-03-08: the next one is a day later.
	if got := s.next(at(la, "2026-03-08 01:00:00"), la).Format("2006-01-02 15:04"); got != "2026-03-09 02:30" {
		t.Errorf("spring forward: next = %s", got)
	}
	if got := s.next(at(la, "2026-11-01 00:00:00"), la).Format("2006-01-02 15:04"); got != "2026-11-01 02:30" {
		t.Errorf("fall back: next = %s", got)
	}
}

func TestBadConfig(t *testing.T) {
	fake.set(at(shanghai, "2026-11-10 08:00:00"))
	for _, schedules := range []string{
		"25:00", "9:60", "0 9 * *", "60 * * * *", "0 9 * * 8", "0 9 5-1 * *", "*/0 * * * *",
		"@every", "@every soon", "@every 30s", "@every 90s", "0 0 30 2 *", "每天九点 开会",
	} {
		h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"schedules": schedules}})
		if err := h.Start(); err == nil {
			t.Errorf("schedules %q: want an error", schedules)
		}
	}
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"schedules": "9:00", "timezone": "Mars/Olympus"}})
	if err := h.Start(); err == nil {
		t.Error("unknown time zone: want an error")
	}
}

func TestFiresOnTime(t *testing.T) {
	c := useClock(t, at(shanghai, "2026-11-10 08:59:30"))
	h := start(t, map[string]any{"schedules": "# 早报\n0 9 * * *   汇总昨天的  GitHub 通知\n30 9 * * *\n"}, nil)
	quiet(t, h)

	c.set(at(shanghai, "2026-11-10 09:00:02"))
	ev := h.NextEvent()
	if ev.Type != "schedule.fired" || ev.Actor != nil {
		t.Fatalf("event = %+v", ev)
	}
	want := map[string]any{
		"text": "汇总昨天的  GitHub 通知", "schedule": "0 9 * * *", "scheduled": "2026-11-10T09:00:00+08:00",
		"date": "2026-11-10", "time": "09:00", "weekday": "周二", "missed": false, "manual": false,
	}
	for k, v := range want {
		if ev.Payload[k] != v {
			t.Errorf("payload.%s = %v, want %v", k, ev.Payload[k], v)
		}
	}
	if ev.Conversation.ID != "0 9 * * * 汇总昨天的  GitHub 通知" || ev.Conversation.Name != "汇总昨天的  GitHub 通知" {
		t.Errorf("conversation = %+v", ev.Conversation)
	}
	quiet(t, h) // once per scheduled time

	c.set(at(shanghai, "2026-11-10 09:30:00"))
	if ev := h.NextEvent(); ev.Payload["text"] != "" || ev.Conversation.Name != "30 9 * * *" {
		t.Errorf("second schedule: %+v %v", ev.Conversation, ev.Payload)
	}
	c.set(at(shanghai, "2026-11-11 09:00:00"))
	if ev := h.NextEvent(); ev.Payload["date"] != "2026-11-11" {
		t.Errorf("next day: %v", ev.Payload)
	}
}

// Rules say when to fire in their trigger nodes: the instance's own settings
// may be empty, and each event names the triggers that asked for its time.
func TestSchedulesFromRules(t *testing.T) {
	c := useClock(t, at(shanghai, "2026-11-10 08:59:30"))
	h := cattest.New(t, New(), cattest.Options{Config: map[string]any{"timezone": "Asia/Shanghai"}})
	// Cat hands the triggers over before the instance starts.
	errs, err := h.SetTriggers(
		cat.Trigger{ID: "t-9", Params: map[string]any{"schedules": "9:00"}},
		cat.Trigger{ID: "t-both", Params: map[string]any{"schedules": "9:00\n10:00 写周报"}},
		cat.Trigger{ID: "t-bad", Params: map[string]any{"schedules": "每天九点"}},
	)
	if err != nil || len(errs) != 1 || !strings.Contains(errs["t-bad"], "看不懂的定时") {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	quiet(t, h)
	c.set(at(shanghai, "2026-11-10 09:00:01"))
	if ev := h.NextEvent(); ev.Payload["time"] != "09:00" || strings.Join(ev.Triggers, ",") != "t-9,t-both" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}
	quiet(t, h) // one event for a time, however many rules ask for it

	// A rule is added while the instance runs. Its time counts from now: the
	// 9:30 that has passed is not fired late.
	c.set(at(shanghai, "2026-11-10 09:40:00"))
	if errs, err := h.SetTriggers(
		cat.Trigger{ID: "t-both", Params: map[string]any{"schedules": "9:00\n10:00 写周报"}},
		cat.Trigger{ID: "t-half", Params: map[string]any{"schedules": "30 * * * *"}},
	); err != nil || len(errs) != 0 {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	quiet(t, h)
	c.set(at(shanghai, "2026-11-10 10:00:01"))
	if ev := h.NextEvent(); ev.Payload["text"] != "写周报" || strings.Join(ev.Triggers, ",") != "t-both" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}
	c.set(at(shanghai, "2026-11-10 10:30:01"))
	if ev := h.NextEvent(); ev.Payload["time"] != "10:30" || strings.Join(ev.Triggers, ",") != "t-half" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}

	// With no rule asking there is nothing to fire, which is not a failure.
	if _, err := h.SetTriggers(); err != nil {
		t.Fatal(err)
	}
	c.set(at(shanghai, "2026-11-11 09:00:01"))
	quiet(t, h)
	if out, err := h.Invoke("fire", nil, nil); err != nil || out["fired"] != float64(0) {
		t.Fatalf("out=%v err=%v", out, err)
	}
}

// The machine slept through 9:00. By default that occurrence is dropped.
func TestMissedIsSkipped(t *testing.T) {
	c := useClock(t, at(shanghai, "2026-11-10 08:00:00"))
	h := start(t, map[string]any{"schedules": "9:00 早报"}, nil)
	c.set(at(shanghai, "2026-11-10 11:20:00"))
	quiet(t, h)
	c.set(at(shanghai, "2026-11-11 09:00:10"))
	if ev := h.NextEvent(); ev.Payload["date"] != "2026-11-11" || ev.Payload["missed"] != false {
		t.Errorf("payload = %v", ev.Payload)
	}
}

func TestCatchUp(t *testing.T) {
	c := useClock(t, at(shanghai, "2026-11-10 08:00:00"))
	h := start(t, map[string]any{"schedules": "@every 1h 喝水", "catch_up": true}, nil)
	// Three were missed; one event makes up for them, for the latest.
	c.set(at(shanghai, "2026-11-10 11:20:00"))
	ev := h.NextEvent()
	if ev.Payload["scheduled"] != "2026-11-10T11:00:00+08:00" || ev.Payload["missed"] != true {
		t.Errorf("payload = %v", ev.Payload)
	}
	quiet(t, h)
	// Waking up just after a scheduled time is not a miss.
	c.set(at(shanghai, "2026-11-10 15:00:40"))
	if ev := h.NextEvent(); ev.Payload["time"] != "15:00" || ev.Payload["missed"] != false {
		t.Errorf("payload = %v", ev.Payload)
	}
}

// Cat was not running at 9:00; with catch-up the next start makes up for it.
func TestCatchUpAcrossRestart(t *testing.T) {
	cfg := map[string]any{"schedules": "9:00 早报", "catch_up": true}
	c := useClock(t, at(shanghai, "2026-11-09 20:00:00"))
	h := start(t, cfg, nil)
	saved := h.State(stateKey("9:00 早报\x00Asia/Shanghai"))
	if saved == "" {
		t.Fatal("no state saved on first start")
	}

	c.set(at(shanghai, "2026-11-10 10:00:00"))
	h2 := start(t, cfg, map[string]string{stateKey("9:00 早报\x00Asia/Shanghai"): saved})
	if ev := h2.NextEvent(); ev.Payload["scheduled"] != "2026-11-10T09:00:00+08:00" || ev.Payload["missed"] != true {
		t.Errorf("payload = %v", ev.Payload)
	}
	// And only once: a third start has nothing left to make up for.
	h3 := start(t, cfg, map[string]string{stateKey("9:00 早报\x00Asia/Shanghai"): h2.State(stateKey("9:00 早报\x00Asia/Shanghai"))})
	quiet(t, h3)

	// Without catch-up the same restart stays silent.
	h4 := start(t, map[string]any{"schedules": "9:00 早报"}, map[string]string{stateKey("9:00 早报\x00Asia/Shanghai"): saved})
	quiet(t, h4)
}

func TestFireAction(t *testing.T) {
	useClock(t, at(shanghai, "2026-11-10 14:05:00"))
	h := start(t, map[string]any{"schedules": "9:00 早报\n0 18 * * 5 周报"}, nil)

	out, err := h.Invoke("fire", nil, nil)
	if err != nil || out["fired"] != float64(2) {
		t.Fatalf("fire all: out=%v err=%v", out, err)
	}
	var evs []cat.Event
	evs = append(evs, h.NextEvent(), h.NextEvent())
	if evs[0].Payload["text"] != "早报" || evs[1].Payload["text"] != "周报" || evs[0].Payload["manual"] != true || evs[0].Payload["time"] != "14:05" {
		t.Errorf("events = %v %v", evs[0].Payload, evs[1].Payload)
	}
	for _, want := range []string{"周报", "0 18 * * 5", "0 18 * * 5   周报"} {
		out, err := h.Invoke("fire", map[string]any{"schedule": want}, nil)
		if err != nil || out["fired"] != float64(1) {
			t.Fatalf("fire %q: out=%v err=%v", want, out, err)
		}
		if ev := h.NextEvent(); ev.Payload["text"] != "周报" {
			t.Errorf("fire %q: payload = %v", want, ev.Payload)
		}
	}
	if _, err := h.Invoke("fire", map[string]any{"schedule": "月报"}, nil); err == nil {
		t.Error("unknown schedule: want an error")
	}
	quiet(t, h) // firing by hand does not disturb the timetable
}

// A rule says beside its times which time zone they are in and whether a
// missed one is made up for; the instance needs no settings for it.
func TestSettingsFromRules(t *testing.T) {
	c := useClock(t, at(shanghai, "2026-11-10 07:59:00"))
	h := cattest.New(t, New(), cattest.Options{})
	errs, err := h.SetTriggers(
		cat.Trigger{ID: "t-sh", Params: map[string]any{"schedules": "9:00", "timezone": "Asia/Shanghai"}},
		cat.Trigger{ID: "t-tokyo", Params: map[string]any{"schedules": "9:00", "timezone": "Asia/Tokyo", "catch_up": true}},
		cat.Trigger{ID: "t-bad", Params: map[string]any{"schedules": "9:00", "timezone": "Mars/Base"}},
	)
	if err != nil || len(errs) != 1 || !strings.Contains(errs["t-bad"], "不认识的时区") {
		t.Fatalf("errs=%v err=%v", errs, err)
	}
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	// 9:00 in Tokyo is 8:00 in Shanghai: the same line, another time.
	c.set(at(shanghai, "2026-11-10 08:00:01"))
	if ev := h.NextEvent(); ev.Payload["scheduled"] != "2026-11-10T09:00:00+09:00" || strings.Join(ev.Triggers, ",") != "t-tokyo" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}
	c.set(at(shanghai, "2026-11-10 09:00:01"))
	if ev := h.NextEvent(); ev.Payload["scheduled"] != "2026-11-10T09:00:00+08:00" || strings.Join(ev.Triggers, ",") != "t-sh" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}
	// Both are slept through: only the rule that asked for it is caught up.
	c.set(at(shanghai, "2026-11-11 12:00:00"))
	if ev := h.NextEvent(); ev.Payload["missed"] != true || strings.Join(ev.Triggers, ",") != "t-tokyo" {
		t.Fatalf("event: %v for %v", ev.Payload, ev.Triggers)
	}
	quiet(t, h)
}
