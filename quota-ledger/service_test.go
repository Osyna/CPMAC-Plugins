package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeHost mimics the loopback management API: auth-files, config and api-call.
type fakeHost struct {
	t         *testing.T
	mu        sync.Mutex
	claude    string // current /oauth/usage body
	claude429 bool   // answer /oauth/usage with 429
	inFlight  atomic.Int32
	peak      atomic.Int32
	calls     []string
}

func (f *fakeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer secret" {
		http.Error(w, "no", http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/v0/management/auth-files":
		io.WriteString(w, `{"files":[
		 {"provider":"claude","id":"claude-1.json","auth_index":"c1","name":"claude-1.json","email":"a@b.co"},
		 {"provider":"codex","id":"codex-1.json","auth_index":"x1","name":"codex-1.json","id_token":{"chatgpt_account_id":"acct","plan_type":"plus"}},
		 {"provider":"claude","id":"claude-off.json","auth_index":"c2","name":"off.json","disabled":true},
		 {"provider":"gemini","id":"g.json","auth_index":"g1"}]}`)
	case "/v8/management/config":
		io.WriteString(w, `{"api-keys":{"openai-compatibility":[
		 {"name":"opencode-go","base-url":"https://opencode.ai/zen/go/v1","keys":[{"api-key":"test-opencode-key-1234","auth_index":"o1"}]},
		 {"name":"other","base-url":"https://example.com","keys":[{"api-key":"k","auth_index":"z"}]}]}}`)
	case "/v0/management/api-call":
		cur := f.inFlight.Add(1)
		defer f.inFlight.Add(-1)
		for {
			p := f.peak.Load()
			if cur <= p || f.peak.CompareAndSwap(p, cur) {
				break
			}
		}
		var in struct {
			AuthIndex string            `json:"auth_index"`
			URL       string            `json:"url"`
			Header    map[string]string `json:"header"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.calls = append(f.calls, in.AuthIndex+" "+in.URL+" "+in.Header["Chatgpt-Account-Id"])
		claude, limited := f.claude, f.claude429
		f.mu.Unlock()
		status, body := 200, ""
		switch in.URL {
		case "https://api.anthropic.com/api/oauth/usage":
			body = claude
			if limited {
				status, body = 429, `{"error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`
			}
		case "https://api.anthropic.com/api/oauth/profile":
			body = `{"account":{"has_claude_max":true},"organization":{"rate_limit_tier":"default_claude_max_5x"}}`
		case "https://chatgpt.com/backend-api/wham/usage":
			body = `{"plan_type":"plus","rate_limit":{"limit_reached":true,"primary_window":{"limit_window_seconds":18000,"reset_at":1893456000}}}`
		case "https://opencode.ai/zen/go/v1/usage":
			status, body = 403, `{"error":{"message":"forbidden"}}`
		}
		json.NewEncoder(w).Encode(map[string]any{"status_code": status, "body": body})
	default:
		http.NotFound(w, r)
	}
}

func claudeBody(five, seven float64) string {
	return fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":"2030-01-01T05:00:00Z"},"seven_day":{"utilization":%v,"resets_at":"2030-01-07T00:00:00Z"}}`, five, seven)
}

func callRoute(t *testing.T, s *service, method, route, query, body string) (int, map[string]any) {
	t.Helper()
	q := map[string][]string{}
	for _, kv := range strings.Split(query, "&") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			q[k] = []string{v}
		}
	}
	resp := s.handleManagement(managementRequest{Method: method, Path: "/v0/management/quota-ledger/" + route, Query: q, Body: []byte(body)})
	var out map[string]any
	if err := json.Unmarshal(resp["Body"].([]byte), &out); err != nil {
		t.Fatalf("%s: %v", route, err)
	}
	return resp["StatusCode"].(int), out
}

