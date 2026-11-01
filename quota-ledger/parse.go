package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Parsers for the three upstream usage payloads. They are 1:1 ports of the v0.1 page's
// claudeWindows/claudePlan/codexWindows/fetchOpenCode and work on decoded JSON (any).

type obj = map[string]any

type window struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Used    *float64 `json:"used"`
	ResetAt *int64   `json:"reset_at"`
	PeriodS int64    `json:"period_s"`
}

type spend struct {
	Currency       string  `json:"currency"`
	Used           float64 `json:"used"`
	Limit          float64 `json:"limit"`
	Percent        float64 `json:"percent"`
	Enabled        bool    `json:"enabled"`
	LimitReached   bool    `json:"limit_reached"`
	DisabledReason string  `json:"disabled_reason"`
}

type credits struct {
	Currency  string  `json:"currency"`
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Remaining float64 `json:"remaining"`
	ResetAt   *int64  `json:"reset_at"`
}

type fetchResult struct {
	windows []window
	plan    string
	spend   *spend
	credits *credits
}

const (
	period5h      = 18000
	periodWeek    = 604800
	periodMonth30 = 2592000
)

func asObj(v any) obj { m, _ := v.(obj); return m }

// nn is JS `a ?? b`: the first key whose value is present and not null.
func nn(m obj, keys ...string) any {
	for _, k := range keys {
		if v := m[k]; v != nil {
			return v
		}
	}
	return nil
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case string:
		return x != ""
	}
	return true
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

// num mirrors the page's num(): finite numbers and numeric strings, otherwise nil.
func num(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case string:
		t := strings.TrimSpace(x)
		if t == "" {
			return nil
		}
		p, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil
		}
		f = p
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func clamp(f float64) float64 { return math.Max(0, math.Min(100, f)) }

var plainNumber = regexp.MustCompile(`^\d+(\.\d+)?$`)

// toMs mirrors the page's toMs(): ISO strings, or unix seconds/milliseconds; result in epoch ms.
func toMs(v any) *int64 {
	if s, ok := v.(string); ok {
		if t := strings.TrimSpace(s); t != "" && !plainNumber.MatchString(t) {
			ts, err := time.Parse(time.RFC3339Nano, t)
			if err != nil {
				return nil
			}
			return new(ts.UnixMilli())
		}
	}
	n := num(v)
	if n == nil || *n <= 0 {
		return nil
	}
	ms := int64(*n)
	if *n < 1e11 { // unix seconds vs milliseconds
		ms = int64(*n * 1000)
	}
	return &ms
}

func usedWindow(key, label string, used *float64, reset any, period int64) window {
	w := window{Key: key, Label: label, ResetAt: toMs(reset), PeriodS: period}
	if used != nil {
		w.Used = new(clamp(*used))
	}
	return w
}

// Order matters: the page uses the first window as the summary headline, so the plan-wide
// 7-day limit leads; model-scoped windows (e.g. Fable) and the credit pool follow.
func claudeWindows(p obj) []window {
	var out, scopedWins []window
	limits, _ := p["limits"].([]any)
	for _, x := range limits {
		l := asObj(x)
		name := strings.TrimSpace(str(asObj(asObj(l["scope"])["model"])["display_name"]))
		pct := num(l["percent"])
		if str(l["kind"]) != "weekly_scoped" || name == "" || pct == nil {
			continue
		}
		scopedWins = append(scopedWins, usedWindow("scoped:"+strings.ToLower(name), "7-day "+name, pct, l["resets_at"], periodWeek))
	}
	scoped := len(scopedWins)
	for _, n := range []struct {
		key, label string
		period     int64
	}{
		{"seven_day", "7-day limit", periodWeek},
		{"five_hour", "5-hour limit", period5h},
		{"seven_day_opus", "7-day Opus", periodWeek},
		{"seven_day_sonnet", "7-day Sonnet", periodWeek},
		{"seven_day_oauth_apps", "7-day OAuth apps", periodWeek},
		{"seven_day_cowork", "7-day Cowork", periodWeek},
	} {
		w := asObj(p[n.key])
		if u := num(w["utilization"]); u != nil {
			out = append(out, usedWindow(n.key, n.label, u, w["resets_at"], n.period))
		}
	}
	out = append(out, scopedWins...)
	// iguana_necktie is a dollar credit pool when it carries *_dollars, otherwise the legacy Fable window.
	pool := asObj(p["iguana_necktie"])
	if u := num(pool["utilization"]); u != nil {
		if num(pool["limit_dollars"]) != nil {
			out = append(out, usedWindow("credits", "Session credits", u, pool["resets_at"], periodWeek))
		} else if scoped == 0 {
			out = append(out, usedWindow("scoped:fable", "7-day Fable", u, pool["resets_at"], periodWeek))
		}
	}
	return out
}

