package main

import (
	"encoding/json"
	"fmt"
	"net/url"
)

const resetDropPoints = 25

type alertEvent struct {
	At        int64    `json:"at"`
	Kind      string   `json:"kind"` // below | reset | error | recovered | runout | login | traffic | digest
	AuthIndex string   `json:"auth_index"`
	Name      string   `json:"name"`
	Provider  string   `json:"provider"`
	Window    string   `json:"window"`
	Label     string   `json:"label"`
	Remaining *float64 `json:"remaining"`
	Message   string   `json:"message"`
	Muted     bool     `json:"muted,omitempty"` // the account is muted: not sent to the webhook or the browser
}

// diffAlerts compares a credential before and after a fetch. prev is a shallow copy taken before the update.
func diffAlerts(prev, cur *credential, belowPct float64, now int64) []alertEvent {
	base := alertEvent{At: now, AuthIndex: cur.AuthIndex, Name: cur.Name, Provider: cur.Provider}
	var out []alertEvent
	switch {
	case prev.Status == "ok" && cur.Status == "error":
		e := base
		e.Kind, e.Message = "error", "quota fetch failed: "+cur.Error
		out = append(out, e)
	case prev.Status == "error" && cur.Status == "ok":
		e := base
		e.Kind, e.Message = "recovered", "quota fetch recovered"
		out = append(out, e)
	}
	if cur.Status != "ok" {
		return out
	}
	before := make(map[string]window, len(prev.Windows))
	for _, w := range prev.Windows {
		before[w.Key] = w
	}
	for _, w := range cur.Windows {
		p, seen := before[w.Key]
		if !seen || p.Used == nil || w.Used == nil {
			continue
		}
		rem := 100 - *w.Used
		e := base
		e.Window, e.Label, e.Remaining = w.Key, w.Label, &rem
		switch {
		case *p.Used-*w.Used >= resetDropPoints || (p.ResetAt != nil && *p.ResetAt <= now && *w.Used < *p.Used):
			e.Kind, e.Message = "reset", fmt.Sprintf("%s reset: %.0f%% remaining", w.Label, rem)
			out = append(out, e)
		case belowPct > 0 && w.Key != "credits" && 100-*p.Used >= belowPct && rem < belowPct: // an empty dollar pool does not block plan usage
			e.Kind, e.Message = "below", fmt.Sprintf("%s at %.0f%% remaining (below %.0f%%)", w.Label, rem, belowPct)
			out = append(out, e)
		}
	}
	return out
}

func alertText(e alertEvent) string {
	if e.Kind == "digest" {
		return e.Message
	}
	return fmt.Sprintf("Quota Ledger · %s · %s", e.Name, e.Message)
}

// runOutAlerts raises one "runout" alert per window cycle when, at the recent pace, the window hits 0%
// at least 30 min before it resets. points returns a window's history; seen reports an earlier alert.
func runOutAlerts(c *credential, now int64, points func(key string, from int64) []histPoint, seen func(key string, since int64) bool) []alertEvent {
	var out []alertEvent
	for _, w := range c.Windows {
		if w.Key == "credits" || w.ResetAt == nil || w.PeriodS <= 0 {
			continue
		}
		cycle := *w.ResetAt - w.PeriodS*1000
		at := runOut(points(w.Key, max(now-24*hourMs, cycle)), w, now)
		if at == 0 || seen(w.Key, cycle) {
			continue
		}
		rem := 100 - *w.Used
		out = append(out, alertEvent{At: now, Kind: "runout", AuthIndex: c.AuthIndex, Name: c.Name, Provider: c.Provider,
			Window: w.Key, Label: w.Label, Remaining: &rem,
			Message: fmt.Sprintf("%s hits 0%% %s at this pace, resets %s", w.Label, inDur(at-now), inDur(*w.ResetAt-now))})
	}
	return out
}

// webhookRequest renders an event for the configured format; returns body and Content-Type.
func webhookRequest(format string, e alertEvent) ([]byte, string, error) {
	switch format {
	case "discord":
		b, err := json.Marshal(map[string]string{"content": alertText(e)})
		return b, "application/json", err
	case "slack":
		b, err := json.Marshal(map[string]string{"text": alertText(e)})
		return b, "application/json", err
	case "ntfy":
		return []byte(alertText(e)), "text/plain; charset=utf-8", nil
	}
	b, err := json.Marshal(e)
	return b, "application/json", err
}

func validWebhookURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("webhook_url must be an absolute http(s) URL")
	}
	return nil
}
