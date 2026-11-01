package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// On-disk layout under data_dir:
//   history.jsonl  append-only, one JSON array per line: [ts_ms, "auth_index", "window_key", used]
//   usage.json     {"ids":{auth_id: auth_index}, "buckets":[usageBucket...]}  (rewritten atomically)
//   alerts.json    [alertEvent...], newest last, at most maxAlerts  (rewritten atomically)
//   last.json      {credential id: credential} last successful fetch per credential  (rewritten atomically)
//   waste.json     [wasteEvent...] unused share of each window at its reset, kept wasteDays  (rewritten atomically)
//   prefs.json     page-set preferences: muted accounts, last digest  (rewritten atomically)

const (
	maxAlerts  = 200
	wasteDays  = 180 // long enough for month-over-month trends whatever history_days is
	hourMs     = int64(time.Hour / time.Millisecond)
	historyLog = "history.jsonl"
	usageFile  = "usage.json"
	alertsFile = "alerts.json"
	lastFile   = "last.json"
	wasteFile  = "waste.json"
	prefsFile  = "prefs.json"
)

type prefs struct {
	Muted    map[string]bool  `json:"muted"`     // auth index -> muted
	DigestAt int64            `json:"digest_at"` // last digest sent (epoch ms)
	Fired    map[string]int64 `json:"fired"`     // once-per-cycle alert key -> cycle it fired in
}

type histPoint struct {
	T    int64
	Auth string
	Key  string
	Used float64
}

type usageKey struct {
	Hour   int64
	Auth   string
	Client string
	Model  string
}

type usageRow struct {
	AuthIndex           string `json:"auth_index"`
	ClientKey           string `json:"client_key"`
	Model               string `json:"model"`
	Requests            int64  `json:"requests"`
	Failed              int64  `json:"failed"`
	RateLimited         int64  `json:"rate_limited"`  // failed with 429
	AuthFailed          int64  `json:"auth_failed"`   // failed with 401/403
	ServerErrors        int64  `json:"server_errors"` // failed with 5xx
	LatencyMs           int64  `json:"latency_ms"`    // sum over Timed requests
	Timed               int64  `json:"timed"`
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheReadTokens     int64  `json:"cache_read_tokens"`
	CacheCreationTokens int64  `json:"cache_creation_tokens"`
}

func (r *usageRow) add(d usageRow) {
	r.Requests += d.Requests
	r.Failed += d.Failed
	r.RateLimited += d.RateLimited
	r.AuthFailed += d.AuthFailed
	r.ServerErrors += d.ServerErrors
	r.LatencyMs += d.LatencyMs
	r.Timed += d.Timed
	r.InputTokens += d.InputTokens
	r.OutputTokens += d.OutputTokens
	r.CacheReadTokens += d.CacheReadTokens
	r.CacheCreationTokens += d.CacheCreationTokens
}

type usageBucket struct {
	Hour int64 `json:"hour"` // bucket start, epoch ms
	usageRow
}

type series struct {
	AuthIndex string       `json:"auth_index"`
	Key       string       `json:"key"`
	Points    [][2]float64 `json:"points"`
}

// store keeps everything in memory and mirrors it to dir; dir == "" is memory-only.
type store struct {
	dir     string
	mu      sync.Mutex
	hist    []histPoint
	usage   map[usageKey]*usageRow
	ids     map[string]string
	alerts  []alertEvent
	last    map[string]credential // last successful fetch per credential ID, survives restarts
	waste   []wasteEvent
	prefs   prefs
	dirty   bool
	lastErr string
}

func openStore(dir string, historyDays int, now int64) (*store, error) {
	s := &store{usage: map[usageKey]*usageRow{}, ids: map[string]string{}, last: map[string]credential{}, prefs: prefs{Muted: map[string]bool{}}}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return s, fmt.Errorf("data_dir: %w", err)
	}
	s.dir = dir
	if err := s.load(); err != nil {
		return s, fmt.Errorf("data_dir: %w", err)
	}
	s.prune(historyDays, now)
	return s, nil
}

