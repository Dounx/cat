package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// spec says when a schedule fires: either a fixed interval, or the minutes
// matched by a cron expression.
type spec struct {
	every time.Duration // > 0: an interval, on round local times (30m: on the hour and half past)

	minute, hour, dom, month, dow uint64 // bit n set: value n matches
	domAny, dowAny                bool   // the field was written as "*"
}

var (
	clockRe = regexp.MustCompile(`^(\d{1,2})[:：](\d{2})$`)
	aliases = map[string]string{
		"@hourly":  "0 * * * *",
		"@daily":   "0 0 * * *",
		"@weekly":  "0 0 * * 0",
		"@monthly": "0 0 1 * *",
	}
	monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	dayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
)

const whenHelp = "应写成「9:00」（每天）、「@every 30m」（每隔一段时间）或 5 段的 cron 表达式（分 时 日 月 周，如 0 9 * * 1-5）"

// parseWhen reads the time part from the start of a line's fields and reports
// how many fields it used.
func parseWhen(fields []string) (spec, int, error) {
	if len(fields) == 0 {
		return spec{}, 0, errors.New("空行")
	}
	first := strings.ToLower(fields[0])
	if m := clockRe.FindStringSubmatch(first); m != nil {
		s, err := parseCron([]string{m[2], m[1], "*", "*", "*"})
		return s, 1, err
	}
	if first == "@every" {
		if len(fields) < 2 {
			return spec{}, 0, errors.New("@every 后面要写间隔，如 @every 30m、@every 2h")
		}
		d, err := time.ParseDuration(strings.ToLower(fields[1]))
		if err != nil {
			return spec{}, 0, fmt.Errorf("看不懂的间隔「%s」：如 30m、2h、1h30m", fields[1])
		}
		if d < time.Minute || d%time.Minute != 0 {
			return spec{}, 0, fmt.Errorf("间隔「%s」无效：最小 1 分钟，且必须是整分钟", fields[1])
		}
		return spec{every: d}, 2, nil
	}
	if expr, ok := aliases[first]; ok {
		s, err := parseCron(strings.Fields(expr))
		return s, 1, err
	}
	if len(fields) < 5 {
		return spec{}, 0, errors.New(whenHelp)
	}
	s, err := parseCron(fields[:5])
	return s, 5, err
}

func parseCron(f []string) (s spec, err error) {
	if s.minute, _, err = parseField(f[0], 0, 59, nil); err != nil {
		return s, fmt.Errorf("「分」%v", err)
	}
	if s.hour, _, err = parseField(f[1], 0, 23, nil); err != nil {
		return s, fmt.Errorf("「时」%v", err)
	}
	if s.dom, s.domAny, err = parseField(f[2], 1, 31, nil); err != nil {
		return s, fmt.Errorf("「日」%v", err)
	}
	if s.month, _, err = parseField(f[3], 1, 12, monthNames); err != nil {
		return s, fmt.Errorf("「月」%v", err)
	}
	if s.dow, s.dowAny, err = parseField(f[4], 0, 7, dayNames); err != nil {
		return s, fmt.Errorf("「周」%v", err)
	}
	if s.dow&(1<<7) != 0 { // 7 is Sunday too
		s.dow |= 1
	}
	return s, nil
}

// parseField parses one cron field: "*", "5", "1-5", "*/15", "1-30/2", and
// comma-separated lists of those.
func parseField(text string, lo, hi int, names map[string]int) (mask uint64, any bool, err error) {
	bad := fmt.Errorf("写法无效「%s」（范围 %d-%d）", text, lo, hi)
	num := func(s string) (int, bool) {
		if n, ok := names[strings.ToLower(s)]; ok {
			return n, true
		}
		n, err := strconv.Atoi(s)
		return n, err == nil && n >= lo && n <= hi
	}
	for _, part := range strings.Split(text, ",") {
		rng, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepText); err != nil || step < 1 {
				return 0, false, bad
			}
		}
		from, to := lo, hi
		if rng != "*" {
			a, b, isRange := strings.Cut(rng, "-")
			var ok bool
			if from, ok = num(a); !ok {
				return 0, false, bad
			}
			to = from
			if isRange {
				if to, ok = num(b); !ok || to < from {
					return 0, false, bad
				}
			} else if hasStep { // "5/10": from 5 on
				to = hi
			}
		}
		for v := from; v <= to; v += step {
			mask |= 1 << v
		}
	}
	return mask, strings.HasPrefix(text, "*"), nil
}

// next returns the first time after t that the schedule fires, in loc. It is
// the zero time for a schedule that never fires (February 30th).
func (s *spec) next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc)
	if s.every > 0 {
		_, offset := t.Zone()
		step := int64(s.every / time.Second)
		local := t.Unix() + int64(offset)
		return time.Unix((local/step+1)*step-int64(offset), 0).In(loc)
	}
	t = t.Truncate(time.Minute).Add(time.Minute)
	// Skip ahead a month, a day or an hour at a time where nothing can match.
	// A clock change can make the computed jump land at or before t; then
	// just step a minute, so the search always moves forward.
	jump := func(to time.Time) {
		if !to.After(t) {
			to = t.Add(time.Minute)
		}
		t = to
	}
	for limit := t.Year() + 8; t.Year() <= limit; {
		y, m, d := t.Date()
		switch {
		case s.month&(1<<int(m)) == 0:
			jump(time.Date(y, m+1, 1, 0, 0, 0, 0, loc))
		case !s.dayMatches(t):
			jump(time.Date(y, m, d+1, 0, 0, 0, 0, loc))
		case s.hour&(1<<t.Hour()) == 0:
			jump(time.Date(y, m, d, t.Hour()+1, 0, 0, 0, loc))
		case s.minute&(1<<t.Minute()) == 0:
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// dayMatches follows cron: when both day-of-month and day-of-week are
// restricted, either one matching is enough.
func (s *spec) dayMatches(t time.Time) bool {
	dom := s.dom&(1<<t.Day()) != 0
	dow := s.dow&(1<<int(t.Weekday())) != 0
	if s.domAny || s.dowAny {
		return dom && dow
	}
	return dom || dow
}
