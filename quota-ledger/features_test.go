package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const day = 24 * hourMs

func TestResetWaste(t *testing.T) {
	prev := cred("a", "", win("seven_day", 60, 10_000), win("five_hour", 20, 50_000), win("credits", 90, 10_000))
	prev.Windows[0].PeriodS = 604800
	prev.FetchedAt = 9_000
	cur := cred("a", "", win("seven_day", 1, 10_000+7*day), win("five_hour", 25, 50_000), win("credits", 0, 10_000+7*day))
	got := resetWaste(prev, cur, 12_000)
	if len(got) != 1 || got[0].Window != "seven_day" || got[0].Unused != 40 || got[0].At != 10_000 || got[0].SeenAt != 9_000 || got[0].PeriodS != 604800 {
		t.Fatalf("waste = %+v", got)
	}
	// A reset time that only jitters (Claude reports ms differences) is the same cycle.
	cur.Windows[0].ResetAt = new(int64(10_400))
	if got := resetWaste(prev, cur, 12_000); len(got) != 0 {
		t.Errorf("jitter counted as a reset: %+v", got)
	}
	// Checked after the reset already: that check belongs to the new cycle.
	prev.FetchedAt = 11_000
	cur.Windows[0].ResetAt = new(int64(10_000 + 7*day))
	if got := resetWaste(prev, cur, 12_000); len(got) != 0 {
		t.Errorf("post-reset check counted: %+v", got)
	}
}

func TestRunOut(t *testing.T) {
	now := 10 * day
	w := win("seven_day", 50, now+3*day)
	w.PeriodS = 604800
	// 5 %/h over the last 4 h: 50% left runs out in 10 h, well before the reset in 3 d.
	var pts []histPoint
	for i := int64(0); i <= 4; i++ {
		pts = append(pts, histPoint{now - (4-i)*hourMs, "a", "seven_day", 30 + 5*float64(i)})
	}
	if at := runOut(pts, w, now); at < now+9*hourMs || at > now+11*hourMs {
		t.Errorf("run-out at %+.1f h, want ~10 h", float64(at-now)/float64(hourMs))
	}
	w.ResetAt = new(now + 8*hourMs) // resets before it would run out
	if at := runOut(pts, w, now); at != 0 {
		t.Errorf("run-out after reset reported: %d", at)
	}
	w.ResetAt = new(now + 3*day)
	if at := runOut(pts[3:], w, now); at != 0 {
		t.Errorf("two points must not project: %d", at)
	}
	// Flat history (live OpenCode monthly data): the fit's float noise (slope ~2.6e-13 %/h) overflowed int64
	// and became "hits 0% now".
	var flat []histPoint
	for _, ts := range []int64{1793347051385, 1793347352260, 1793347652659, 1793347669464, 1793347762231, 1793347831045, 1793347890224, 1793347989653,
		1793348074126, 1793348374554, 1793348544063, 1793348927965, 1793349228393, 1793349529002, 1793349829470, 1793350129903, 1793350430403,
		1793350819746, 1793351120189, 1793351420666, 1793351721127, 1793351951362, 1793352273482, 1793352573828, 1793352874299, 1793353174721,
		1793353475161, 1793353775593, 1793354076009, 1793354376440, 1793354676964, 1793354977880, 1793355279185, 1793355574325, 1793356014895,
		1793356315234, 1793356615685, 1793356916105, 1793357216580, 1793357517246, 1793357817612, 1793358120897, 1793358292064} {
		flat = append(flat, histPoint{ts, "o", "monthly", 76})
	}
	m := win("monthly", 76, 1793358292064+25*day)
	m.PeriodS = 30 * 86400
	if at := runOut(flat, m, 1793358292064); at != 0 {
		t.Errorf("flat history projected a run-out %d ms from now", at-1793358292064)
	}

	c := cred("a", "", w)
	st, _ := openStore("", 14, 0)
	seen := func(key string, cycle int64) bool { return !st.fireOnce(key, cycle) } // as service.apply does
	pointsOf := func(string, int64) []histPoint { return pts }
	if ev := runOutAlerts(c, now, pointsOf, seen); len(ev) != 1 || ev[0].Kind != "runout" || !strings.Contains(ev[0].Message, "hits 0% in 10 h") {
		t.Fatalf("alerts = %+v", ev)
	}
	if ev := runOutAlerts(c, now+hourMs, pointsOf, seen); len(ev) != 0 {
		t.Errorf("second alert in the same cycle: %+v", ev)
	}
}