func (s *store) load() error {
	if f, err := os.Open(filepath.Join(s.dir, historyLog)); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var r []any
			if json.Unmarshal(sc.Bytes(), &r) != nil || len(r) != 4 {
				continue // a torn last line from a crash is dropped
			}
			t, a, k, u := num(r[0]), str(r[1]), str(r[2]), num(r[3])
			if t != nil && u != nil {
				s.hist = append(s.hist, histPoint{int64(*t), a, k, *u})
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var u struct {
		IDs     map[string]string `json:"ids"`
		Buckets []usageBucket     `json:"buckets"`
	}
	if err := readJSON(filepath.Join(s.dir, usageFile), &u); err != nil {
		return err
	}
	for id, idx := range u.IDs {
		s.ids[id] = idx
	}
	for _, b := range u.Buckets {
		row := b.usageRow
		s.usage[usageKey{b.Hour, b.AuthIndex, b.ClientKey, b.Model}] = &row
	}
	if err := readJSON(filepath.Join(s.dir, lastFile), &s.last); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(s.dir, wasteFile), &s.waste); err != nil {
		return err
	}
	if err := readJSON(filepath.Join(s.dir, prefsFile), &s.prefs); err != nil {
		return err
	}
	if s.prefs.Muted == nil {
		s.prefs.Muted = map[string]bool{}
	}
	return readJSON(filepath.Join(s.dir, alertsFile), &s.alerts)
}

func readJSON(path string, v any) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// writeAtomic writes via a temp file in the same directory, then renames over path.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func (s *store) fail(err error) {
	if err != nil {
		s.lastErr = "data_dir: " + err.Error()
	} else {
		s.lastErr = ""
	}
}

func (s *store) err() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// lastGood returns the credential's last saved snapshot (last good data plus any rate-limit backoff).
func (s *store) lastGood(id string) (credential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.last[id]
	return c, ok
}

// saveLastGood records the last good data and any rate-limit backoff, so a restart shows the last
// known quota and keeps waiting out a 429 instead of calling the upstream again right away.
func (s *store) saveLastGood(c credential) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]credential{}
	}
	s.last[c.ID] = credential{ID: c.ID, Plan: c.Plan, Status: c.Status, Error: c.Error, RetryAt: c.RetryAt, FetchAuth: c.FetchAuth, FetchedAt: c.FetchedAt, Windows: c.Windows, Spend: c.Spend, Credits: c.Credits}
	if s.dir == "" {
		return
	}
	data, err := json.Marshal(s.last)
	if err == nil {
		err = writeAtomic(filepath.Join(s.dir, lastFile), data)
	}
	s.fail(err)
}

// prune drops history, usage and alerts older than days and rewrites the files that shrank.
func (s *store) prune(days int, now int64) {
	cutoff := now - int64(days)*24*hourMs
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.hist[:0]
	for _, p := range s.hist {
		if p.T >= cutoff {
			kept = append(kept, p)
		}
	}
	if len(kept) != len(s.hist) {
		s.hist = kept
		s.rewriteHistory()
	}
	for k := range s.usage {
		if k.Hour+hourMs < cutoff {
			delete(s.usage, k)
			s.dirty = true
		}
	}
	s.filterAlertsLocked(func(a alertEvent) bool { return a.At >= cutoff })
	wcut := now - wasteDays*24*hourMs
	waste := s.waste[:0]
	for _, w := range s.waste {
		if w.At >= wcut {
			waste = append(waste, w)
		}
	}
	if len(waste) != len(s.waste) {
		s.waste = waste
		s.writeJSONLocked(wasteFile, s.waste)
	}
	s.flushLocked()
}

// filterAlertsLocked keeps the alerts keep accepts and rewrites the file when any went; returns how many.
func (s *store) filterAlertsLocked(keep func(alertEvent) bool) int {
	kept := s.alerts[:0]
	for _, a := range s.alerts {
		if keep(a) {
			kept = append(kept, a)
		}
	}
	n := len(s.alerts) - len(kept)
	if n > 0 {
		s.alerts = kept
		s.writeAlerts()
	}
	return n
}

// dismissAlerts removes alerts: all of them, or the one matching e's at/kind/auth_index/window.
func (s *store) dismissAlerts(all bool, e alertEvent) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filterAlertsLocked(func(a alertEvent) bool {
		return !all && (a.At != e.At || a.Kind != e.Kind || a.AuthIndex != e.AuthIndex || a.Window != e.Window)
	})
}