func TestPollerEndToEnd(t *testing.T) {
	host := &fakeHost{t: t, claude: claudeBody(30, 40)}
	srv := httptest.NewServer(host)
	defer srv.Close()
	var hook atomic.Value
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hook.Store(string(b))
	}))
	defer hookSrv.Close()

	s := newService()
	s.cfg = defaultSettings()
	s.cfg.ManagementURL, s.cfg.AlertBelow = srv.URL, 40
	s.cfg.WebhookURL, s.cfg.WebhookFormat = hookSrv.URL, "slack"
	s.cfg.Mode, s.cfg.ReserveFloor = modeMostRemaining, 50
	ctx := context.Background()

	// No password: the poller keeps running and reports it.
	t.Setenv("MANAGEMENT_PASSWORD", "")
	s.poll(ctx, "", 0)
	if _, st := callRoute(t, s, "GET", "state", "", ""); st["poller"].(map[string]any)["error"] != passwordMissing {
		t.Fatalf("poller error = %v", st["poller"])
	}

	t.Setenv("MANAGEMENT_PASSWORD", "secret")
	if err := s.poll(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.poll(ctx, "nope", 0); err != errUnknownAuthIndex {
		t.Fatalf("unknown index err = %v", err)
	}
	if p := host.peak.Load(); p > upstreamCalls {
		t.Errorf("upstream concurrency peak %d > %d", p, upstreamCalls)
	}

	code, st := callRoute(t, s, "GET", "state", "", "")
	if code != 200 || st["version"] != version || st["poller"].(map[string]any)["error"] != "" {
		t.Fatalf("state = %d %v", code, st)
	}
	byID := map[string]map[string]any{}
	for _, c := range st["credentials"].([]any) {
		m := c.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if len(byID) != 4 { // claude c1, c2 (disabled), codex x1, opencode o1; gemini and the other group are ignored
		t.Fatalf("credentials = %v", byID)
	}
	c1 := byID["claude:c1"]
	if c1["status"] != "ok" || c1["plan"] != "Max 5x" || c1["auth_id"] != "claude-1.json" || len(c1["windows"].([]any)) != 2 || c1["reserved"] != false {
		t.Errorf("claude c1 = %v", c1) // binding window: 7-day at 60% remaining, above the 50% floor
	}
	if byID["claude:c2"]["status"] != "disabled" || byID["codex:x1"]["status"] != "ok" || byID["codex:x1"]["plan"] != "Plus" {
		t.Errorf("c2/x1 = %v / %v", byID["claude:c2"], byID["codex:x1"])
	}
	if o := byID["opencode:o1"]; o["status"] != "error" || o["error"] != "No OpenCode Go subscription on this key" || o["name"] != "opencode-go · test-…1234" {
		t.Errorf("opencode = %v", o)
	}
	host.mu.Lock()
	sawAccount := false
	for _, c := range host.calls {
		sawAccount = sawAccount || strings.HasSuffix(c, "wham/usage acct")
	}
	host.mu.Unlock()
	if !sawAccount {
		t.Errorf("codex call lacked Chatgpt-Account-Id: %v", host.calls)
	}

	// History got one point per window with a known usage.
	_, h := callRoute(t, s, "GET", "history", "hours=1&auth_index=c1", "")
	if series := h["series"].([]any); len(series) != 2 {
		t.Errorf("history = %v", h)
	}

	// Second poll: 7-day crosses under 40% remaining (used 40 -> 70), 5-hour resets (used 30 -> 2).
	host.mu.Lock()
	host.claude = claudeBody(2, 70)
	host.mu.Unlock()
	if err := s.poll(ctx, "c1", 0); err != nil {
		t.Fatal(err)
	}
	_, al := callRoute(t, s, "GET", "alerts", "since=0", "")
	kinds := map[string]bool{}
	for _, e := range al["events"].([]any) {
		m := e.(map[string]any)
		kinds[m["kind"].(string)+":"+m["window"].(string)] = true
	}
	if !kinds["below:seven_day"] || !kinds["reset:five_hour"] || len(kinds) != 2 {
		t.Errorf("alert kinds = %v", kinds)
	}
	if code, ok := callRoute(t, s, "POST", "alerts/test", "", ""); code != 200 || ok["ok"] != true {
		t.Errorf("alerts/test = %d %v", code, ok)
	}
	if got, _ := hook.Load().(string); !strings.Contains(got, `"text"`) {
		t.Errorf("slack webhook body = %q", got)
	}

	// Error recovery path: a failing usage call keeps the last windows and flags the error.
	host.mu.Lock()
	host.claude = `{}`
	host.mu.Unlock()
	s.poll(ctx, "c1", 0)
	_, st = callRoute(t, s, "GET", "state", "", "")
	for _, c := range st["credentials"].([]any) {
		if m := c.(map[string]any); m["id"] == "claude:c1" && (m["status"] != "error" || len(m["windows"].([]any)) != 2) {
			t.Errorf("after failure = %v", m)
		}
	}

	// 429: back off. Neither the scheduled loop nor a manual refresh calls the upstream until retry_at,
	// and the plan comes from the one profile read of the first poll.
	countCalls := func(path string) (n int) {
		host.mu.Lock()
		defer host.mu.Unlock()
		for _, c := range host.calls {
			if strings.Contains(c, path) {
				n++
			}
		}
		return n
	}
	host.mu.Lock()
	host.claude429 = true
	host.mu.Unlock()
	s.poll(ctx, "c1", 0)
	before := countCalls("/oauth/usage")
	s.poll(ctx, "", 0)
	callRoute(t, s, "POST", "refresh", "", `{"auth_index":"c1"}`)
	if after := countCalls("/oauth/usage"); after != before {
		t.Errorf("usage calls during backoff: %d -> %d", before, after)
	}
	_, st = callRoute(t, s, "GET", "state", "", "")
	for _, c := range st["credentials"].([]any) {
		if m := c.(map[string]any); m["id"] == "claude:c1" {
			if ra, _ := m["retry_at"].(float64); int64(ra) < nowMs()+14*60_000 || len(m["windows"].([]any)) != 2 {
				t.Errorf("rate limited c1 = %v", m)
			}
		}
	}
	if n := countCalls("/oauth/profile"); n != 1 {
		t.Errorf("profile calls = %d, want 1", n)
	}

	// Scheduler: c1 (30% left, below the 50% floor) is reserved for non-exempt clients, x1 is blocked (100% used).
	_, st = callRoute(t, s, "GET", "state", "", "")
	for _, c := range st["credentials"].([]any) {
		if m := c.(map[string]any); m["id"] == "claude:c1" && m["reserved"] != true {
			t.Errorf("c1 should be reserved: %v", m)
		}
	}
	pick := func(ua string) bool {
		req := fmt.Sprintf(`{"Provider":"claude","Model":"m","Options":{"Headers":{"User-Agent":[%q]}},"Candidates":[{"ID":"claude-1.json"},{"ID":"codex-1.json"}]}`, ua)
		return s.pick([]byte(req))["Handled"] == true
	}
	if pick("curl/8") {
		t.Error("reserve should leave the request to the host")
	}
	if !pick("claude-cli/2.1") {
		t.Error("exempt client should be scheduled")
	}
	_, st = callRoute(t, s, "GET", "state", "", "")
	sc := st["scheduler"].(map[string]any)
	if sc["picks"] != float64(1) || sc["fallbacks"] != float64(1) || sc["last_pick"].(map[string]any)["auth_index"] != "c1" {
		t.Errorf("scheduler = %v", sc)
	}

	// Usage attribution: masked keys only, AuthID learned for the OpenCode credential.
	s.usage([]byte(`{"Model":"gpt-x","APIKey":"sk-client-secret-9999","AuthID":"opencode-auth","AuthIndex":"o1","Detail":{"InputTokens":5,"OutputTokens":7}}`))
	_, u := callRoute(t, s, "GET", "usage", "hours=1", "")
	rows := u["rows"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["client_key"] != "sk-cl…9999" || rows[0].(map[string]any)["input_tokens"] != float64(5) {
		t.Errorf("usage rows = %v", rows)
	}
	if raw, _ := json.Marshal(u); strings.Contains(string(raw), "secret") {
		t.Error("raw client key leaked")
	}
	_, st = callRoute(t, s, "GET", "state", "", "")
	for _, c := range st["credentials"].([]any) {
		if m := c.(map[string]any); m["id"] == "opencode:o1" && m["auth_id"] != "opencode-auth" {
			t.Errorf("learned auth_id missing: %v", m)
		}
	}

	if code, _ := callRoute(t, s, "GET", "refresh", "", ""); code != 405 {
		t.Errorf("GET refresh = %d", code)
	}
	if code, _ := callRoute(t, s, "POST", "refresh", "", `{"auth_index":"nope"}`); code != 404 {
		t.Errorf("unknown refresh = %d", code)
	}
	if code, body := callRoute(t, s, "GET", "bogus", "", ""); code != 404 || body["error"] == nil {
		t.Errorf("bogus = %d %v", code, body)
	}
}

func TestHandleMethodRegister(t *testing.T) {
	raw, err := handleMethod("management.register", nil)
	if err != nil || !strings.Contains(string(raw), `"quota-ledger/alerts/test"`) || !strings.Contains(string(raw), `"/ledger"`) {
		t.Fatalf("management.register = %s %v", raw, err)
	}
	// plugin.register with the host's lifecycle request; the data dir is redirected to a temp dir.
	yaml := "data_dir: " + t.TempDir() + "\nenabled: true\n"
	req, _ := json.Marshal(map[string]any{"config_yaml": []byte(yaml), "schema_version": 8})
	raw, err = handleMethod("plugin.register", req)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.stop()
	var env struct {
		OK     bool
		Result struct {
			Metadata struct {
				Name, Version string
				ConfigFields  []configField
			}
			Capabilities map[string]bool
		}
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("envelope = %s %v", raw, err)
	}
	r := env.Result
	if len(r.Metadata.ConfigFields) == 0 || !r.Capabilities["scheduler"] || !r.Capabilities["usage_plugin"] || !r.Capabilities["management_api"] {
		t.Errorf("registration = %+v", r)
	}
}
