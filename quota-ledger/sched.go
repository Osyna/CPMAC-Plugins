package main

import (
	"math"
	"strings"
	"time"
)

const (
	modeOff           = "off"
	modeMostRemaining = "most-remaining"
	modeSoonestReset  = "soonest-reset"
	modeUseFirst      = "use-first"
)

type schedConfig struct {
	Mode   string
	Floor  float64
	Exempt []string
	Rules  []routingRule
}

// quotaEntry is a credential's binding quota: the most-used plan window decides.
type quotaEntry struct {
	AuthIndex string
	Provider  string
	Remaining float64
	ResetAt   int64    // epoch ms; 0 = unknown
	Loss      float64  // use-first: projected unused share of the most urgent ≥1-day window
	LossReset int64    // that window's reset
	scoped    []window // model-scoped windows ("scoped:<model>"), applied per request model
	now       int64    // view build time, for scoped losses
}

type schedView struct {
	cfg    schedConfig
	byAuth map[string]quotaEntry // host auth ID -> binding quota; credentials with unknown quota are absent
	avoid  map[string]int64      // host auth ID -> until (epoch ms): failing in live traffic, used only if nothing else fits
}

// pickParams is the request context the scheduler decides on.
type pickParams struct {
	At        time.Time // local time, for rules
	Provider  string
	Model     string
	UserAgent string
}

// policy returns the mode and reserve floor for a request: the first matching rule overrides the
// global ones (rule index -1 = none). A rule's floor applies to every request it matches.
func (c schedConfig) policy(p pickParams) (mode string, floor float64, exempt bool, rule int) {
	mode, floor, exempt, rule = c.Mode, c.Floor, true, -1
	for i, r := range c.Rules {
		if !r.matches(p.At, p.Provider, p.Model, p.UserAgent) {
			continue
		}
		if r.Mode != "" {
			mode = r.Mode
		}
		if r.Floor != nil {
			floor, exempt = float64(*r.Floor), false
		}
		return mode, floor, exempt, i
	}
	return mode, floor, exempt, rule
}

// planWindow reports whether a window limits every request: the dollar credit pool does not
// block plan usage, and model-scoped windows only bind requests for that model.
func planWindow(w window) bool {
	return w.Key != "credits" && !strings.HasPrefix(w.Key, "scoped:")
}

// binding returns the lowest remaining over plan windows with a known usage and that window's reset.
func binding(ws []window) (rem float64, reset int64, ok bool) {
	rem = math.Inf(1)
	for _, w := range ws {
		if w.Used == nil || !planWindow(w) {
			continue
		}
		r := 100 - *w.Used
		var at int64
		if w.ResetAt != nil {
			at = *w.ResetAt
		}
		if r < rem || (r == rem && at != 0 && (reset == 0 || at < reset)) {
			rem, reset = r, at
		}
		ok = true
	}
	return rem, reset, ok
}

// forModel tightens the entry with scoped windows whose model name appears in the requested model
// ("scoped:fable 5" binds "claude-fable-5-1"); their projected loss counts for use-first too.
func (q quotaEntry) forModel(model string) quotaEntry {
	m := strings.ToLower(model)
	for _, w := range q.scoped {
		name := strings.ReplaceAll(strings.TrimPrefix(w.Key, "scoped:"), " ", "-")
		if w.Used == nil || name == "" || !strings.Contains(m, name) {
			continue
		}
		if r := 100 - *w.Used; r < q.Remaining {
			q.Remaining, q.ResetAt = r, 0
			if w.ResetAt != nil {
				q.ResetAt = *w.ResetAt
			}
		}
		if l := lossOf(w, q.now); l >= minLoss && (q.Loss < minLoss || *w.ResetAt < q.LossReset || *w.ResetAt == q.LossReset && l > q.Loss) {
			q.Loss, q.LossReset = l, *w.ResetAt
		}
	}
	return q
}

// urgentLoss is the use-first key: the soonest-resetting ≥1-day plan window that would lose ≥5%.
func urgentLoss(ws []window, now int64) (loss float64, reset int64) {
	for _, w := range ws {
		if !planWindow(w) {
			continue
		}
		if l := lossOf(w, now); l >= minLoss && (reset == 0 || *w.ResetAt < reset || *w.ResetAt == reset && l > loss) {
			loss, reset = l, *w.ResetAt
		}
	}
	return loss, reset
}