// forget drops alerts, saved snapshots, mutes and waste of accounts that no longer exist.
// known holds the current auth indexes; alerts without an account (e.g. digests) stay.
func (s *store) forget(known map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filterAlertsLocked(func(a alertEvent) bool { return a.AuthIndex == "" || known[a.AuthIndex] })
	gone := false
	for id := range s.last {
		if _, idx, _ := strings.Cut(id, ":"); !known[idx] {
			delete(s.last, id)
			gone = true
		}
	}
	if gone {
		s.writeJSONLocked(lastFile, s.last)
	}
	gone = false
	for idx := range s.prefs.Muted {
		if !known[idx] {
			delete(s.prefs.Muted, idx)
			gone = true
		}
	}
	if gone {
		s.writeJSONLocked(prefsFile, s.prefs)
	}
}

func (s *store) writeJSONLocked(name string, v any) {
	if s.dir == "" {
		return
	}
	raw, err := json.Marshal(v)
	if err == nil {
		err = writeAtomic(filepath.Join(s.dir, name), raw)
	}
	s.fail(err)
}

// fireOnce reports whether key has not fired in this cycle yet, and records that it has now.
// Kept apart from the alert list so dismissing an alert does not make it fire again.
func (s *store) fireOnce(key string, cycle int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefs.Fired == nil {
		s.prefs.Fired = map[string]int64{}
	}
	if s.prefs.Fired[key] >= cycle {
		return false
	}
	s.prefs.Fired[key] = cycle
	s.writeJSONLocked(prefsFile, s.prefs)
	return true
}

func (s *store) setMuted(authIndex string, muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefs.Muted == nil {
		s.prefs.Muted = map[string]bool{}
	}
	if muted {
		s.prefs.Muted[authIndex] = true
	} else {
		delete(s.prefs.Muted, authIndex)
	}
	s.writeJSONLocked(prefsFile, s.prefs)
}

func (s *store) muted() map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]bool, len(s.prefs.Muted))
	for k, v := range s.prefs.Muted {
		out[k] = v
	}
	return out
}

func (s *store) digestAt() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prefs.DigestAt
}

func (s *store) setDigestAt(at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefs.DigestAt = at
	s.writeJSONLocked(prefsFile, s.prefs)
}

func (s *store) addWaste(evs []wasteEvent) {
	if len(evs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waste = append(s.waste, evs...)
	s.writeJSONLocked(wasteFile, s.waste)
}

func (s *store) wasteSince(from int64) []wasteEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []wasteEvent{}
	for _, w := range s.waste {
		if w.At >= from {
			out = append(out, w)
		}
	}
	return out
}

// points returns one window's history for an account from `from` on, oldest first.
func (s *store) points(authIndex, key string, from int64) []histPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []histPoint
	for _, p := range s.hist {
		if p.T >= from && p.Auth == authIndex && p.Key == key {
			out = append(out, p)
		}
	}
	return out
}

func (s *store) rewriteHistory() {
	if s.dir == "" {
		return
	}
	var buf []byte
	for _, p := range s.hist {
		buf = appendHist(buf, p)
	}
	s.fail(writeAtomic(filepath.Join(s.dir, historyLog), buf))
}

func appendHist(buf []byte, p histPoint) []byte {
	line, _ := json.Marshal([]any{p.T, p.Auth, p.Key, p.Used})
	return append(append(buf, line...), '\n')
}

func (s *store) addHistory(points []histPoint) {
	if len(points) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hist = append(s.hist, points...)
	if s.dir == "" {
		return
	}
	var buf []byte
	for _, p := range points {
		buf = appendHist(buf, p)
	}
	f, err := os.OpenFile(filepath.Join(s.dir, historyLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		_, err = f.Write(buf)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	s.fail(err)
}

// series returns one series per (auth_index, window key) for points newer than from; authIndex "" = all.
func (s *store) series(from int64, authIndex string) []series {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := map[[2]string]int{}
	out := []series{}
	for _, p := range s.hist {
		if p.T < from || (authIndex != "" && p.Auth != authIndex) {
			continue
		}
		k := [2]string{p.Auth, p.Key}
		i, ok := idx[k]
		if !ok {
			i = len(out)
			idx[k] = i
			out = append(out, series{AuthIndex: p.Auth, Key: p.Key, Points: [][2]float64{}})
		}
		out[i].Points = append(out[i].Points, [2]float64{float64(p.T), p.Used})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].AuthIndex != out[b].AuthIndex {
			return out[a].AuthIndex < out[b].AuthIndex
		}
		return out[a].Key < out[b].Key
	})
	return out
}

