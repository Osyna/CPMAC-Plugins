package main

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const version = "0.3.0"

type credential struct {
	ID        string   `json:"id"`
	AuthIndex string   `json:"auth_index"`
	AuthID    string   `json:"auth_id"`
	Provider  string   `json:"provider"`
	Name      string   `json:"name"`
	Email     string   `json:"email"`
	Plan      string   `json:"plan"`
	Disabled  bool     `json:"disabled"`
	Status    string   `json:"status"` // pending | ok | error | disabled
	Error     string   `json:"error"`
	FetchedAt int64    `json:"fetched_at"`
	Windows   []window `json:"windows"`
	Spend     *spend   `json:"spend"`
	Credits   *credits `json:"credits"`
	Reserved  bool     `json:"reserved"`
	RetryAt   int64    `json:"retry_at,omitempty"` // rate limited upstream: no fetch before this (epoch ms)
	// Login health: "" fine, "expired" (OAuth login needs redoing) or "rejected" (API key refused).
	Login       string  `json:"login,omitempty"`
	LoginReason string  `json:"login_reason,omitempty"`
	AuthStatus  string  `json:"auth_status,omitempty"`  // host auth state: active | error | refreshing | pending
	AuthMessage string  `json:"auth_message,omitempty"` // host status message for that state
	Muted       bool    `json:"muted,omitempty"`        // alerts recorded but not sent (webhook, browser)
	Health      *health `json:"health,omitempty"`       // live request outcomes (state view only)
	accountID   string  // codex Chatgpt-Account-Id
	strikes     int     // consecutive 429s, drives the backoff
	profileAt   int64   // last Claude profile (plan) read
	FetchAuth   int     `json:"fetch_auth,omitempty"` // 401/403 from the last quota check, 0 otherwise
	liveAuthAt  int64   // last 401/403 seen in proxied traffic
	liveOKAt    int64   // last successful proxied request
	live429At   int64   // last 429 seen in proxied traffic
	live429Sent int64   // last live rate-limit alert
	lastFail    string  // last live failure, "HTTP 429: message"
	lastFailAt  int64
}

type settings struct {
	PollMinutes   int           `yaml:"poll_interval_minutes"`
	HistoryDays   int           `yaml:"history_days"`
	ManagementURL string        `yaml:"management_url"`
	DataDir       string        `yaml:"data_dir"`
	AlertBelow    int           `yaml:"alert_below_percent"`
	AlertRunOut   bool          `yaml:"alert_run_out"`
	WebhookURL    string        `yaml:"webhook_url"`
	WebhookFormat string        `yaml:"webhook_format"`
	QuietHours    string        `yaml:"quiet_hours"` // "22:00-07:00" server local time; webhooks are not sent inside
	DigestDay     string        `yaml:"digest_day"`  // off | mon … sun
	DigestHour    int           `yaml:"digest_hour"` // server local hour
	Mode          string        `yaml:"scheduler_mode"`
	ReserveFloor  int           `yaml:"reserve_floor_percent"`
	Exempt        any           `yaml:"reserve_exempt_user_agents"` // comma-separated string; a YAML list is accepted too
	Rules         []routingRule `yaml:"routing_rules"`
	quiet         *span         // parsed QuietHours; nil = none
	problems      []string      // config entries that were ignored, shown on the page
}

func defaultSettings() settings {
	return settings{
		PollMinutes: 5, HistoryDays: 14, ManagementURL: "http://127.0.0.1:8317", DataDir: "quota-ledger-data",
		AlertBelow: 20, AlertRunOut: true, WebhookFormat: "json", DigestDay: "mon", DigestHour: 9, Mode: modeOff, Exempt: "claude-cli",
	}
}