// buildView indexes credentials by every host auth ID known for them and sets each Reserved flag.
// ids maps learned auth ID -> auth index (from usage records).
func buildView(cfg schedConfig, creds []*credential, ids map[string]string, now int64) *schedView {
	byIndex := make(map[string]quotaEntry, len(creds))
	for _, c := range creds {
		c.Reserved = false
		if c.Disabled {
			continue
		}
		rem, reset, ok := binding(c.Windows)
		if !ok {
			continue
		}
		e := quotaEntry{AuthIndex: c.AuthIndex, Provider: c.Provider, Remaining: rem, ResetAt: reset, now: now}
		e.Loss, e.LossReset = urgentLoss(c.Windows, now)
		for _, w := range c.Windows {
			if strings.HasPrefix(w.Key, "scoped:") {
				e.scoped = append(e.scoped, w)
			}
		}
		byIndex[c.AuthIndex] = e
		c.Reserved = cfg.Mode != modeOff && cfg.Floor > 0 && rem < cfg.Floor
	}
	v := &schedView{cfg: cfg, byAuth: make(map[string]quotaEntry, len(byIndex)), avoid: map[string]int64{}}
	avoidIdx := map[string]int64{}
	for _, c := range creds {
		if until := avoidUntil(c); until > now {
			avoidIdx[c.AuthIndex] = until
		}
		if e, ok := byIndex[c.AuthIndex]; ok && c.AuthID != "" {
			v.byAuth[c.AuthID] = e
		}
	}
	for authID, idx := range ids {
		if e, ok := byIndex[idx]; ok {
			v.byAuth[authID] = e
		}
	}
	for authID, idx := range ids {
		if until, ok := avoidIdx[idx]; ok {
			v.avoid[authID] = until
		}
	}
	for _, c := range creds {
		if until, ok := avoidIdx[c.AuthIndex]; ok && c.AuthID != "" {
			v.avoid[c.AuthID] = until
		}
	}
	return v
}

// liveAvoid is how long a credential that just answered 429 in live traffic is passed over.
const liveAvoid = 2 * 60_000

// avoidUntil: an expired login is avoided until it recovers, a live 429 for liveAvoid.
func avoidUntil(c *credential) int64 {
	if c.Login != "" {
		return math.MaxInt64
	}
	if c.live429At > 0 {
		return c.live429At + liveAvoid
	}
	return 0
}

func isExempt(exempt []string, userAgent string) bool {
	ua := strings.ToLower(userAgent)
	for _, e := range exempt {
		if e != "" && strings.Contains(ua, strings.ToLower(e)) {
			return true
		}
	}
	return false
}

// pickAuth chooses among candidate auth IDs; ok=false means "not handled" and the host schedules.
// Candidates failing in live traffic are tried only when no healthy one fits.
func pickAuth(v *schedView, p pickParams, candidates []string) (id string, e quotaEntry, rule int, ok bool) {
	if v == nil || v.cfg.Mode == modeOff {
		return "", e, -1, false
	}
	mode, floor, exemptApplies, rule := v.cfg.policy(p)
	if mode == modeOff {
		return "", e, rule, false
	}
	reserve := floor > 0 && !(exemptApplies && isExempt(v.cfg.Exempt, p.UserAgent))
	for _, healthy := range []bool{true, false} {
		for _, cand := range candidates {
			q, known := v.byAuth[cand]
			if failing := v.avoid[cand] > p.At.UnixMilli(); !known || failing == healthy {
				continue
			}
			q = q.forModel(p.Model)
			if reserve && q.Remaining < floor {
				continue
			}
			if (mode == modeSoonestReset || mode == modeUseFirst) && q.Remaining <= 0 {
				continue
			}
			if !ok || better(mode, q, e) {
				id, e, ok = cand, q, true
			}
		}
		if ok {
			break
		}
	}
	return id, e, rule, ok
}

// better reports whether a beats b; ties keep the earlier candidate.
func better(mode string, a, b quotaEntry) bool {
	switch mode {
	case modeSoonestReset:
		if ar, br := resetKey(a), resetKey(b); ar != br {
			return ar < br
		}
	case modeUseFirst:
		al, bl := a.Loss >= minLoss, b.Loss >= minLoss
		switch {
		case al != bl:
			return al
		case al && a.LossReset != b.LossReset:
			return a.LossReset < b.LossReset
		case al && a.Loss != b.Loss:
			return a.Loss > b.Loss
		}
	}
	return a.Remaining > b.Remaining
}

func resetKey(q quotaEntry) int64 {
	if q.ResetAt == 0 {
		return math.MaxInt64
	}
	return q.ResetAt
}