func TestFireOnceSurvivesDismiss(t *testing.T) {
	st, _ := openStore(t.TempDir(), 14, 0)
	if !st.fireOnce("runout|a|seven_day", 100) || st.fireOnce("runout|a|seven_day", 100) {
		t.Fatal("fireOnce must fire once per cycle")
	}
	st.addAlerts([]alertEvent{{At: 1, Kind: "runout", AuthIndex: "a", Window: "seven_day"}, {At: 2, Kind: "below", AuthIndex: "b"}})
	if n := st.dismissAlerts(false, alertEvent{At: 1, Kind: "runout", AuthIndex: "a", Window: "seven_day"}); n != 1 {
		t.Fatalf("dismissed %d", n)
	}
	if st.fireOnce("runout|a|seven_day", 100) {
		t.Error("dismissing the alert re-armed it")
	}
	if !st.fireOnce("runout|a|seven_day", 200) {
		t.Error("next cycle must fire")
	}
	if n := st.dismissAlerts(true, alertEvent{}); n != 1 || len(st.alertsSince(0)) != 0 {
		t.Errorf("dismiss all removed %d", n)
	}
}

func TestForgetRemovedAccounts(t *testing.T) {
	st, _ := openStore(t.TempDir(), 14, 0)
	st.addAlerts([]alertEvent{{At: 1, AuthIndex: "gone"}, {At: 2, AuthIndex: "kept"}, {At: 3, Kind: "digest"}})
	st.saveLastGood(credential{ID: "opencode:gone"})
	st.saveLastGood(credential{ID: "claude:kept"})
	st.setMuted("gone", true)
	st.forget(map[string]bool{"kept": true})
	if ev := st.alertsSince(0); len(ev) != 2 || ev[0].AuthIndex != "kept" || ev[1].Kind != "digest" {
		t.Errorf("alerts = %+v", ev)
	}
	if _, ok := st.lastGood("opencode:gone"); ok {
		t.Error("snapshot of removed account kept")
	}
	if _, ok := st.lastGood("claude:kept"); !ok {
		t.Error("snapshot of live account dropped")
	}
	if st.muted()["gone"] {
		t.Error("mute of removed account kept")
	}
}