// parseSettings reads the plugin's config_yaml; omitted keys keep defaults, out-of-range values are clamped.
func parseSettings(raw []byte) (settings, error) {
	s := defaultSettings()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &s); err != nil {
			return s, err
		}
	}
	s.PollMinutes = max(s.PollMinutes, 1)
	s.HistoryDays = max(s.HistoryDays, 1)
	s.AlertBelow = min(max(s.AlertBelow, 0), 100)
	s.ReserveFloor = min(max(s.ReserveFloor, 0), 100)
	s.ManagementURL = strings.TrimRight(strings.TrimSpace(s.ManagementURL), "/")
	if s.ManagementURL == "" {
		s.ManagementURL = defaultSettings().ManagementURL
	}
	if strings.TrimSpace(s.DataDir) == "" {
		s.DataDir = defaultSettings().DataDir
	}
	s.WebhookURL = strings.TrimSpace(s.WebhookURL)
	switch s.WebhookFormat {
	case "json", "discord", "slack", "ntfy":
	default:
		s.WebhookFormat = "json"
	}
	switch s.Mode {
	case modeOff, modeMostRemaining, modeSoonestReset, modeUseFirst:
	default:
		s.Mode = modeOff
	}
	s.DigestHour = min(max(s.DigestHour, 0), 23)
	if _, ok := weekdays[strings.ToLower(strings.TrimSpace(s.DigestDay))]; ok {
		s.DigestDay = strings.ToLower(strings.TrimSpace(s.DigestDay))
	} else {
		s.DigestDay = "off"
	}
	// Bad quiet hours or rules are dropped and reported on the page, not fatal: a failed register would
	// unload the plugin (page and scheduler) over a typo.
	s.problems = nil
	if q := strings.TrimSpace(s.QuietHours); q != "" {
		if sp, err := parseSpan(q); err != nil {
			s.problems = append(s.problems, "quiet_hours: "+err.Error())
		} else {
			s.quiet = &sp
		}
	}
	kept := s.Rules[:0]
	for i, r := range s.Rules {
		if err := r.compile(); err != nil {
			s.problems = append(s.problems, fmt.Sprintf("routing_rules[%d]: %v", i, err))
			continue
		}
		kept = append(kept, r)
	}
	s.Rules = kept
	return s, nil
}

func (s settings) exemptList() []string {
	var parts []string
	switch v := s.Exempt.(type) {
	case string:
		parts = strings.Split(v, ",")
	case []any:
		for _, x := range v {
			parts = append(parts, str(x))
		}
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s settings) sched() schedConfig {
	return schedConfig{Mode: s.Mode, Floor: float64(s.ReserveFloor), Exempt: s.exemptList(), Rules: s.Rules}
}

type configField struct {
	Name        string
	Type        string
	Default     any
	EnumValues  []string
	Description string
}

func configFields() []configField {
	d := defaultSettings()
	return []configField{
		{Name: "poll_interval_minutes", Type: "integer", Default: d.PollMinutes, Description: "How often the server polls provider quotas, in minutes (minimum 1)."},
		{Name: "history_days", Type: "integer", Default: d.HistoryDays, Description: "Retention in days for quota history, usage attribution and alerts."},
		{Name: "management_url", Type: "string", Default: d.ManagementURL, Description: "Loopback base URL of the management API the poller calls (uses MANAGEMENT_PASSWORD)."},
		{Name: "data_dir", Type: "string", Default: d.DataDir, Description: "Directory for history, usage and alerts, relative to the server's working directory."},
		{Name: "alert_below_percent", Type: "integer", Default: d.AlertBelow, Description: "Raise a 'below' alert when a window's remaining quota drops under this percent (0 disables)."},
		{Name: "alert_run_out", Type: "boolean", Default: d.AlertRunOut, Description: "Alert once per window when, at the recent pace, it would hit 0% before it resets."},
		{Name: "webhook_url", Type: "string", Default: "", Description: "Webhook that receives alerts; empty disables the webhook."},
		{Name: "webhook_format", Type: "enum", Default: d.WebhookFormat, EnumValues: []string{"json", "discord", "slack", "ntfy"}, Description: "Webhook body: json event object, discord {content}, slack {text} or ntfy plain text."},
		{Name: "quiet_hours", Type: "string", Default: "", Description: "Server local time range like 22:00-07:00 when alerts are not sent to the webhook (still listed on the page). Empty = always send."},
		{Name: "digest_day", Type: "enum", Default: d.DigestDay, EnumValues: []string{"off", "mon", "tue", "wed", "thu", "fri", "sat", "sun"}, Description: "Day the weekly digest goes to the webhook; 'off' disables it."},
		{Name: "digest_hour", Type: "integer", Default: d.DigestHour, Description: "Server local hour (0-23) the weekly digest is sent."},
		{Name: "scheduler_mode", Type: "enum", Default: d.Mode, EnumValues: []string{modeOff, modeMostRemaining, modeSoonestReset, modeUseFirst}, Description: "Quota-aware credential scheduling; 'off' defers entirely to the built-in scheduler. use-first spends the quota that would otherwise be lost at reset first."},
		{Name: "reserve_floor_percent", Type: "integer", Default: d.ReserveFloor, Description: "0 = off. Otherwise credentials below this remaining percent are skipped for non-exempt clients."},
		{Name: "reserve_exempt_user_agents", Type: "string", Default: "claude-cli", Description: "Comma-separated User-Agent substrings that may still use reserved credentials."},
		{Name: "routing_rules", Type: "array", Default: []any{}, Description: "Ordered rules; the first that matches a request overrides scheduler_mode and the reserve floor. Fields: days (mon-fri), hours (09:00-18:00), provider, model (substring), client (User-Agent substring, !x = not x), mode, reserve_floor_percent."},
	}
}