func claudePlan(profile obj) string {
	if profile == nil {
		return ""
	}
	org, acc := asObj(profile["organization"]), asObj(profile["account"])
	if str(org["organization_type"]) == "claude_team" && str(org["subscription_status"]) == "active" {
		return "Team"
	}
	tier := regexp.MustCompile(`max_(\d+x)`).FindStringSubmatch(str(org["rate_limit_tier"]))
	switch {
	case truthy(acc["has_claude_max"]):
		if tier != nil {
			return "Max " + tier[1]
		}
		return "Max"
	case truthy(acc["has_claude_pro"]):
		return "Pro"
	case acc["has_claude_max"] == false && acc["has_claude_pro"] == false:
		return "Free"
	}
	return ""
}

// money converts {amount_minor, currency, exponent} to a major-unit amount.
func money(v any) (amount float64, currency string, ok bool) {
	m := asObj(v)
	minor := num(m["amount_minor"])
	if minor == nil {
		return 0, "", false
	}
	exp := 2.0
	if e := num(m["exponent"]); e != nil {
		exp = *e
	}
	return *minor / math.Pow(10, exp), str(m["currency"]), true
}

// claudeSpend reads the monthly extra-usage spend from `spend` (preferred) or `extra_usage` (minor units).
func claudeSpend(p obj) *spend {
	sp, ex := asObj(p["spend"]), asObj(p["extra_usage"])
	var out spend
	var haveUsed, haveLimit bool
	if sp != nil {
		var c1, c2 string
		out.Used, c1, haveUsed = money(sp["used"])
		out.Limit, c2, haveLimit = money(sp["limit"])
		out.Currency = c1
		if out.Currency == "" {
			out.Currency = c2
		}
		if pc := num(sp["percent"]); pc != nil {
			out.Percent = *pc
		}
		out.Enabled = truthy(sp["enabled"])
		out.DisabledReason = str(sp["disabled_reason"])
	}
	if !haveUsed && !haveLimit && ex != nil {
		scale := math.Pow(10, 2)
		if d := num(ex["decimal_places"]); d != nil {
			scale = math.Pow(10, *d)
		}
		if u := num(ex["used_credits"]); u != nil {
			out.Used, haveUsed = *u/scale, true
		}
		if l := num(ex["monthly_limit"]); l != nil {
			out.Limit, haveLimit = *l/scale, true
		}
		out.Currency = str(ex["currency"])
		if pc := num(ex["utilization"]); pc != nil {
			out.Percent = *pc
		}
		out.Enabled = truthy(ex["is_enabled"])
		out.DisabledReason = str(ex["disabled_reason"])
	}
	if !haveUsed && !haveLimit {
		return nil
	}
	if ex != nil {
		out.LimitReached = truthy(ex["spend_limit_reached"])
		if out.DisabledReason == "" {
			out.DisabledReason = str(ex["disabled_reason"])
		}
	}
	if !out.LimitReached && out.Limit > 0 && out.Used >= out.Limit {
		out.LimitReached = true
	}
	return &out
}

// claudeCredits reads the dollar credit pool (iguana_necktie) when it carries *_dollars.
func claudeCredits(p obj) *credits {
	pool := asObj(p["iguana_necktie"])
	limit := num(pool["limit_dollars"])
	if limit == nil {
		return nil
	}
	c := credits{Currency: "USD", Limit: *limit, ResetAt: toMs(pool["resets_at"])}
	switch used, rem, util := num(pool["used_dollars"]), num(pool["remaining_dollars"]), num(pool["utilization"]); {
	case used != nil:
		c.Used = *used
	case rem != nil:
		c.Used = *limit - *rem
	case util != nil:
		c.Used = *limit * *util / 100
	}
	c.Remaining = math.Max(0, c.Limit-c.Used)
	if rem := num(pool["remaining_dollars"]); rem != nil {
		c.Remaining = *rem
	}
	return &c
}