func TestSpansDaysAndRules(t *testing.T) {
	night, err := parseSpan("22:00-07:00")
	if err != nil {
		t.Fatal(err)
	}
	at := func(wd time.Weekday, h, m int) time.Time { return time.Date(2030, 1, 6+int(wd), h, m, 0, 0, time.UTC) } // 2030-01-06 is a Sunday
	for _, c := range []struct {
		t    time.Time
		want bool
	}{{at(1, 23, 0), true}, {at(1, 6, 59), true}, {at(1, 7, 0), false}, {at(1, 12, 0), false}} {
		if night.contains(c.t) != c.want {
			t.Errorf("22-07 contains %s = %v", c.t.Format("15:04"), !c.want)
		}
	}
	for _, bad := range []string{"9", "25:00-07:00", "08:00-08:00", "08:61-09:00"} {
		if _, err := parseSpan(bad); err == nil {
			t.Errorf("parseSpan(%q) accepted", bad)
		}
	}
	wk, _ := parseDays("fri-mon")
	if wk != [7]bool{true, true, false, false, false, true, true} {
		t.Errorf("fri-mon = %v", wk)
	}

	s, err := parseSettings([]byte(`
scheduler_mode: most-remaining
quiet_hours: "nope"
routing_rules:
  - {days: mon-fri, hours: "09:00-18:00", provider: claude, client: "!claude-cli", reserve_floor_percent: 60}
  - {model: fable, mode: use-first}
  - {mode: sideways}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rules) != 2 || len(s.problems) != 2 || s.quiet != nil {
		t.Fatalf("rules=%d problems=%v quiet=%v", len(s.Rules), s.problems, s.quiet)
	}
	cfg := s.sched()
	work := at(2, 10, 0)
	if mode, floor, exempt, rule := cfg.policy(pickParams{At: work, Provider: "claude", UserAgent: "python-requests"}); mode != modeMostRemaining || floor != 60 || exempt || rule != 0 {
		t.Errorf("weekday script = %s %v %v %d", mode, floor, exempt, rule)
	}
	if _, floor, _, rule := cfg.policy(pickParams{At: work, Provider: "claude", UserAgent: "claude-cli/2.1"}); floor != 0 || rule != -1 {
		t.Errorf("claude-cli must not match the !claude-cli rule: floor %v rule %d", floor, rule)
	}
	if _, _, _, rule := cfg.policy(pickParams{At: at(6, 10, 0), Provider: "claude"}); rule != -1 {
		t.Errorf("saturday matched rule %d", rule)
	}
	if mode, _, _, rule := cfg.policy(pickParams{At: work, Provider: "codex", Model: "claude-fable-5"}); mode != modeUseFirst || rule != 1 {
		t.Errorf("fable rule = %s %d", mode, rule)
	}
}

func TestUseFirstAndLiveAvoid(t *testing.T) {
	now := 100 * day
	weekly := func(used float64, resetIn int64) window {
		w := win("seven_day", used, now+resetIn)
		w.PeriodS = 604800
		return w
	}
	creds := []*credential{
		cred("a", "auth-a", weekly(10, 6*day)),  // 1 d gone, 10% used: ~30% lost in 6 d
		cred("b", "auth-b", weekly(20, 2*day)),  // 5 d gone, 20% used: ~72% lost in 2 d
		cred("c", "auth-c", weekly(95, 1*day)),  // nearly empty, nothing lost
		cred("d", "auth-d", weekly(5, 6*day+1)), // most remaining
	}
	all := []string{"auth-a", "auth-b", "auth-c", "auth-d"}
	pick := func(mode string) string {
		id, _, _, _ := pickAuth(buildView(schedConfig{Mode: mode}, creds, nil, now), pickParams{At: time.UnixMilli(now)}, all)
		return id
	}
	if got := pick(modeUseFirst); got != "auth-b" {
		t.Errorf("use-first = %s, want the soonest window that would lose quota", got)
	}
	if got := pick(modeMostRemaining); got != "auth-d" {
		t.Errorf("most-remaining = %s", got)
	}
	// A live 429 passes the account over for liveAvoid; an expired login until it recovers.
	creds[1].live429At = now - 60_000
	if got := pick(modeUseFirst); got != "auth-a" {
		t.Errorf("after live 429 = %s, want auth-a", got)
	}
	creds[1].live429At = now - liveAvoid - 1
	creds[0].Login, creds[1].Login, creds[2].Login, creds[3].Login = "expired", "expired", "expired", "expired"
	if got := pick(modeUseFirst); got != "auth-b" {
		t.Errorf("all failing must still pick (never reject), got %q", got)
	}
}

func TestLoginState(t *testing.T) {
	c := cred("a", "")
	if ev := updateLogin(c, 1); len(ev) != 0 || c.Login != "" {
		t.Fatalf("healthy = %+v %q", ev, c.Login)
	}
	c.liveAuthAt, c.lastFail = 10, "HTTP 401: invalid token"
	if ev := updateLogin(c, 11); len(ev) != 1 || ev[0].Kind != "login" || c.Login != "expired" || !strings.Contains(c.LoginReason, "401") {
		t.Fatalf("after live 401 = %+v %q %q", ev, c.Login, c.LoginReason)
	}
	c.liveOKAt = 20 // a later success clears it
	if ev := updateLogin(c, 21); len(ev) != 1 || ev[0].Kind != "recovered" || c.Login != "" {
		t.Fatalf("after success = %+v %q", ev, c.Login)
	}
	c.AuthStatus, c.AuthMessage = "error", "refresh token revoked"
	if updateLogin(c, 30); c.Login != "expired" {
		t.Errorf("host auth error = %q", c.Login)
	}
	c.AuthMessage = "upstream 502 bad gateway"
	if updateLogin(c, 31); c.Login != "" {
		t.Errorf("non-auth host error flagged as login: %q", c.LoginReason)
	}
	k := &credential{Provider: "opencode", FetchAuth: 401}
	if updateLogin(k, 1); k.Login != "rejected" {
		t.Errorf("opencode = %q", k.Login)
	}
}

func TestDigestDue(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	mon9 := time.Date(2030, 1, 7, 9, 0, 0, 0, loc) // a Monday
	cases := []struct {
		now  time.Time
		last int64
		want bool
	}{
		{mon9.Add(-time.Minute), 0, false},                                   // before the slot
		{mon9.Add(time.Minute), 0, true},                                     // just after
		{mon9.Add(time.Hour), mon9.Add(time.Minute).UnixMilli(), false},      // already sent
		{mon9.Add(30 * time.Hour), 0, false},                                 // missed by more than a day
		{mon9.AddDate(0, 0, 7).Add(time.Minute), mon9.UnixMilli() + 1, true}, // next week
	}
	for _, c := range cases {
		if got := digestDue("mon", 9, c.last, c.now); got != c.want {
			t.Errorf("digestDue(%s, last %d) = %v", c.now, c.last, got)
		}
	}
	if digestDue("off", 9, 0, mon9.Add(time.Minute)) {
		t.Error("off must never be due")
	}
}

func TestDigestAndBar(t *testing.T) {
	now := 100 * day
	w := win("seven_day", 8, now+6*day)
	w.Label, w.PeriodS = "7-day limit", 604800
	creds := []credential{
		{Provider: "claude", Plan: "Max 20x", Name: "c.json", Status: "ok", Windows: []window{w}},
		{Provider: "codex", Plan: "Pro", Name: "x.json", Status: "error", Error: "HTTP 500", Windows: []window{win("weekly", 100, now+day)}},
	}
	waste := []wasteEvent{{At: now - day, Provider: "codex", Label: "Weekly limit", PeriodS: 604800, Unused: 38}}
	rows := []usageRow{{ClientKey: "sk-te…0001", Model: "claude-fable-5", Requests: 7, Failed: 1}}
	text := buildDigest(creds, waste, rows, now)
	for _, want := range []string{"Claude (Max 20x): 7-day 92% left, resets in 6 d", "Lost at reset this week: Codex Weekly 38%", "Use first: 1. Claude (Max 20x) 7-day", "7 requests, 1 failed", "Codex (Pro): HTTP 500"} {
		if !strings.Contains(text, want) {
			t.Errorf("digest lacks %q:\n%s", want, text)
		}
	}
	bar := barOutput(creds, 20, now)
	if bar["text"] != "C 92% · X 0%!" || bar["class"] != "critical" || bar["percentage"] != 0 {
		t.Errorf("bar = %+v", bar)
	}
}

// Muted accounts and quiet hours keep alerts on the page but off the webhook.
func TestRaiseMuteAndQuietHours(t *testing.T) {
	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer hook.Close()
	s := newService()
	st, _ := openStore("", 14, 0)
	st.setMuted("m", true)
	cfg, _ := parseSettings([]byte("webhook_url: " + hook.URL))
	s.raise(cfg, st, []alertEvent{{At: 1, AuthIndex: "m", Message: "muted"}, {At: 2, AuthIndex: "u", Message: "sent"}})
	all := time.Now().Add(-time.Hour).Format("15:04") + "-" + time.Now().Add(time.Hour).Format("15:04")
	quiet, _ := parseSettings([]byte("webhook_url: " + hook.URL + "\nquiet_hours: \"" + all + "\""))
	s.raise(quiet, st, []alertEvent{{At: 3, AuthIndex: "u", Message: "quiet"}})
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 1 {
		t.Errorf("webhook hits = %d, want 1 (muted and quiet ones held back)", hits.Load())
	}
	if ev := st.alertsSince(0); len(ev) != 3 || !ev[0].Muted || ev[1].Muted {
		t.Errorf("recorded = %+v", ev)
	}
}
