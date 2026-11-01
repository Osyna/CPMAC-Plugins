package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ---- wasted quota at reset ----

type wasteEvent struct {
	At        int64   `json:"at"` // the reset
	AuthIndex string  `json:"auth_index"`
	Provider  string  `json:"provider"`
	Name      string  `json:"name"`
	Window    string  `json:"window"`
	Label     string  `json:"label"`
	PeriodS   int64   `json:"period_s"`
	Unused    float64 `json:"unused"`  // % of the window left at the last check before the reset
	SeenAt    int64   `json:"seen_at"` // that check
}

// resetWaste records, for each window whose reset passed between two checks, how much was still
// unused at the last check before it. Usage between that check and the reset is not seen, so the
// figure can be a little high (by at most one poll interval of use).
func resetWaste(prev, cur *credential, now int64) []wasteEvent {
	if prev.FetchedAt == 0 || cur.Status != "ok" {
		return nil
	}
	after := make(map[string]window, len(cur.Windows))
	for _, w := range cur.Windows {
		after[w.Key] = w
	}
	var out []wasteEvent
	for _, p := range prev.Windows {
		w, ok := after[p.Key]
		if !ok || p.Key == "credits" || p.Used == nil || p.ResetAt == nil || *p.ResetAt > now || prev.FetchedAt >= *p.ResetAt {
			continue
		}
		if w.ResetAt != nil && *w.ResetAt <= *p.ResetAt+60_000 { // same cycle (reset times jitter by ms)
			continue
		}
		out = append(out, wasteEvent{At: *p.ResetAt, AuthIndex: cur.AuthIndex, Provider: cur.Provider, Name: cur.Name,
			Window: p.Key, Label: p.Label, PeriodS: p.PeriodS, Unused: clamp(100 - *p.Used), SeenAt: prev.FetchedAt})
	}
	return out
}

// ---- run-out projection ----

// runOut projects when a window reaches 100% used from its history in the current cycle (least squares
// over the last 24 h). It returns 0 unless that happens at least 30 min before the reset.
func runOut(pts []histPoint, w window, now int64) int64 {
	if w.Used == nil || *w.Used >= 100 || *w.Used < 10 || w.ResetAt == nil || *w.ResetAt <= now || w.PeriodS <= 0 {
		return 0
	}
	period := w.PeriodS * 1000
	from := max(now-24*hourMs, *w.ResetAt-period)
	var n, st, su, stt, stu float64
	var first, last int64
	for _, p := range pts {
		if p.T < from || p.T > now {
			continue
		}
		if n == 0 {
			first = p.T
		}
		last = p.T
		t := float64(p.T-from) / float64(hourMs)
		n++
		st += t
		su += p.Used
		stt += t * t
		stu += t * p.Used
	}
	if n < 3 || last-first < min(2*hourMs, period/4) {
		return 0
	}
	slope := (n*stu - st*su) / (n*stt - st*st) // percent per hour
	if slope <= 0 {
		return 0
	}
	// Compare in hours before converting: a flat history fits a ~1e-15 slope whose run-out overflows int64.
	hours := (100 - *w.Used) / slope
	if hours >= float64(*w.ResetAt-30*60_000-now)/float64(hourMs) {
		return 0
	}
	return now + int64(hours*float64(hourMs))
}

// inDur renders a future offset compactly: "in 40 min", "in 9 h", "in 6 d".
func inDur(ms int64) string {
	switch m := ms / 60_000; {
	case m < 1:
		return "now"
	case m < 60:
		return fmt.Sprintf("in %d min", m)
	case m < 48*60:
		return fmt.Sprintf("in %d h", (m+30)/60)
	default:
		return fmt.Sprintf("in %d d", (m+12*60)/(24*60))
	}
}

// ---- login health ----

var authProblem = regexp.MustCompile(`(?i)\b40[13]\b|unauthori[sz]ed|invalid_grant|token (has )?expired|revoked|refresh token`)

// loginState decides whether an account needs a new login (OAuth) or a new key (OpenCode), from the
// last quota check, live traffic (a 401/403 newer than the last success) and the host's auth status.
func loginState(c *credential) (state, reason string) {
	bad := "expired"
	if c.Provider == "opencode" {
		bad = "rejected"
	}
	switch {
	case c.Disabled:
	case c.FetchAuth != 0:
		return bad, fmt.Sprintf("quota check answered HTTP %d", c.FetchAuth)
	case c.liveAuthAt > c.liveOKAt:
		return bad, "requests answered " + firstNonEmpty(c.lastFail, "HTTP 401/403")
	case c.AuthStatus == "error" && authProblem.MatchString(c.AuthMessage):
		return bad, "server: " + c.AuthMessage
	}
	return "", ""
}

// updateLogin recomputes c's login state and returns the alert for a change, if any.
func updateLogin(c *credential, now int64) []alertEvent {
	prev := c.Login
	c.Login, c.LoginReason = loginState(c)
	if prev == c.Login {
		return nil
	}
	e := alertEvent{At: now, AuthIndex: c.AuthIndex, Name: c.Name, Provider: c.Provider, Kind: "login"}
	switch {
	case c.Login == "":
		e.Kind, e.Message = "recovered", "login works again"
	case c.Login == "rejected":
		e.Message = "API key rejected: " + c.LoginReason
	default:
		e.Message = "login expired, sign in again: " + c.LoginReason
	}
	return []alertEvent{e}
}

