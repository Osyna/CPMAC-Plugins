package main

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func mustObj(t *testing.T, raw string) obj {
	t.Helper()
	var o obj
	if err := json.Unmarshal([]byte(raw), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func ms(s string) int64 {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

func usedOf(w window) float64 {
	if w.Used == nil {
		return -1
	}
	return *w.Used
}

func TestToMs(t *testing.T) {
	cases := []struct {
		in   any
		want int64 // 0 = nil
	}{
		{"2026-10-30T12:00:00.123456+00:00", ms("2026-10-30T12:00:00.123456Z")},
		{float64(1761800000), 1761800000000},
		{float64(1761800000000), 1761800000000},
		{"1761800000", 1761800000000},
		{float64(0), 0},
		{"not a date", 0},
		{nil, 0},
	}
	for _, c := range cases {
		got := toMs(c.in)
		switch {
		case c.want == 0 && got != nil:
			t.Errorf("toMs(%v) = %d, want nil", c.in, *got)
		case c.want != 0 && (got == nil || *got != c.want):
			t.Errorf("toMs(%v) = %v, want %d", c.in, got, c.want)
		}
	}
}

const claudeUsage = `{
 "five_hour": {"utilization": 6, "resets_at": "2026-10-30T15:00:00.000000+00:00"},
 "seven_day": {"utilization": 41.5, "resets_at": "2026-11-02T15:00:00+00:00"},
 "iguana_necktie": {"utilization": 100, "resets_at": 1762329540, "limit_dollars": 250, "used_dollars": 250.75, "remaining_dollars": 0},
 "limits": [
  {"kind": "weekly_scoped", "percent": 12, "resets_at": "2026-11-02T15:00:00+00:00", "scope": {"model": {"display_name": "Opus"}}},
  {"kind": "weekly", "percent": 41.5},
  {"kind": "weekly_scoped", "percent": 3, "scope": {"model": {}}}
 ],
 "extra_usage": {"is_enabled": false, "monthly_limit": 1000, "used_credits": 1074, "utilization": 100, "currency": "EUR", "decimal_places": 2, "disabled_reason": "org_level_disabled_until", "spend_limit_reached": true}
}`

func TestClaudeWindows(t *testing.T) {
	ws := claudeWindows(mustObj(t, claudeUsage))
	want := []struct {
		key    string
		used   float64
		period int64
	}{{"seven_day", 41.5, periodWeek}, {"five_hour", 6, period5h}, {"scoped:opus", 12, periodWeek}, {"credits", 100, periodWeek}}
	if len(ws) != len(want) {
		t.Fatalf("got %d windows %+v, want %d", len(ws), ws, len(want))
	}
	for i, w := range want {
		if ws[i].Key != w.key || usedOf(ws[i]) != w.used || ws[i].PeriodS != w.period {
			t.Errorf("window %d = %+v, want %+v", i, ws[i], w)
		}
	}
	if ws[0].Label != "7-day limit" || ws[1].Label != "5-hour limit" || ws[2].Label != "7-day Opus" {
		t.Errorf("labels = %q %q %q", ws[0].Label, ws[1].Label, ws[2].Label)
	}
	if r := ws[1].ResetAt; r == nil || *r != ms("2026-10-30T15:00:00Z") {
		t.Errorf("five_hour reset = %v", r)
	}
	if r := ws[3].ResetAt; r == nil || *r != 1762329540000 {
		t.Errorf("credits reset = %v", r)
	}
}

func TestClaudeLegacyFableWindow(t *testing.T) {
	ws := claudeWindows(mustObj(t, `{"iguana_necktie": {"utilization": 30, "resets_at": 1762329540}}`))
	if len(ws) != 1 || ws[0].Key != "scoped:fable" || usedOf(ws[0]) != 30 {
		t.Fatalf("windows = %+v", ws)
	}
	// a scoped limit suppresses the legacy window
	ws = claudeWindows(mustObj(t, `{"iguana_necktie": {"utilization": 30}, "limits": [{"kind":"weekly_scoped","percent":1,"scope":{"model":{"display_name":"Sonnet"}}}]}`))
	if len(ws) != 1 || ws[0].Key != "scoped:sonnet" {
		t.Fatalf("windows = %+v", ws)
	}
}

func TestClaudeSpendAndCredits(t *testing.T) {
	p := mustObj(t, claudeUsage)
	// extra_usage alone (minor units)
	sp := claudeSpend(p)
	want := spend{Currency: "EUR", Used: 10.74, Limit: 10, Percent: 100, Enabled: false, LimitReached: true, DisabledReason: "org_level_disabled_until"}
	if sp == nil || *sp != want {
		t.Fatalf("extra_usage spend = %+v, want %+v", sp, want)
	}
	// the dedicated spend object wins for amounts
	p["spend"] = mustObj(t, `{"used":{"amount_minor":2500,"currency":"EUR","exponent":2},"limit":{"amount_minor":5000,"currency":"EUR","exponent":2},"percent":50,"enabled":true,"disabled_reason":""}`)
	sp = claudeSpend(p)
	if sp == nil || sp.Used != 25 || sp.Limit != 50 || sp.Percent != 50 || !sp.Enabled || !sp.LimitReached {
		// limit_reached still comes from extra_usage.spend_limit_reached
		t.Fatalf("spend object = %+v", sp)
	}
	if claudeSpend(mustObj(t, `{"five_hour":{"utilization":1}}`)) != nil {
		t.Error("spend without extra_usage/spend should be nil")
	}
	c := claudeCredits(mustObj(t, claudeUsage))
	if c == nil || c.Used != 250.75 || c.Limit != 250 || c.Remaining != 0 || c.Currency != "USD" || c.ResetAt == nil || *c.ResetAt != 1762329540000 {
		t.Fatalf("credits = %+v", c)
	}
	if claudeCredits(mustObj(t, `{"iguana_necktie":{"utilization":30}}`)) != nil {
		t.Error("credits without *_dollars should be nil")
	}
}

func TestClaudePlan(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"organization":{"organization_type":"claude_team","subscription_status":"active"}}`, "Team"},
		{`{"organization":{"rate_limit_tier":"default_claude_max_20x"},"account":{"has_claude_max":true}}`, "Max 20x"},
		{`{"account":{"has_claude_max":true}}`, "Max"},
		{`{"account":{"has_claude_max":false,"has_claude_pro":true}}`, "Pro"},
		{`{"account":{"has_claude_max":false,"has_claude_pro":false}}`, "Free"},
		{`{}`, ""},
	}
	for _, c := range cases {
		if got := claudePlan(mustObj(t, c.in)); got != c.want {
			t.Errorf("claudePlan(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeResult(t *testing.T) {
	r, err := claudeResult(mustObj(t, claudeUsage), mustObj(t, `{"account":{"has_claude_pro":true}}`), nil)
	if err != nil || r.plan != "Pro" || r.spend == nil || r.credits == nil {
		t.Fatalf("result = %+v err=%v", r, err)
	}
	if r, _ = claudeResult(mustObj(t, claudeUsage), nil, errors.New("boom")); r.plan != "" {
		t.Errorf("plan with failed profile = %q", r.plan)
	}
	if _, err = claudeResult(mustObj(t, `{}`), nil, nil); err == nil || err.Error() != "No quota windows in usage response" {
		t.Errorf("empty usage err = %v", err)
	}
}

func TestCodexWindows(t *testing.T) {
	now := ms("2026-10-30T12:00:00Z")
	ws := codexWindows(mustObj(t, `{"rate_limit":{"allowed":true,"limit_reached":false,
	  "primary_window":{"used_percent":10,"limit_window_seconds":18000,"reset_at":1761830400},
	  "secondary_window":{"used_percent":55,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`), now)
	if len(ws) != 2 || ws[0].Key != "weekly" || ws[1].Key != "five_hour" {
		t.Fatalf("order = %+v", ws)
	}
	if usedOf(ws[0]) != 55 || ws[0].ResetAt == nil || *ws[0].ResetAt != now+3600*1000 || ws[0].PeriodS != 604800 {
		t.Errorf("weekly = %+v", ws[0])
	}
	if ws[1].PeriodS != 18000 || *ws[1].ResetAt != 1761830400000 {
		t.Errorf("five_hour = %+v", ws[1])
	}

	// limit_reached without used_percent => 100% when a reset time is known
	ws = codexWindows(mustObj(t, `{"rateLimit":{"limitReached":true,
	  "primaryWindow":{"limitWindowSeconds":18000,"resetAt":1761830400},
	  "secondaryWindow":{"limitWindowSeconds":604800}}}`), now)
	if len(ws) != 2 || usedOf(ws[1]) != 100 || ws[0].Used != nil {
		t.Fatalf("blocked windows = %+v", ws)
	}

	// 30-day window is "monthly" with its own period
	ws = codexWindows(mustObj(t, `{"rate_limit":{"primary_window":{"used_percent":3,"limit_window_seconds":2592000}}}`), now)
	if len(ws) != 1 || ws[0].Key != "monthly" || ws[0].PeriodS != 2592000 || ws[0].Label != "Monthly limit" {
		t.Fatalf("monthly = %+v", ws)
	}
	// unknown window length: primary defaults to 5h, secondary to weekly
	ws = codexWindows(mustObj(t, `{"rate_limit":{"primary_window":{"used_percent":1},"secondary_window":{"used_percent":2}}}`), now)
	if len(ws) != 2 || ws[0].Key != "weekly" || ws[1].Key != "five_hour" || ws[1].PeriodS != period5h {
		t.Fatalf("defaults = %+v", ws)
	}
	if len(codexWindows(mustObj(t, `{}`), now)) != 0 {
		t.Error("missing rate_limit should yield no windows")
	}
}

func TestCodexResultPlan(t *testing.T) {
	body := mustObj(t, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":1}}}`)
	if r, err := codexResult(body, "plus", 0); err != nil || r.plan != "Pro" {
		t.Fatalf("plan = %q err=%v", r.plan, err)
	}
	delete(body, "plan_type")
	if r, _ := codexResult(body, "plus", 0); r.plan != "Plus" {
		t.Fatalf("fallback plan = %q", r.plan)
	}
	if _, err := codexResult(mustObj(t, `{}`), "", 0); err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestOpenCodeResult(t *testing.T) {
	r, err := openCodeResult(mustObj(t, `{"usage":{
	  "rolling":{"percent":20,"resetsAt":"2026-10-30T15:00:00Z"},
	  "weekly":{"percent":"35.5","resetsAt":1762329540},
	  "monthly":{"resetsAt":1762329540}}}`))
	if err != nil || r.plan != "Go" || len(r.windows) != 3 {
		t.Fatalf("result = %+v err=%v", r, err)
	}
	w := r.windows
	if w[0].Key != "weekly" || usedOf(w[0]) != 35.5 || w[0].PeriodS != periodWeek ||
		w[1].Key != "rolling" || w[1].Label != "5-hour limit" || w[1].PeriodS != period5h ||
		w[2].Key != "monthly" || w[2].Used != nil || w[2].PeriodS != periodMonth30 {
		t.Fatalf("windows = %+v", w)
	}
	if _, err := openCodeResult(mustObj(t, `{"usage":{}}`)); err == nil {
		t.Fatal("expected error for empty usage")
	}
}

func TestUpstreamMessage(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{mustObj(t, `{"error":{"message":"rate limited","type":"x"}}`), "rate limited"},
		{mustObj(t, `{"error":{"type":"forbidden"}}`), "forbidden"},
		{mustObj(t, `{"detail":"nope"}`), "nope"},
		{"plain text", "plain text"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := upstreamMessage(c.in); got != c.want {
			t.Errorf("upstreamMessage(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if e := (&httpStatusError{403, "x"}); e.Error() != "HTTP 403 · x" {
		t.Errorf("error text = %q", e.Error())
	}
}
