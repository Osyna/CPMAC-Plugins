package main

import (
	"fmt"
	"math"
	"strings"
)

var barLetters = map[string]string{"claude": "C", "codex": "X", "opencode": "O"}

// barOutput is a Waybar custom-module object ({text, tooltip, class, percentage}): per provider the
// best account's plan remaining ("C 92% · X 82% · O 0%"), "!" when an account of it has a problem.
func barOutput(creds []credential, below int, now int64) obj {
	var text, tips []string
	class, lowest := "ok", math.Inf(1)
	worse := func(c string) {
		if c == "critical" || class == "ok" {
			class = c
		}
	}
	for _, p := range []string{"claude", "codex", "opencode"} {
		best, has, trouble := math.Inf(-1), false, false
		for _, c := range creds {
			if c.Provider != p || c.Disabled {
				continue
			}
			has = true
			trouble = trouble || c.Login != "" || c.Status == "error"
			if rem, _, ok := binding(c.Windows); ok {
				best = math.Max(best, rem)
			}
			var parts []string
			for _, w := range c.Windows {
				if w.Key == "credits" || w.Used == nil {
					continue
				}
				part := fmt.Sprintf("%s %.0f%%", shortLabel(w.Label), 100-*w.Used)
				if w.ResetAt != nil && *w.ResetAt > now && *w.Used > 0 {
					part += " (resets " + inDur(*w.ResetAt-now) + ")"
				}
				parts = append(parts, part)
			}
			tip := credLabel(c, creds) + ": " + strings.Join(parts, " · ")
			if c.Login != "" {
				tip += " · login " + c.Login
			} else if c.Status == "error" {
				tip += " · " + c.Error
			}
			tips = append(tips, tip)
		}
		if !has {
			continue
		}
		t := barLetters[p] + " "
		if math.IsInf(best, -1) {
			t += "?"
		} else {
			t += fmt.Sprintf("%.0f%%", best)
			lowest = math.Min(lowest, best)
			switch {
			case best <= 0:
				worse("critical")
			case below > 0 && best < float64(below):
				worse("warning")
			}
		}
		if trouble {
			t += "!"
			worse("warning")
		}
		text = append(text, t)
	}
	pct := 0
	if !math.IsInf(lowest, 1) {
		pct = int(math.Round(lowest))
	}
	return obj{"text": strings.Join(text, " · "), "tooltip": strings.Join(tips, "\n"), "class": class, "percentage": pct}
}

// metricsText renders the Prometheus text exposition format.
func metricsText(v stateView, waste []wasteEvent, now int64) string {
	var b strings.Builder
	family := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	sample := func(name, labels string, val float64) {
		fmt.Fprintf(&b, "%s{%s} %g\n", name, labels, val)
	}
	acct := func(c credential) string {
		return fmt.Sprintf(`provider="%s",account="%s",auth_index="%s"`, esc(c.Provider), esc(c.Name), esc(c.AuthIndex))
	}
	creds := v.Credentials
	family("quota_ledger_remaining_percent", "gauge", "Remaining share of a quota window.")
	for _, c := range creds {
		for _, w := range c.Windows {
			if w.Used != nil {
				sample("quota_ledger_remaining_percent", acct(c)+`,window="`+esc(w.Key)+`"`, 100-*w.Used)
			}
		}
	}
	family("quota_ledger_reset_timestamp_seconds", "gauge", "When a quota window resets (unix seconds).")
	for _, c := range creds {
		for _, w := range c.Windows {
			if w.ResetAt != nil {
				sample("quota_ledger_reset_timestamp_seconds", acct(c)+`,window="`+esc(w.Key)+`"`, float64(*w.ResetAt)/1000)
			}
		}
	}
	family("quota_ledger_projected_loss_percent", "gauge", "Share of a weekly or longer window that would go unused at reset at the pace so far.")
	for _, c := range creds {
		for _, w := range c.Windows {
			if w.Key != "credits" && w.PeriodS >= 86400 && w.Used != nil && w.ResetAt != nil {
				sample("quota_ledger_projected_loss_percent", acct(c)+`,window="`+esc(w.Key)+`"`, lossOf(w, now))
			}
		}
	}
	family("quota_ledger_wasted_percent", "gauge", "Unused share of a window at its last recorded reset.")
	lastWaste := map[string]wasteEvent{}
	for _, w := range waste {
		lastWaste[w.AuthIndex+"|"+w.Window] = w // oldest first, so the latest wins
	}
	for _, c := range creds {
		for _, w := range c.Windows {
			if e, ok := lastWaste[c.AuthIndex+"|"+w.Key]; ok {
				sample("quota_ledger_wasted_percent", acct(c)+`,window="`+esc(w.Key)+`"`, e.Unused)
			}
		}
	}
	flags := []struct {
		name, help string
		val        func(credential) bool
	}{
		{"quota_ledger_up", "1 when the last quota check succeeded.", func(c credential) bool { return c.Status == "ok" }},
		{"quota_ledger_login_ok", "0 when the account needs a new login or key.", func(c credential) bool { return c.Login == "" }},
		{"quota_ledger_rate_limited", "1 while quota checks wait out an upstream 429.", func(c credential) bool { return c.RetryAt > now }},
		{"quota_ledger_disabled", "1 when the account is disabled.", func(c credential) bool { return c.Disabled }},
	}
	for _, f := range flags {
		family(f.name, "gauge", f.help)
		for _, c := range creds {
			sample(f.name, acct(c), bit(f.val(c)))
		}
	}
	family("quota_ledger_requests_24h", "gauge", "Proxied requests in the last 24 h by outcome.")
	family("quota_ledger_latency_avg_seconds_24h", "gauge", "Average proxied request latency in the last 24 h.")
	for _, c := range creds {
		if c.Health == nil {
			continue
		}
		d := c.Health.Day
		for _, o := range []struct {
			name string
			n    int64
		}{{"ok", d.Requests - d.Failed}, {"failed", d.Failed}, {"rate_limited", d.RateLimited}, {"auth_failed", d.AuthFailed}, {"server_error", d.ServerErrors}} {
			sample("quota_ledger_requests_24h", acct(c)+`,outcome="`+o.name+`"`, float64(o.n))
		}
		if d.Timed > 0 {
			sample("quota_ledger_latency_avg_seconds_24h", acct(c), float64(d.LatencyMs)/float64(d.Timed)/1000)
		}
	}
	family("quota_ledger_scheduler_picks_total", "counter", "Requests routed by the quota scheduler since start.")
	fmt.Fprintf(&b, "quota_ledger_scheduler_picks_total %d\n", v.Scheduler.Picks)
	family("quota_ledger_scheduler_fallbacks_total", "counter", "Requests left to the built-in scheduler since start.")
	fmt.Fprintf(&b, "quota_ledger_scheduler_fallbacks_total %d\n", v.Scheduler.Fallbacks)
	return b.String()
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func esc(s string) string { return labelEscaper.Replace(s) }

func bit(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