func (s *store) addAlerts(evs []alertEvent) {
	if len(evs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, evs...)
	if n := len(s.alerts) - maxAlerts; n > 0 {
		s.alerts = append([]alertEvent(nil), s.alerts[n:]...)
	}
	s.writeAlerts()
}

func (s *store) writeAlerts() {
	if s.dir == "" {
		return
	}
	raw, err := json.Marshal(s.alerts)
	if err == nil {
		err = writeAtomic(filepath.Join(s.dir, alertsFile), raw)
	}
	s.fail(err)
}

func (s *store) alertsSince(since int64) []alertEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []alertEvent{}
	for _, a := range s.alerts {
		if a.At > since {
			out = append(out, a)
		}
	}
	return out
}

// maskKey shows only first5…last4 of a client key; short keys are fully hidden. Raw keys are never stored.
func maskKey(k string) string {
	if k == "" {
		return ""
	}
	r := []rune(k)
	if len(r) <= 10 {
		return "••••"
	}
	return string(r[:5]) + "…" + string(r[len(r)-4:])
}

// addUsage folds one request's counters d into its hourly bucket; masked must already be masked.
func (s *store) addUsage(hour int64, auth, masked, model string, d usageRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := usageKey{hour, auth, masked, model}
	r := s.usage[k]
	if r == nil {
		r = &usageRow{AuthIndex: auth, ClientKey: masked, Model: model}
		s.usage[k] = r
	}
	r.add(d)
	s.dirty = true
}

// learn records a host auth ID -> auth index pair; true when it is new or changed.
func (s *store) learn(authID, authIndex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[authID] == authIndex {
		return false
	}
	s.ids[authID] = authIndex
	s.dirty = true
	return true
}

func (s *store) index(authID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[authID]
}

func (s *store) idMap() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.ids))
	for k, v := range s.ids {
		out[k] = v
	}
	return out
}

// authTotals sums the buckets overlapping [from, now] per account.
func (s *store) authTotals(from int64) map[string]usageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]usageRow{}
	for k, r := range s.usage {
		if k.Hour+hourMs > from {
			t := out[k.Auth]
			t.add(*r)
			out[k.Auth] = t
		}
	}
	return out
}

// usageRows aggregates buckets overlapping [from, now] by (auth_index, client_key, model).
func (s *store) usageRows(from int64) []usageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	agg := map[usageKey]*usageRow{}
	for k, r := range s.usage {
		if k.Hour+hourMs <= from {
			continue
		}
		g := usageKey{Auth: k.Auth, Client: k.Client, Model: k.Model}
		a := agg[g]
		if a == nil {
			a = &usageRow{AuthIndex: k.Auth, ClientKey: k.Client, Model: k.Model}
			agg[g] = a
		}
		a.add(*r)
	}
	out := make([]usageRow, 0, len(agg))
	for _, a := range agg {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Requests != b.Requests:
			return a.Requests > b.Requests
		case a.AuthIndex != b.AuthIndex:
			return a.AuthIndex < b.AuthIndex
		case a.ClientKey != b.ClientKey:
			return a.ClientKey < b.ClientKey
		}
		return a.Model < b.Model
	})
	return out
}

func (s *store) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
}

func (s *store) flushLocked() {
	if s.dir == "" || !s.dirty {
		return
	}
	u := struct {
		IDs     map[string]string `json:"ids"`
		Buckets []usageBucket     `json:"buckets"`
	}{IDs: s.ids, Buckets: make([]usageBucket, 0, len(s.usage))}
	for k, r := range s.usage {
		u.Buckets = append(u.Buckets, usageBucket{Hour: k.Hour, usageRow: *r})
	}
	sort.Slice(u.Buckets, func(i, j int) bool { return u.Buckets[i].Hour < u.Buckets[j].Hour })
	raw, err := json.Marshal(u)
	if err == nil {
		err = writeAtomic(filepath.Join(s.dir, usageFile), raw)
	}
	s.fail(err)
	if err == nil {
		s.dirty = false
	}
}