func claudeResult(usage, profile any, profileErr error) (fetchResult, error) {
	u := asObj(usage)
	ws := claudeWindows(u)
	if len(ws) == 0 {
		return fetchResult{}, errors.New("No quota windows in usage response")
	}
	r := fetchResult{windows: ws, spend: claudeSpend(u), credits: claudeCredits(u)}
	if profileErr == nil {
		r.plan = claudePlan(asObj(profile))
	}
	return r, nil
}

// codexWindows parses wham/usage; now is only used for reset_after_seconds.
func codexWindows(p obj, now int64) []window {
	var out []window
	rl := asObj(nn(p, "rate_limit", "rateLimit"))
	if rl == nil {
		return out
	}
	blocked := truthy(nn(rl, "limit_reached", "limitReached")) || rl["allowed"] == false
	for i, raw := range []any{nn(rl, "primary_window", "primaryWindow"), nn(rl, "secondary_window", "secondaryWindow")} {
		w := asObj(raw)
		if w == nil {
			continue
		}
		secs := num(nn(w, "limit_window_seconds", "limitWindowSeconds"))
		var key, label string
		var period int64
		switch {
		case (secs != nil && *secs == 18000) || (secs == nil && i == 0):
			key, label, period = "five_hour", "5-hour limit", period5h
		case secs != nil && *secs >= 28*86400 && *secs <= 31*86400:
			key, label, period = "monthly", "Monthly limit", int64(*secs)
		default:
			key, label, period = "weekly", "Weekly limit", periodWeek
			if secs != nil {
				period = int64(*secs)
			}
		}
		reset := toMs(nn(w, "reset_at", "resetAt"))
		if after := num(nn(w, "reset_after_seconds", "resetAfterSeconds")); reset == nil && after != nil && *after > 0 {
			reset = new(now + int64(*after*1000))
		}
		cw := window{Key: key, Label: label, ResetAt: reset, PeriodS: period}
		used := num(nn(w, "used_percent", "usedPercent"))
		if used == nil && blocked && reset != nil {
			used = new(100.0)
		}
		if used != nil {
			cw.Used = new(clamp(*used))
		}
		out = append(out, cw)
	}
	// Weekly first: it is the binding limit for subscription planning.
	sort.SliceStable(out, func(a, b int) bool { return out[a].Key != "five_hour" && out[b].Key == "five_hour" })
	return out
}

func codexResult(body any, credPlan string, now int64) (fetchResult, error) {
	b := asObj(body)
	ws := codexWindows(b, now)
	if len(ws) == 0 {
		return fetchResult{}, errors.New("No rate-limit windows in usage response")
	}
	plan := str(nn(b, "plan_type", "planType"))
	if plan == "" {
		plan = credPlan
	}
	if plan != "" {
		plan = strings.ToUpper(plan[:1]) + plan[1:]
	}
	return fetchResult{windows: ws, plan: plan}, nil
}

func openCodeResult(body any) (fetchResult, error) {
	usage := asObj(asObj(body)["usage"])
	var ws []window
	for _, k := range []struct {
		key, label string
		period     int64
	}{
		{"weekly", "Weekly limit", periodWeek},
		{"rolling", "5-hour limit", period5h},
		{"monthly", "Monthly limit", periodMonth30},
	} {
		u := asObj(usage[k.key])
		if u == nil {
			continue
		}
		ws = append(ws, usedWindow(k.key, k.label, num(u["percent"]), u["resetsAt"], k.period))
	}
	if len(ws) == 0 {
		return fetchResult{}, errors.New("No usage windows in response")
	}
	return fetchResult{windows: ws, plan: "Go"}, nil
}

// httpStatusError is a non-2xx upstream answer relayed through the management api-call.
type httpStatusError struct {
	status int
	msg    string
}

func (e *httpStatusError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return fmt.Sprintf("HTTP %d · %s", e.status, e.msg)
}

// upstreamMessage is the page's error-message extraction for failed api-call bodies.
func upstreamMessage(body any) string {
	if m := asObj(body); m != nil {
		e := asObj(m["error"])
		for _, v := range []any{e["message"], e["type"], m["detail"], m["message"]} {
			if truthy(v) {
				return str(v)
			}
		}
		return ""
	}
	if s, ok := body.(string); ok {
		if len(s) > 160 {
			s = s[:160]
		}
		return s
	}
	return ""
}
