package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	firstPollDelay  = 10 * time.Second
	retryPollDelay  = 15 * time.Second
	rateLimitWait   = 15 * time.Minute // first wait after a 429; doubles per repeat up to 8x (2 h)
	manualMinAge    = time.Minute      // a manual refresh skips accounts checked more recently than this
	profileEvery    = 12 * time.Hour   // the Claude plan rarely changes; each profile read costs a rate-limited call
	flushEvery      = time.Minute
	pruneEvery      = 24 * time.Hour
	refreshTimeout  = 28 * time.Second
	maxUpstreamBody = 8 << 20
	upstreamCalls   = 4
	passwordMissing = "MANAGEMENT_PASSWORD is not set"
)

var errUnknownAuthIndex = errors.New("unknown auth_index")

type pollerInfo struct {
	IntervalS  int64  `json:"interval_s"`
	LastRunAt  int64  `json:"last_run_at"`
	NextRunAt  int64  `json:"next_run_at"`
	Running    bool   `json:"running"`
	Error      string `json:"error"`
	WebhookErr string `json:"webhook_error,omitempty"`
}

type pickInfo struct {
	At        int64  `json:"at"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	AuthIndex string `json:"auth_index"`
	Reason    string `json:"reason"`
}

type service struct {
	mu       sync.RWMutex // guards everything below
	cfg      settings
	creds    []*credential
	poller   pollerInfo
	everOK   bool
	store    *store
	storeErr string
	cancel   context.CancelFunc
	wg       sync.WaitGroup

	pollSem  chan struct{} // one poll at a time
	callSem  chan struct{} // ≤4 concurrent upstream calls
	view     atomic.Pointer[schedView]
	lastPick atomic.Pointer[pickInfo]
	picks    atomic.Int64
	fallback atomic.Int64
	client   *http.Client
}

func newService() *service {
	return &service{
		cfg:     defaultSettings(),
		store:   &store{usage: map[usageKey]*usageRow{}, ids: map[string]string{}},
		pollSem: make(chan struct{}, 1),
		callSem: make(chan struct{}, upstreamCalls),
		client:  &http.Client{Timeout: 25 * time.Second},
	}
}

var svc = newService()

func nowMs() int64 { return time.Now().UnixMilli() }

// configure applies new settings and (re)starts the background loops.
func (s *service) configure(cfg settings) {
	s.stop()
	st, err := openStore(cfg.DataDir, cfg.HistoryDays, nowMs())
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg, s.store = cfg, st
	s.storeErr = ""
	if err != nil {
		s.storeErr = err.Error() + " (history is memory-only)"
	}
	s.poller.IntervalS = int64(cfg.PollMinutes) * 60
	s.rebuildViewLocked()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(2)
	go s.pollLoop(ctx, cfg)
	go s.maintainLoop(ctx, cfg, st)
}

// stop ends the background loops and flushes pending usage to disk.
func (s *service) stop() {
	s.mu.Lock()
	cancel, st := s.cancel, s.store
	s.cancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	st.flush()
}

func (s *service) pollLoop(ctx context.Context, cfg settings) {
	defer s.wg.Done()
	interval := time.Duration(cfg.PollMinutes) * time.Minute
	delay := firstPollDelay
	for {
		s.mu.Lock()
		s.poller.NextRunAt = nowMs() + delay.Milliseconds()
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		s.poll(ctx, "", interval/2) // fresh data (e.g. restored after a restart) is not refetched
		delay = interval
		s.mu.RLock()
		failing := !s.everOK && s.poller.Error != ""
		s.mu.RUnlock()
		if failing { // the HTTP server may not be listening yet, or the password is missing: retry soon
			delay = min(interval, retryPollDelay)
		}
	}
}

func (s *service) maintainLoop(ctx context.Context, cfg settings, st *store) {
	defer s.wg.Done()
	flush, prune := time.NewTicker(flushEvery), time.NewTicker(pruneEvery)
	defer flush.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			st.flush()
			if cfg.WebhookURL != "" && digestDue(cfg.DigestDay, cfg.DigestHour, st.digestAt(), time.Now()) {
				st.setDigestAt(nowMs()) // also on failure: the error shows on the page, no retry every minute
				s.sendDigest(ctx, cfg)
			}
		case <-prune.C:
			st.prune(cfg.HistoryDays, nowMs())
		}
	}
}

func (s *service) rebuildViewLocked() {
	s.view.Store(buildView(s.cfg.sched(), s.creds, s.store.idMap(), nowMs()))
}

// raise records alerts and sends them to the webhook unless the account is muted or it is quiet hours.
func (s *service) raise(cfg settings, st *store, evs []alertEvent) {
	if len(evs) == 0 {
		return
	}
	muted := st.muted()
	for i := range evs {
		evs[i].Muted = muted[evs[i].AuthIndex]
	}
	st.addAlerts(evs)
	if cfg.quiet != nil && cfg.quiet.contains(time.Now()) {
		return
	}
	for _, e := range evs {
		if !e.Muted {
			s.notifyAsync(cfg, e)
		}
	}
}

// digestText builds the weekly digest from the current state, last week's resets and traffic.
func (s *service) digestText() string {
	v := s.state()
	s.mu.RLock()
	st := s.store
	s.mu.RUnlock()
	now := nowMs()
	return buildDigest(v.Credentials, st.wasteSince(now-7*24*hourMs), st.usageRows(now-7*24*hourMs), now)
}

func (s *service) sendDigest(ctx context.Context, cfg settings) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	err := s.sendWebhook(ctx, cfg, alertEvent{At: nowMs(), Kind: "digest", Name: "weekly digest", Message: s.digestText()})
	s.mu.Lock()
	s.poller.WebhookErr = ""
	if err != nil {
		s.poller.WebhookErr = err.Error()
	}
	s.mu.Unlock()
	return err
}

// ---- polling ----

func (s *service) setPollError(msg string) {
	s.mu.Lock()
	s.poller.Error = msg
	s.mu.Unlock()
}

// poll refreshes every credential, or only the one with auth index `only`. Accounts checked within
// minAge, or still in a rate-limit backoff, are skipped.
func (s *service) poll(ctx context.Context, only string, minAge time.Duration) error {
	select {
	case s.pollSem <- struct{}{}:
		defer func() { <-s.pollSem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	cfg := s.cfg
	s.poller.Running = true
	s.mu.Unlock()
	pollErr, err := s.pollOnce(ctx, cfg, only, minAge)
	s.mu.Lock()
	s.poller.Running = false
	s.poller.LastRunAt = nowMs()
	s.poller.Error = pollErr
	if pollErr == "" && err == nil {
		s.everOK = true
	}
	s.mu.Unlock()
	return err
}

func (s *service) pollOnce(ctx context.Context, cfg settings, only string, minAge time.Duration) (string, error) {
	pw := os.Getenv("MANAGEMENT_PASSWORD")
	if pw == "" {
		return passwordMissing, nil
	}
	found, err := s.discover(ctx, cfg, pw)
	if err != nil {
		return err.Error(), nil
	}
	if only != "" {
		match := false
		for _, c := range found {
			match = match || c.AuthIndex == only
		}
		if !match {
			return "", errUnknownAuthIndex
		}
	}
	s.mu.Lock()
	targets, events := s.mergeLocked(found, nowMs())
	st := s.store
	s.mu.Unlock()
	known := make(map[string]bool, len(found))
	for _, c := range found {
		known[c.AuthIndex] = true
	}
	st.forget(known)
	s.raise(cfg, st, events)
	var wg sync.WaitGroup
	start := nowMs()
	for _, c := range targets {
		if only != "" && c.AuthIndex != only || c.RetryAt > start || start-c.FetchedAt < minAge.Milliseconds() {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			now := nowMs()
			res, err := s.fetchCred(ctx, cfg, pw, c, now)
			s.apply(cfg, c.ID, res, err, now)
		}()
	}
	wg.Wait()
	return "", nil
}

// mergeLocked replaces the credential list with the discovered one, keeping fetched data of known
// credentials, and returns a copy of the enabled ones to fetch plus login-change alerts.
func (s *service) mergeLocked(found []*credential, now int64) ([]credential, []alertEvent) {
	old := make(map[string]*credential, len(s.creds))
	for _, c := range s.creds {
		old[c.ID] = c
	}
	var targets []credential
	var events []alertEvent
	for _, f := range found {
		o := old[f.ID]
		if o != nil {
			f.Plan = firstNonEmpty(o.Plan, f.Plan)
			f.Status, f.Error, f.FetchedAt, f.Windows, f.Spend, f.Credits = o.Status, o.Error, o.FetchedAt, o.Windows, o.Spend, o.Credits
			f.RetryAt, f.strikes, f.profileAt = o.RetryAt, o.strikes, o.profileAt
			f.Login, f.LoginReason, f.FetchAuth = o.Login, o.LoginReason, o.FetchAuth
			f.liveAuthAt, f.liveOKAt, f.live429At, f.live429Sent, f.lastFail, f.lastFailAt = o.liveAuthAt, o.liveOKAt, o.live429At, o.live429Sent, o.lastFail, o.lastFailAt
		} else if s.store != nil {
			if g, ok := s.store.lastGood(f.ID); ok {
				// First sight since start: show the persisted snapshot (its fetched_at shows its age) and
				// keep a pending rate-limit backoff, so restarts do not hammer the upstream.
				f.Plan = firstNonEmpty(g.Plan, f.Plan)
				f.Status, f.Error, f.FetchedAt, f.Windows, f.Spend, f.Credits = g.Status, g.Error, g.FetchedAt, g.Windows, g.Spend, g.Credits
				f.RetryAt, f.profileAt, f.FetchAuth = g.RetryAt, g.FetchedAt, g.FetchAuth
			}
		}
		switch {
		case f.Disabled:
			f.Status = "disabled"
		case f.Status == "" || f.Status == "disabled":
			f.Status = "pending"
		}
		if ev := updateLogin(f, now); o != nil { // first sight (e.g. after a restart) sets the state silently
			events = append(events, ev...)
		}
		if !f.Disabled {
			targets = append(targets, *f)
		}
	}
	s.creds = found
	s.rebuildViewLocked()
	return targets, events
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// apply stores one fetch outcome, then records history, alerts and webhooks.
func (s *service) apply(cfg settings, id string, res fetchResult, fetchErr error, now int64) {
	s.mu.Lock()
	var c *credential
	for _, x := range s.creds {
		if x.ID == id {
			c = x
		}
	}
	if c == nil {
		s.mu.Unlock()
		return
	}
	prev := *c
	if fetchErr == nil {
		c.Status, c.Error, c.FetchedAt = "ok", "", now
		c.Windows, c.Spend, c.Credits = res.windows, res.spend, res.credits
		c.Plan = firstNonEmpty(res.plan, c.Plan)
		if res.plan != "" {
			c.profileAt = now
		}
	} else {
		c.Status, c.Error = "error", fetchErr.Error()
	}
	var hs *httpStatusError
	isHTTP := errors.As(fetchErr, &hs)
	if isHTTP && hs.status == http.StatusTooManyRequests {
		// shortcut: the backoff count is not persisted, a restart mid-backoff starts again at 15 min.
		c.strikes = min(c.strikes+1, 4)
		c.RetryAt = now + (rateLimitWait << (c.strikes - 1)).Milliseconds()
	} else {
		c.strikes, c.RetryAt = 0, 0
	}
	switch {
	case isHTTP && (hs.status == http.StatusUnauthorized || hs.status == http.StatusForbidden):
		c.FetchAuth = hs.status
	case fetchErr == nil:
		c.FetchAuth = 0
	}
	login := updateLogin(c, now)
	events := diffAlerts(&prev, c, float64(cfg.AlertBelow), now)
	if len(login) > 0 { // the login alert says it better than "quota fetch failed/recovered"
		events = slices.DeleteFunc(events, func(e alertEvent) bool { return e.Kind == "error" || e.Kind == "recovered" })
	}
	events = append(events, login...)
	waste := resetWaste(&prev, c, now)
	var points []histPoint
	if fetchErr == nil {
		for _, w := range c.Windows {
			if w.Used != nil {
				points = append(points, histPoint{now, c.AuthIndex, w.Key, *w.Used})
			}
		}
	}
	s.rebuildViewLocked()
	st := s.store
	snap := *c
	s.mu.Unlock()
	if fetchErr == nil || snap.RetryAt != 0 || snap.FetchAuth != 0 { // a backoff or a dead login must survive a restart too
		st.saveLastGood(snap)
	}
	st.addHistory(points)
	st.addWaste(waste)
	if cfg.AlertRunOut && fetchErr == nil {
		events = append(events, runOutAlerts(&snap, now,
			func(key string, from int64) []histPoint { return st.points(snap.AuthIndex, key, from) },
			func(key string, cycle int64) bool { return !st.fireOnce("runout|"+snap.AuthIndex+"|"+key, cycle) })...)
	}
	s.raise(cfg, st, events)
}

// notifyAsync must outlive the request or poll that raised the event, hence its own context.
func (s *service) notifyAsync(cfg settings, e alertEvent) {
	if cfg.WebhookURL == "" {
		return
	}
	go func() {
		err := s.sendWebhook(context.Background(), cfg, e)
		s.mu.Lock()
		s.poller.WebhookErr = ""
		if err != nil {
			s.poller.WebhookErr = err.Error()
		}
		s.mu.Unlock()
	}()
}

func (s *service) sendWebhook(ctx context.Context, cfg settings, e alertEvent) error {
	if cfg.WebhookURL == "" {
		return errors.New("webhook_url is not set")
	}
	if err := validWebhookURL(cfg.WebhookURL); err != nil {
		return err
	}
	body, ctype, err := webhookRequest(cfg.WebhookFormat, e)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", ctype)
	if cfg.WebhookFormat == "ntfy" {
		req.Header.Set("Title", "Quota Ledger")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---- management API client ----

// mgmt calls the loopback management API and decodes a JSON object answer.
func (s *service) mgmt(ctx context.Context, cfg settings, pw, method, path string, in any) (obj, error) {
	var body io.Reader
	if in != nil {
		raw, _ := json.Marshal(in)
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.ManagementURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+pw)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, fmt.Errorf("%s: management key rejected (check MANAGEMENT_PASSWORD)", path)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	var out obj
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return out, nil
}

var opencodeURL = regexp.MustCompile(`(?i)opencode\.ai`)

func (s *service) discover(ctx context.Context, cfg settings, pw string) ([]*credential, error) {
	files, err := s.mgmt(ctx, cfg, pw, http.MethodGet, "/v0/management/auth-files", nil)
	if err != nil {
		return nil, err
	}
	conf, err := s.mgmt(ctx, cfg, pw, http.MethodGet, "/v8/management/config", nil)
	if err != nil {
		return nil, err
	}
	return discovered(files, conf), nil
}

// discovered turns auth-files and config reads into credentials (Claude, Codex, OpenCode Go).
func discovered(files, conf obj) []*credential {
	var out []*credential
	list, _ := files["files"].([]any)
	for _, x := range list {
		f := asObj(x)
		provider := strings.ToLower(firstNonEmpty(str(f["provider"]), str(f["type"])))
		if provider != "claude" && provider != "codex" {
			continue
		}
		idx := str(f["auth_index"])
		tok := asObj(f["id_token"])
		c := &credential{
			ID: provider + ":" + idx, AuthIndex: idx, AuthID: str(f["id"]), Provider: provider,
			Name: str(f["name"]), Email: str(f["email"]), Disabled: truthy(f["disabled"]),
			accountID:  str(tok["chatgpt_account_id"]),
			AuthStatus: str(f["status"]), AuthMessage: str(f["status_message"]),
		}
		if provider == "codex" {
			c.Plan = str(tok["plan_type"])
		}
		out = append(out, c)
	}
	groups, _ := asObj(conf["api-keys"])["openai-compatibility"].([]any)
	for _, g := range groups {
		group := asObj(g)
		if !opencodeURL.MatchString(str(group["base-url"])) {
			continue
		}
		keys, _ := group["keys"].([]any)
		for _, k := range keys {
			key := asObj(k)
			idx := str(key["auth_index"])
			if idx == "" {
				continue
			}
			out = append(out, &credential{
				ID: "opencode:" + idx, AuthIndex: idx, Provider: "opencode",
				Name:     firstNonEmpty(str(group["name"]), "opencode") + " · " + maskKey(str(key["api-key"])),
				Disabled: truthy(group["disabled"]) || truthy(key["disabled"]),
			})
		}
	}
	return out
}

// apiCall runs one upstream GET through the host (which substitutes $TOKEN$) with ≤4 calls in flight.
func (s *service) apiCall(ctx context.Context, cfg settings, pw, authIndex, url string, header map[string]string) (any, error) {
	select {
	case s.callSem <- struct{}{}:
		defer func() { <-s.callSem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	res, err := s.mgmt(ctx, cfg, pw, http.MethodPost, "/v0/management/api-call",
		obj{"auth_index": authIndex, "method": "GET", "url": url, "header": header})
	if err != nil {
		return nil, err
	}
	var body any = res["body"]
	if text, ok := body.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(text), &parsed) == nil {
			body = parsed
		}
	}
	status := 0
	if n := num(res["status_code"]); n != nil {
		status = int(*n)
	}
	if status < 200 || status >= 300 {
		return nil, &httpStatusError{status, upstreamMessage(body)}
	}
	return body, nil
}

const (
	claudeUA = "claude-cli/2.1.280 (external, cli)"
	codexUA  = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
)

func (s *service) fetchCred(ctx context.Context, cfg settings, pw string, c credential, now int64) (fetchResult, error) {
	switch c.Provider {
	case "claude":
		h := map[string]string{"User-Agent": claudeUA, "Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "anthropic-beta": "oauth-2025-04-20"}
		uh := map[string]string{"Accept": "application/json"}
		for k, v := range h {
			uh[k] = v
		}
		usage, err := s.apiCall(ctx, cfg, pw, c.AuthIndex, "https://api.anthropic.com/api/oauth/usage", uh)
		if err != nil {
			return fetchResult{}, err
		}
		var profile any
		var profileErr error
		if c.Plan == "" || now-c.profileAt >= profileEvery.Milliseconds() { // otherwise the known plan is kept
			profile, profileErr = s.apiCall(ctx, cfg, pw, c.AuthIndex, "https://api.anthropic.com/api/oauth/profile", h)
		}
		return claudeResult(usage, profile, profileErr)
	case "codex":
		h := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "User-Agent": codexUA}
		if c.accountID != "" {
			h["Chatgpt-Account-Id"] = c.accountID
		}
		body, err := s.apiCall(ctx, cfg, pw, c.AuthIndex, "https://chatgpt.com/backend-api/wham/usage", h)
		if err != nil {
			return fetchResult{}, err
		}
		return codexResult(body, c.Plan, now)
	default:
		body, err := s.apiCall(ctx, cfg, pw, c.AuthIndex, "https://opencode.ai/zen/go/v1/usage", map[string]string{"Authorization": "Bearer $TOKEN$"})
		var he *httpStatusError
		if errors.As(err, &he) && he.status == http.StatusForbidden {
			return fetchResult{}, errors.New("No OpenCode Go subscription on this key")
		}
		if err != nil {
			return fetchResult{}, err
		}
		return openCodeResult(body)
	}
}

// ---- views for the management routes ----

type stateView struct {
	Version     string       `json:"version"`
	Now         int64        `json:"now"`
	Poller      pollerInfo   `json:"poller"`
	Credentials []credential `json:"credentials"`
	Settings    settingsView `json:"settings"`
	Scheduler   schedulerOut `json:"scheduler"`
}

type settingsView struct {
	PollMinutes   int           `json:"poll_interval_minutes"`
	HistoryDays   int           `json:"history_days"`
	AlertBelow    int           `json:"alert_below_percent"`
	AlertRunOut   bool          `json:"alert_run_out"`
	WebhookSet    bool          `json:"webhook_configured"`
	WebhookFormat string        `json:"webhook_format"`
	QuietHours    string        `json:"quiet_hours"`
	QuietNow      bool          `json:"quiet_now"`
	DigestDay     string        `json:"digest_day"`
	DigestHour    int           `json:"digest_hour"`
	DigestAt      int64         `json:"digest_at"`
	SchedulerMode string        `json:"scheduler_mode"`
	ReserveFloor  int           `json:"reserve_floor_percent"`
	ReserveExempt []string      `json:"reserve_exempt_user_agents"`
	Rules         []routingRule `json:"routing_rules"`
	Problems      []string      `json:"problems"`
}

type schedulerOut struct {
	Mode      string    `json:"mode"`
	Picks     int64     `json:"picks"`
	Fallbacks int64     `json:"fallbacks"`
	LastPick  *pickInfo `json:"last_pick"`
}

// health is an account's live request record, from the proxied traffic the host reports.
type health struct {
	Day        usageRow `json:"day"` // last 24 h, hourly buckets
	LastFail   string   `json:"last_fail,omitempty"`
	LastFailAt int64    `json:"last_fail_at,omitempty"`
	AvoidUntil int64    `json:"avoid_until,omitempty"` // the scheduler tries other accounts first until then
}

func (s *service) state() stateView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := nowMs()
	ids := s.store.idMap()
	muted := s.store.muted()
	day := s.store.authTotals(now - 24*hourMs)
	creds := make([]credential, len(s.creds))
	for i, c := range s.creds {
		creds[i] = *c
		if creds[i].Windows == nil {
			creds[i].Windows = []window{}
		}
		if creds[i].AuthID == "" { // e.g. OpenCode keys: learned from usage records
			for authID, idx := range ids {
				if idx == c.AuthIndex && (creds[i].AuthID == "" || authID < creds[i].AuthID) {
					creds[i].AuthID = authID
				}
			}
		}
		creds[i].Muted = muted[c.AuthIndex]
		h := &health{Day: day[c.AuthIndex], LastFail: c.lastFail, LastFailAt: c.lastFailAt}
		if until := avoidUntil(c); until > now && s.cfg.Mode != modeOff {
			h.AvoidUntil = until
		}
		creds[i].Health = h
	}
	p := s.poller
	if p.Error == "" {
		p.Error = firstNonEmpty(s.storeErr, s.store.err())
	}
	t := time.Now()
	rules := append([]routingRule{}, s.cfg.Rules...)
	for i := range rules {
		rules[i].Active = rules[i].activeAt(t)
	}
	return stateView{
		Version: version, Now: now, Poller: p, Credentials: creds,
		Settings: settingsView{
			PollMinutes: s.cfg.PollMinutes, HistoryDays: s.cfg.HistoryDays, AlertBelow: s.cfg.AlertBelow, AlertRunOut: s.cfg.AlertRunOut,
			WebhookSet: s.cfg.WebhookURL != "", WebhookFormat: s.cfg.WebhookFormat,
			QuietHours: s.cfg.QuietHours, QuietNow: s.cfg.quiet != nil && s.cfg.quiet.contains(t),
			DigestDay: s.cfg.DigestDay, DigestHour: s.cfg.DigestHour, DigestAt: s.store.digestAt(),
			SchedulerMode: s.cfg.Mode, ReserveFloor: s.cfg.ReserveFloor, ReserveExempt: append([]string{}, s.cfg.exemptList()...),
			Rules: rules, Problems: append([]string{}, s.cfg.problems...),
		},
		Scheduler: schedulerOut{Mode: s.cfg.Mode, Picks: s.picks.Load(), Fallbacks: s.fallback.Load(), LastPick: s.lastPick.Load()},
	}
}

// ---- scheduler and usage hooks ----

type pickRequest struct {
	Provider   string
	Model      string
	Options    struct{ Headers map[string][]string }
	Candidates []struct{ ID string }
}

func headerValue(h map[string][]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// pick never rejects: anything it cannot decide is left to the host's scheduler.
func (s *service) pick(raw []byte) obj {
	v := s.view.Load()
	if v == nil || v.cfg.Mode == modeOff {
		return obj{"Handled": false}
	}
	var req pickRequest
	if json.Unmarshal(raw, &req) != nil {
		return obj{"Handled": false}
	}
	ids := make([]string, len(req.Candidates))
	for i, c := range req.Candidates {
		ids[i] = c.ID
	}
	p := pickParams{At: time.Now(), Provider: req.Provider, Model: req.Model, UserAgent: headerValue(req.Options.Headers, "User-Agent")}
	id, e, rule, ok := pickAuth(v, p, ids)
	if !ok {
		s.fallback.Add(1)
		return obj{"Handled": false}
	}
	s.picks.Add(1)
	reason := v.cfg.Mode
	if rule >= 0 {
		reason = fmt.Sprintf("%s (rule %d)", firstNonEmpty(v.cfg.Rules[rule].Mode, v.cfg.Mode), rule+1)
	}
	s.lastPick.Store(&pickInfo{At: nowMs(), Provider: e.Provider, Model: req.Model, AuthIndex: e.AuthIndex, Reason: reason})
	return obj{"Handled": true, "AuthID": id}
}

type usageRecord struct {
	Model     string
	Alias     string
	APIKey    string
	AuthID    string
	AuthIndex string
	Failed    bool
	Failure   struct {
		StatusCode int
		Body       string
	}
	RequestedAt time.Time
	Latency     time.Duration
	Detail      struct {
		InputTokens         int64
		OutputTokens        int64
		CacheReadTokens     int64
		CacheCreationTokens int64
	}
}

// failText is a one-line failure summary: "HTTP 429: rate limited".
func failText(status int, body string) string {
	msg := strings.Join(strings.Fields(body), " ")
	if r := []rune(msg); len(r) > 120 {
		msg = string(r[:120]) + "…"
	}
	out := "failed"
	if status != 0 {
		out = fmt.Sprintf("HTTP %d", status)
	}
	if msg != "" {
		out += ": " + msg
	}
	return out
}

func (s *service) usage(raw []byte) {
	var r usageRecord
	if json.Unmarshal(raw, &r) != nil {
		return
	}
	s.mu.RLock()
	st := s.store
	s.mu.RUnlock()
	if r.AuthID != "" && r.AuthIndex != "" && st.learn(r.AuthID, r.AuthIndex) {
		s.mu.Lock()
		s.rebuildViewLocked()
		s.mu.Unlock()
	}
	if r.AuthIndex == "" {
		r.AuthIndex = st.index(r.AuthID)
	}
	at := r.RequestedAt.UnixMilli()
	if r.RequestedAt.IsZero() {
		at = nowMs()
	}
	dt := r.Detail
	d := usageRow{Requests: 1, InputTokens: dt.InputTokens, OutputTokens: dt.OutputTokens, CacheReadTokens: dt.CacheReadTokens, CacheCreationTokens: dt.CacheCreationTokens}
	status := r.Failure.StatusCode
	if r.Failed {
		d.Failed = 1
		switch {
		case status == http.StatusTooManyRequests:
			d.RateLimited = 1
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			d.AuthFailed = 1
		case status >= 500:
			d.ServerErrors = 1
		}
	}
	if r.Latency > 0 {
		d.LatencyMs, d.Timed = r.Latency.Milliseconds(), 1
	}
	st.addUsage(at-at%hourMs, r.AuthIndex, maskKey(r.APIKey), firstNonEmpty(r.Model, r.Alias), d)
	if r.AuthIndex != "" {
		s.observe(r.AuthIndex, r.Failed, status, r.Failure.Body)
	}
}

// observe folds one proxied request outcome into the account's live state: a 429 makes the scheduler
// try other accounts first for a while, a 401/403 not followed by a success marks the login as expired.
func (s *service) observe(authIndex string, failed bool, status int, body string) {
	now := nowMs()
	s.mu.Lock()
	var c *credential
	for _, x := range s.creds {
		if x.AuthIndex == authIndex {
			c = x
		}
	}
	if c == nil {
		s.mu.Unlock()
		return
	}
	login, avoid := c.Login, avoidUntil(c)
	if failed {
		c.lastFail, c.lastFailAt = failText(status, body), now
		switch status {
		case http.StatusTooManyRequests:
			c.live429At = now
		case http.StatusUnauthorized, http.StatusForbidden:
			c.liveAuthAt = now
		}
	} else {
		c.liveOKAt = now
	}
	events := updateLogin(c, now)
	if status == http.StatusTooManyRequests && now-c.live429Sent >= hourMs {
		c.live429Sent = now
		events = append(events, alertEvent{At: now, Kind: "traffic", AuthIndex: c.AuthIndex, Name: c.Name, Provider: c.Provider,
			Message: "requests are being rate limited upstream (" + c.lastFail + ")"})
	}
	if c.Login != login || avoidUntil(c) != avoid {
		s.rebuildViewLocked()
	}
	st, cfg := s.store, s.cfg
	s.mu.Unlock()
	s.raise(cfg, st, events)
}