// ---- weekly digest ----

var providerNames = map[string]string{"claude": "Claude", "codex": "Codex", "opencode": "OpenCode"}

// credLabel names an account for messages: "Claude (Max 20x)", plus the account name when a provider has several.
func credLabel(c credential, creds []credential) string {
	n := 0
	for _, x := range creds {
		if x.Provider == c.Provider && !x.Disabled {
			n++
		}
	}
	s := firstNonEmpty(providerNames[c.Provider], c.Provider)
	if c.Plan != "" {
		s += " (" + c.Plan + ")"
	}
	if n > 1 {
		s += " " + c.Name
	}
	return s
}

func shortLabel(label string) string { return strings.TrimSuffix(label, " limit") }

// digestDue reports whether this week's slot (day at hour, local time) has passed and was not sent yet.
// A slot missed by more than a day (server down) is skipped rather than sent late.
func digestDue(day string, hour int, last int64, now time.Time) bool {
	wd, ok := weekdays[day]
	if !ok {
		return false
	}
	slot := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	slot = slot.AddDate(0, 0, -((int(now.Weekday()) - int(wd) + 7) % 7))
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -7)
	}
	return last < slot.UnixMilli() && now.Sub(slot) < 24*time.Hour
}

// buildDigest summarises the week: quota left, quota lost at reset, what to use first, traffic, problems.
func buildDigest(creds []credential, waste []wasteEvent, rows []usageRow, now int64) string {
	lines := []string{"Quota Ledger · weekly digest"}
	type urgent struct {
		label string
		loss  float64
		reset int64
	}
	var first []urgent
	var issues []string
	for _, c := range creds {
		if c.Disabled {
			continue
		}
		name := credLabel(c, creds)
		var parts []string
		for _, w := range c.Windows {
			if w.Key == "credits" || w.Used == nil {
				continue
			}
			p := fmt.Sprintf("%s %.0f%% left", shortLabel(w.Label), 100-*w.Used)
			if w.ResetAt != nil && *w.ResetAt > now && *w.Used > 0 {
				p += ", resets " + inDur(*w.ResetAt-now)
			}
			parts = append(parts, p)
		}
		if len(parts) == 0 {
			parts = []string{"no quota data"}
		}
		lines = append(lines, name+": "+strings.Join(parts, " · "))
		if loss, reset := urgentLoss(c.Windows, now); loss > 0 {
			for _, w := range c.Windows {
				if w.ResetAt != nil && *w.ResetAt == reset && planWindow(w) {
					first = append(first, urgent{name + " " + shortLabel(w.Label), loss, reset})
					break
				}
			}
		}
		switch {
		case c.Login != "":
			issues = append(issues, fmt.Sprintf("%s: login %s (%s)", name, c.Login, c.LoginReason))
		case c.Status == "error":
			issues = append(issues, fmt.Sprintf("%s: %s", name, c.Error))
		}
	}
	var lost []string
	for _, w := range waste {
		if w.At >= now-7*24*hourMs && w.PeriodS >= 86400 {
			lost = append(lost, fmt.Sprintf("%s %s %.0f%%", firstNonEmpty(providerNames[w.Provider], w.Provider), shortLabel(w.Label), w.Unused))
		}
	}
	if len(lost) == 0 {
		lost = []string{"no weekly or monthly reset recorded"}
	}
	lines = append(lines, "Lost at reset this week: "+strings.Join(lost, " · "))
	if len(first) > 0 {
		sort.Slice(first, func(i, j int) bool { return first[i].reset < first[j].reset })
		var parts []string
		for i, u := range first {
			parts = append(parts, fmt.Sprintf("%d. %s (~%.0f%% would be lost, resets %s)", i+1, u.label, u.loss, inDur(u.reset-now)))
		}
		lines = append(lines, "Use first: "+strings.Join(parts, " · "))
	}
	var total, failed int64
	keys, models := map[string]int64{}, map[string]int64{}
	for _, r := range rows {
		total += r.Requests
		failed += r.Failed
		keys[firstNonEmpty(r.ClientKey, "no key")] += r.Requests
		models[firstNonEmpty(r.Model, "unknown")] += r.Requests
	}
	if total > 0 {
		lines = append(lines, fmt.Sprintf("Traffic (7 d): %d requests, %d failed · keys: %s · models: %s", total, failed, top(keys, 3), top(models, 3)))
	}
	if len(issues) > 0 {
		lines = append(lines, "Needs attention: "+strings.Join(issues, " · "))
	}
	return strings.Join(lines, "\n")
}

// top lists the n largest entries as "name (count)", largest first.
func top(m map[string]int64, n int) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		return m[names[i]] > m[names[j]] || m[names[i]] == m[names[j]] && names[i] < names[j]
	})
	parts := make([]string, 0, n)
	for _, k := range names[:min(n, len(names))] {
		parts = append(parts, fmt.Sprintf("%s (%d)", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
