package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func cred(idx, authID string, ws ...window) *credential {
	return &credential{ID: "claude:" + idx, AuthIndex: idx, AuthID: authID, Provider: "claude", Name: idx + ".json", Status: "ok", Windows: ws}
}

func win(key string, used float64, reset int64) window {
	w := window{Key: key, Label: key, Used: new(used)}
	if reset != 0 {
		w.ResetAt = new(reset)
	}
	return w
}

func TestBinding(t *testing.T) {
	rem, reset, ok := binding([]window{win("five_hour", 10, 100), win("seven_day", 80, 900), {Key: "unknown"}})
	if !ok || rem != 20 || reset != 900 {
		t.Fatalf("binding = %v %v %v", rem, reset, ok)
	}
	if _, _, ok := binding([]window{{Key: "x"}}); ok {
		t.Fatal("no known usage must not bind")
	}
	// The exhausted dollar pool and a model-scoped window do not bind every request.
	rem, _, _ = binding([]window{win("five_hour", 10, 100), win("credits", 100, 900), win("scoped:fable", 90, 900)})
	if rem != 90 {
		t.Fatalf("credits/scoped must not bind plan quota, rem = %v", rem)
	}
}

func TestPickAuthModelScoped(t *testing.T) {
	creds := []*credential{
		cred("a", "auth-a", win("seven_day", 10, 3000), win("scoped:fable 5", 95, 3000)), // 90% plan, 5% Fable
		cred("b", "auth-b", win("seven_day", 40, 3000)),                                  // 60% plan, no Fable limit
	}
	v := buildView(schedConfig{Mode: modeMostRemaining}, creds, nil, 0)
	if id, _, _, _ := pickAuth(v, pickParams{Model: "claude-sonnet-5-5"}, []string{"auth-a", "auth-b"}); id != "auth-a" {
		t.Errorf("non-Fable model ignores Fable window, got %q", id)
	}
	if id, e, _, _ := pickAuth(v, pickParams{Model: "claude-fable-5-1"}, []string{"auth-a", "auth-b"}); id != "auth-b" || e.Provider != "claude" {
		t.Errorf("Fable model is bound by the Fable window, got %q %+v", id, e)
	}
}

