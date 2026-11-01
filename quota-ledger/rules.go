package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// span is a daily local-time range in minutes after midnight; End < Start wraps past midnight.
type span struct{ Start, End int }

func parseClock(s string) (int, error) {
	h, m, hasMin := strings.Cut(strings.TrimSpace(s), ":")
	hh, err := strconv.Atoi(h)
	mm := 0
	if err == nil && hasMin {
		mm, err = strconv.Atoi(m)
	}
	if err != nil || hh < 0 || hh > 24 || mm < 0 || mm > 59 || (hh == 24 && mm != 0) {
		return 0, fmt.Errorf("bad time %q (want HH:MM)", s)
	}
	return hh*60 + mm, nil
}

func parseSpan(s string) (span, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return span{}, fmt.Errorf("want HH:MM-HH:MM, got %q", s)
	}
	x, err := parseClock(a)
	if err != nil {
		return span{}, err
	}
	y, err := parseClock(b)
	if err != nil {
		return span{}, err
	}
	if x == y {
		return span{}, fmt.Errorf("empty range %q", s)
	}
	return span{x, y}, nil
}

func (sp span) contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if sp.Start < sp.End {
		return m >= sp.Start && m < sp.End
	}
	return m >= sp.Start || m < sp.End
}

var weekdays = map[string]time.Weekday{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// parseDays reads "mon-fri", "sat,sun" or "fri-mon" (wraps) into a weekday set.
func parseDays(s string) ([7]bool, error) {
	var set [7]bool
	for _, part := range strings.Split(strings.ToLower(s), ",") {
		a, b, isRange := strings.Cut(strings.TrimSpace(part), "-")
		from, ok1 := weekdays[strings.TrimSpace(a)]
		to, ok2 := from, true
		if isRange {
			to, ok2 = weekdays[strings.TrimSpace(b)]
		}
		if !ok1 || !ok2 {
			return set, fmt.Errorf("bad days %q (want e.g. mon-fri or sat,sun)", s)
		}
		for d := from; ; d = (d + 1) % 7 {
			set[d] = true
			if d == to {
				break
			}
		}
	}
	return set, nil
}

// routingRule overrides the scheduler mode and/or reserve floor for the requests it matches.
// Empty fields match everything; the first matching rule wins.
type routingRule struct {
	Days     string `yaml:"days" json:"days,omitempty"`
	Hours    string `yaml:"hours" json:"hours,omitempty"`
	Provider string `yaml:"provider" json:"provider,omitempty"`
	Model    string `yaml:"model" json:"model,omitempty"`
	Client   string `yaml:"client" json:"client,omitempty"` // User-Agent substring; "!x" matches clients without x
	Mode     string `yaml:"mode" json:"mode,omitempty"`
	Floor    *int   `yaml:"reserve_floor_percent" json:"reserve_floor_percent,omitempty"`
	Active   bool   `yaml:"-" json:"active"` // days/hours match right now (state view only)
	days     *[7]bool
	hours    *span
}

func (r *routingRule) compile() error {
	switch r.Mode {
	case "", modeOff, modeMostRemaining, modeSoonestReset, modeUseFirst:
	default:
		return fmt.Errorf("unknown mode %q", r.Mode)
	}
	if r.Mode == "" && r.Floor == nil {
		return fmt.Errorf("needs mode or reserve_floor_percent")
	}
	if r.Floor != nil {
		f := min(max(*r.Floor, 0), 100)
		r.Floor = &f
	}
	if strings.TrimSpace(r.Days) != "" {
		d, err := parseDays(r.Days)
		if err != nil {
			return err
		}
		r.days = &d
	}
	if strings.TrimSpace(r.Hours) != "" {
		h, err := parseSpan(r.Hours)
		if err != nil {
			return fmt.Errorf("hours: %w", err)
		}
		r.hours = &h
	}
	return nil
}

// activeAt reports whether the rule's days and hours include t.
func (r routingRule) activeAt(t time.Time) bool {
	return (r.days == nil || r.days[t.Weekday()]) && (r.hours == nil || r.hours.contains(t))
}

func (r routingRule) matches(t time.Time, provider, model, ua string) bool {
	if !r.activeAt(t) {
		return false
	}
	if r.Provider != "" && !strings.EqualFold(r.Provider, provider) {
		return false
	}
	if r.Model != "" && !strings.Contains(strings.ToLower(model), strings.ToLower(r.Model)) {
		return false
	}
	if c := strings.ToLower(r.Client); c != "" {
		neg := strings.HasPrefix(c, "!")
		if strings.Contains(strings.ToLower(ua), strings.TrimPrefix(c, "!")) == neg {
			return false
		}
	}
	return true
}

// minLoss: smaller projected losses are noise (same cut-off as the page's "Use first").
const minLoss = 5

// lossOf is the share of a ≥1-day window that would go unused at reset at the average pace so far
// (the remaining share before 10% of the window has passed). Mirrors the page's "Use first".
func lossOf(w window, now int64) float64 {
	if w.Used == nil || w.ResetAt == nil || *w.ResetAt <= now || w.PeriodS < 86400 {
		return 0
	}
	elapsed := 1 - float64(*w.ResetAt-now)/float64(w.PeriodS*1000)
	if elapsed >= 0.1 {
		return clamp(100 - *w.Used/elapsed)
	}
	return clamp(100 - *w.Used)
}