func TestPickAuth(t *testing.T) {
	creds := []*credential{
		cred("a", "auth-a", win("seven_day", 50, 3000)), // 50% left, resets last
		cred("b", "auth-b", win("seven_day", 20, 2000)), // 80% left
		cred("c", "auth-c", win("seven_day", 95, 1000)), // 5% left, resets first
		cred("d", "auth-d"),                             // unknown quota
		cred("e", "auth-e", win("seven_day", 100, 500)), // exhausted
	}
	ids := map[string]string{"learned-b": "b"}
	pick := func(cfg schedConfig, ua string, cands ...string) (string, bool) {
		id, _, _, ok := pickAuth(buildView(cfg, creds, ids, 0), pickParams{UserAgent: ua}, cands)
		return id, ok
	}
	all := []string{"auth-a", "auth-b", "auth-c", "auth-d", "auth-e"}

	if _, ok := pick(schedConfig{Mode: modeOff}, "", all...); ok {
		t.Error("off must be unhandled")
	}
	if id, ok := pick(schedConfig{Mode: modeMostRemaining}, "", all...); !ok || id != "auth-b" {
		t.Errorf("most-remaining = %q %v", id, ok)
	}
	if id, ok := pick(schedConfig{Mode: modeSoonestReset}, "", all...); !ok || id != "auth-c" {
		t.Errorf("soonest-reset = %q %v (exhausted e resets sooner but has nothing left)", id, ok)
	}
	if id, ok := pick(schedConfig{Mode: modeMostRemaining}, "", "learned-b", "auth-d"); !ok || id != "learned-b" {
		t.Errorf("learned id = %q %v", id, ok)
	}
	if _, ok := pick(schedConfig{Mode: modeMostRemaining}, "", "auth-d", "unknown"); ok {
		t.Error("no known quota must be unhandled")
	}

	reserve := schedConfig{Mode: modeMostRemaining, Floor: 30, Exempt: []string{"claude-cli"}}
	if id, ok := pick(reserve, "python-requests/2", "auth-c", "auth-a"); !ok || id != "auth-a" {
		t.Errorf("reserve for non-exempt = %q %v", id, ok)
	}
	if id, ok := pick(reserve, "claude-cli/2.1 (external)", "auth-c", "auth-a"); !ok || id != "auth-a" {
		t.Errorf("exempt still prefers most remaining = %q %v", id, ok)
	}
	reserveSoon := schedConfig{Mode: modeSoonestReset, Floor: 30, Exempt: []string{"claude-cli"}}
	if id, _ := pick(reserveSoon, "claude-cli/2.1", "auth-c", "auth-a"); id != "auth-c" {
		t.Errorf("exempt may use reserved credential = %q", id)
	}
	if id, _ := pick(reserveSoon, "curl/8", "auth-c", "auth-a"); id != "auth-a" {
		t.Errorf("non-exempt must skip reserved credential = %q", id)
	}
	if _, ok := pick(reserve, "curl/8", "auth-c", "auth-e"); ok {
		t.Error("all filtered out must be unhandled")
	}

	// Reserved flags follow the same floor.
	buildView(reserve, creds, ids, 0)
	if got := []bool{creds[0].Reserved, creds[1].Reserved, creds[2].Reserved, creds[3].Reserved, creds[4].Reserved}; got[0] || got[1] || !got[2] || got[3] || !got[4] {
		t.Errorf("reserved flags = %v", got)
	}
	buildView(schedConfig{Mode: modeOff, Floor: 30}, creds, ids, 0)
	if creds[2].Reserved {
		t.Error("scheduler off must clear reserved flags")
	}
}

func TestDiffAlerts(t *testing.T) {
	now := int64(10_000)
	prev := cred("a", "", win("seven_day", 70, 20_000), win("five_hour", 50, 5_000))
	next := func(used7, used5 float64) *credential {
		c := cred("a", "", win("seven_day", used7, 20_000), win("five_hour", used5, 5_000))
		return c
	}
	kinds := func(evs []alertEvent) string {
		s := ""
		for _, e := range evs {
			s += e.Kind + ":" + e.Window + " "
		}
		return s
	}

	if got := kinds(diffAlerts(prev, next(85, 50), 20, now)); got != "below:seven_day " {
		t.Errorf("crossing below = %q", got)
	}
	if got := kinds(diffAlerts(next(85, 50), next(90, 50), 20, now)); got != "" {
		t.Errorf("already below must not repeat = %q", got)
	}
	if got := kinds(diffAlerts(prev, next(70, 20), 20, now)); got != "reset:five_hour " {
		// the 5h window dropped 30 points
		t.Errorf("drop reset = %q", got)
	}
	// reset_at passed and used decreased slightly
	if got := kinds(diffAlerts(prev, next(70, 45), 20, now)); got != "reset:five_hour " {
		t.Errorf("reset_at passed = %q", got)
	}
	if got := kinds(diffAlerts(prev, next(70, 55), 20, now)); got != "" {
		t.Errorf("normal growth = %q", got)
	}
	if got := diffAlerts(prev, next(85, 50), 0, now); len(got) != 0 {
		t.Errorf("threshold 0 disables below: %v", got)
	}

	failed := *prev
	failed.Status, failed.Error = "error", "HTTP 429"
	if evs := diffAlerts(prev, &failed, 20, now); len(evs) != 1 || evs[0].Kind != "error" || evs[0].Message != "quota fetch failed: HTTP 429" {
		t.Errorf("ok->error = %+v", evs)
	}
	if evs := diffAlerts(&failed, prev, 20, now); len(evs) != 1 || evs[0].Kind != "recovered" {
		t.Errorf("error->ok = %+v", evs)
	}
	pending := &credential{Status: "pending"}
	if evs := diffAlerts(pending, prev, 20, now); len(evs) != 0 {
		t.Errorf("first fetch must be silent: %+v", evs)
	}
	e := diffAlerts(prev, next(85, 50), 20, now)[0]
	if e.Remaining == nil || *e.Remaining != 15 || e.Label != "seven_day" || e.AuthIndex != "a" {
		t.Errorf("event fields = %+v", e)
	}
}

func TestWebhookRequest(t *testing.T) {
	e := alertEvent{At: 1, Kind: "below", Name: "n", Message: "m"}
	cases := []struct{ format, ctype, body string }{
		{"discord", "application/json", `{"content":"Quota Ledger · n · m"}`},
		{"slack", "application/json", `{"text":"Quota Ledger · n · m"}`},
		{"ntfy", "text/plain; charset=utf-8", "Quota Ledger · n · m"},
	}
	for _, c := range cases {
		b, ct, err := webhookRequest(c.format, e)
		if err != nil || ct != c.ctype || string(b) != c.body {
			t.Errorf("%s: %q %q %v", c.format, b, ct, err)
		}
	}
	b, _, _ := webhookRequest("json", e)
	var back alertEvent
	if json.Unmarshal(b, &back) != nil || back.Kind != "below" {
		t.Errorf("json body = %s", b)
	}
	if validWebhookURL("ftp://x") == nil || validWebhookURL("https://hooks.example/x") != nil {
		t.Error("webhook url validation")
	}
}

func TestParseSettings(t *testing.T) {
	d, err := parseSettings(nil)
	if err != nil || d.PollMinutes != 5 || d.Mode != modeOff || len(d.exemptList()) != 1 {
		t.Fatalf("defaults = %+v %v", d, err)
	}
	s, err := parseSettings([]byte("enabled: true\npriority: 1\npoll_interval_minutes: 0\nhistory_days: 3\nalert_below_percent: 250\nscheduler_mode: bogus\nwebhook_format: slack\nmanagement_url: http://x:1/\nreserve_exempt_user_agents: ' a, b ,,'\nreserve_floor_percent: 10\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.PollMinutes != 1 || s.HistoryDays != 3 || s.AlertBelow != 100 || s.Mode != modeOff || s.WebhookFormat != "slack" ||
		s.ManagementURL != "http://x:1" || len(s.exemptList()) != 2 || s.ReserveFloor != 10 {
		t.Errorf("settings = %+v", s)
	}
	s, _ = parseSettings([]byte("reserve_exempt_user_agents: [claude-cli, codex]\nscheduler_mode: soonest-reset\n"))
	if got := s.exemptList(); len(got) != 2 || got[1] != "codex" || s.Mode != modeSoonestReset {
		t.Errorf("list form = %+v", s)
	}
	if _, err := parseSettings([]byte("poll_interval_minutes: [")); err == nil {
		t.Error("broken YAML must error")
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]string{"": "", "short": "••••", "sk-test-demo-key-0001": "sk-te…0001"}
	for in, want := range cases {
		if got := maskKey(in); got != want {
			t.Errorf("maskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestStoreRoundTripAndPrune(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	now := int64(30) * 24 * hourMs
	st, err := openStore(dir, 14, now)
	if err != nil {
		t.Fatal(err)
	}
	st.addHistory([]histPoint{{now - 20*24*hourMs, "a", "seven_day", 1}, {now - 2*hourMs, "a", "seven_day", 40}, {now - hourMs, "a", "five_hour", 10}})
	st.addAlerts([]alertEvent{{At: now - 20*24*hourMs, Kind: "reset"}, {At: now - hourMs, Kind: "below"}})
	st.learn("auth-1", "a")
	st.addUsage(now-now%hourMs, "a", "sk-te…0001", "claude-opus-5-5", usageRow{Requests: 1, InputTokens: 10, OutputTokens: 20, CacheReadTokens: 3, CacheCreationTokens: 4})
	st.addUsage(now-now%hourMs, "a", "sk-te…0001", "claude-opus-5-5", usageRow{Requests: 1, Failed: 1, RateLimited: 1, InputTokens: 1, OutputTokens: 2})
	st.addUsage(now-30*24*hourMs, "a", "old", "m", usageRow{Requests: 1, InputTokens: 1, OutputTokens: 1})
	st.flush()

	re, err := openStore(dir, 14, now) // reload prunes the 20-day-old rows
	if err != nil {
		t.Fatal(err)
	}
	ser := re.series(0, "")
	if len(ser) != 2 || ser[0].Key != "five_hour" || len(ser[1].Points) != 1 || ser[1].Points[0] != [2]float64{float64(now - 2*hourMs), 40} {
		t.Errorf("series = %+v", ser)
	}
	if ev := re.alertsSince(0); len(ev) != 1 || ev[0].Kind != "below" {
		t.Errorf("alerts = %+v", ev)
	}
	rows := re.usageRows(now - 24*hourMs)
	want := usageRow{AuthIndex: "a", ClientKey: "sk-te…0001", Model: "claude-opus-5-5", Requests: 2, Failed: 1, RateLimited: 1, InputTokens: 11, OutputTokens: 22, CacheReadTokens: 3, CacheCreationTokens: 4}
	if len(rows) != 1 || rows[0] != want {
		t.Errorf("usage rows = %+v, want %+v", rows, want)
	}
	if re.index("auth-1") != "a" {
		t.Error("learned id lost")
	}
	// the on-disk history was rewritten without the pruned point, and no temp files remain
	raw, _ := os.ReadFile(filepath.Join(dir, historyLog))
	wantHist := fmt.Sprintf("[%d,\"a\",\"seven_day\",40]\n[%d,\"a\",\"five_hour\",10]\n", now-2*hourMs, now-hourMs)
	if string(raw) != wantHist {
		t.Errorf("history file = %q", raw)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if got := re.series(now-90*60*1000, "a"); len(got) != 1 || got[0].Key != "five_hour" {
		t.Errorf("series window filter = %+v", got)
	}
}

func TestAlertRingBuffer(t *testing.T) {
	st, _ := openStore("", 14, 0)
	for i := range maxAlerts + 5 {
		st.addAlerts([]alertEvent{{At: int64(i + 1)}})
	}
	ev := st.alertsSince(0)
	if len(ev) != maxAlerts || ev[0].At != 6 || ev[len(ev)-1].At != maxAlerts+5 {
		t.Errorf("ring = %d events first=%d last=%d", len(ev), ev[0].At, ev[len(ev)-1].At)
	}
	if got := st.alertsSince(int64(maxAlerts)); len(got) != 5 {
		t.Errorf("since filter = %d", len(got))
	}
}

// After a restart the last successful fetch is shown until a new fetch lands, so an upstream 429
// right after startup keeps the last known quota instead of emptying the card.
func TestLastGoodSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	st, _ := openStore(dir, 14, 0)
	ok := cred("a", "", win("seven_day", 7, 9_000))
	ok.FetchedAt = 5_000
	st.saveLastGood(*ok)

	s := newService()
	s.store, _ = openStore(dir, 14, 0)
	s.mergeLocked([]*credential{cred("a", ""), cred("b", "")}, 5_500)
	a, b := s.creds[0], s.creds[1]
	if a.Status != "ok" || a.FetchedAt != 5_000 || len(a.Windows) != 1 || usedOf(a.Windows[0]) != 7 {
		t.Fatalf("seeded = %+v", a)
	}
	if b.Status != "ok" || len(b.Windows) != 0 { // cred() builds Status ok; no snapshot leaves it untouched
		t.Fatalf("unseeded = %+v", b)
	}
	s.apply(s.cfg, a.ID, fetchResult{}, fmt.Errorf("HTTP 429"), 6_000)
	if a.Status != "error" || len(a.Windows) != 1 {
		t.Fatalf("after 429 = %+v", a)
	}
}
