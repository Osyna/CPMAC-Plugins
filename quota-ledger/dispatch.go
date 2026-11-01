package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type managementRequest struct {
	Method string
	Path   string
	Query  map[string][]string
	Body   []byte
}

// logoURI is the sidebar/plugin-list icon: a quota ring on a Claude→Codex gradient badge. CPAMC shows
// it in an <img> at 16 px, so it carries its own colors and reads on both light and dark themes.
var logoURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">`+
	`<defs><linearGradient id="g" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#E07A55"/><stop offset="1" stop-color="#5B5FE9"/></linearGradient></defs>`+
	`<rect width="24" height="24" rx="6" fill="url(#g)"/>`+
	`<circle cx="12" cy="12" r="6.5" fill="none" stroke="#fff" stroke-opacity=".35" stroke-width="2.6"/>`+
	`<circle cx="12" cy="12" r="6.5" fill="none" stroke="#fff" stroke-width="2.6" stroke-linecap="round" stroke-dasharray="29 41" transform="rotate(-90 12 12)"/>`+
	`<circle cx="12" cy="12" r="1.7" fill="#fff"/></svg>`))

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var lc struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if len(req) > 0 {
			if err := json.Unmarshal(req, &lc); err != nil {
				return nil, err
			}
		}
		cfg, err := parseSettings(lc.ConfigYAML)
		if err != nil {
			return nil, err
		}
		svc.configure(cfg)
		return okEnvelope(map[string]any{
			"schema_version": schemaVersion,
			"metadata": map[string]any{
				"Name":    "quota-ledger",
				"Version": version,
				"Author":  "NewWay",
				// Required non-empty by the host; display-only (updates come from the plugin store registry).
				"GitHubRepository": "local",
				"Logo":             logoURI,
				"ConfigFields":     configFields(),
			},
			"capabilities": map[string]bool{"management_api": true, "scheduler": true, "usage_plugin": true},
		})
	case "plugin.quiesce", "plugin.shutdown":
		svc.stop()
		return okEnvelope(map[string]any{})
	case "management.register":
		return okEnvelope(map[string]any{
			"routes": []map[string]string{
				{"Method": "GET", "Path": "quota-ledger/state"},
				{"Method": "POST", "Path": "quota-ledger/refresh"},
				{"Method": "GET", "Path": "quota-ledger/history"},
				{"Method": "GET", "Path": "quota-ledger/usage"},
				{"Method": "GET", "Path": "quota-ledger/alerts"},
				{"Method": "POST", "Path": "quota-ledger/alerts/test"},
				{"Method": "POST", "Path": "quota-ledger/alerts/dismiss"},
				{"Method": "POST", "Path": "quota-ledger/mute"},
				{"Method": "GET", "Path": "quota-ledger/waste"},
				{"Method": "GET", "Path": "quota-ledger/digest"},
				{"Method": "POST", "Path": "quota-ledger/digest"},
				{"Method": "GET", "Path": "quota-ledger/bar"},
				{"Method": "GET", "Path": "quota-ledger/metrics"},
			},
			"resources": []map[string]string{{
				"Path":        "/ledger",
				"Menu":        "Quota Ledger",
				"Description": "Quota ledger for Claude, Codex and OpenCode Go credentials.",
			}},
		})
	case "management.handle":
		var r managementRequest
		if err := json.Unmarshal(req, &r); err != nil {
			return nil, err
		}
		return okEnvelope(svc.handleManagement(r))
	case "scheduler.pick":
		return okEnvelope(svc.pick(req))
	case "usage.handle":
		svc.usage(req)
		return okEnvelope(map[string]any{})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func okEnvelope(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func jsonResponse(status int, v any) obj {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"encode failed"}`)
	}
	return obj{
		"StatusCode": status,
		"Headers":    map[string][]string{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		"Body":       body,
	}
}

func errorResponse(status int, msg string) obj { return jsonResponse(status, obj{"error": msg}) }

func textResponse(ctype string, body []byte) obj {
	return obj{"StatusCode": http.StatusOK, "Headers": map[string][]string{"Content-Type": {ctype}, "Cache-Control": {"no-store"}}, "Body": body}
}

func queryInt(q map[string][]string, name string, def, lo, hi int64) int64 {
	if v := q[name]; len(v) > 0 {
		if n, err := strconv.ParseInt(v[0], 10, 64); err == nil {
			def = n
		}
	}
	return min(max(def, lo), hi)
}

func (s *service) handleManagement(r managementRequest) obj {
	// Management routes live under /v0/management/quota-ledger/<route>; the page is the /ledger resource.
	i := strings.LastIndex(r.Path, "/quota-ledger/")
	if i < 0 {
		return errorResponse(http.StatusNotFound, "not found")
	}
	route := r.Path[i+len("/quota-ledger/"):]
	if route == "ledger" && r.Method == http.MethodGet {
		return obj{
			"StatusCode": 200,
			"Headers":    map[string][]string{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}},
			"Body":       page,
		}
	}
	want := http.MethodGet
	switch route {
	case "refresh", "alerts/test", "alerts/dismiss", "mute":
		want = http.MethodPost
	case "digest":
		if r.Method == http.MethodPost {
			want = r.Method
		}
	case "state", "history", "usage", "alerts", "waste", "bar", "metrics":
	default:
		return errorResponse(http.StatusNotFound, "not found")
	}
	if r.Method != want {
		return errorResponse(http.StatusMethodNotAllowed, "method not allowed")
	}
	s.mu.RLock()
	cfg, st := s.cfg, s.store
	s.mu.RUnlock()
	now := nowMs()
	switch route {
	case "state":
		return jsonResponse(http.StatusOK, s.state())
	case "refresh":
		var in struct {
			AuthIndex string `json:"auth_index"`
		}
		if len(r.Body) > 0 && json.Unmarshal(r.Body, &in) != nil {
			return errorResponse(http.StatusBadRequest, "invalid JSON body")
		}
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		switch err := s.poll(ctx, in.AuthIndex, manualMinAge); err {
		case nil:
		case errUnknownAuthIndex:
			return errorResponse(http.StatusNotFound, err.Error())
		default:
			return errorResponse(http.StatusGatewayTimeout, err.Error())
		}
		return jsonResponse(http.StatusOK, s.state())
	case "history":
		hours := queryInt(r.Query, "hours", 168, 1, int64(cfg.HistoryDays)*24)
		idx := ""
		if v := r.Query["auth_index"]; len(v) > 0 {
			idx = v[0]
		}
		return jsonResponse(http.StatusOK, obj{"series": st.series(now-hours*hourMs, idx)})
	case "usage":
		hours := queryInt(r.Query, "hours", 24, 1, int64(cfg.HistoryDays)*24)
		return jsonResponse(http.StatusOK, obj{"from": now - hours*hourMs, "to": now, "rows": st.usageRows(now - hours*hourMs)})
	case "alerts":
		return jsonResponse(http.StatusOK, obj{"events": st.alertsSince(queryInt(r.Query, "since", 0, 0, 1<<62))})
	case "alerts/dismiss":
		var in struct {
			All bool `json:"all"`
			alertEvent
		}
		if json.Unmarshal(r.Body, &in) != nil || !in.All && in.At == 0 {
			return errorResponse(http.StatusBadRequest, `body must be {"all":true} or an alert's at/kind/auth_index/window`)
		}
		return jsonResponse(http.StatusOK, obj{"removed": st.dismissAlerts(in.All, in.alertEvent)})
	case "mute":
		var in struct {
			AuthIndex string `json:"auth_index"`
			Muted     bool   `json:"muted"`
		}
		if json.Unmarshal(r.Body, &in) != nil || in.AuthIndex == "" {
			return errorResponse(http.StatusBadRequest, `body must be {"auth_index":"…","muted":true|false}`)
		}
		st.setMuted(in.AuthIndex, in.Muted)
		return jsonResponse(http.StatusOK, s.state())
	case "waste":
		days := queryInt(r.Query, "days", 56, 1, wasteDays)
		return jsonResponse(http.StatusOK, obj{"events": st.wasteSince(now - days*24*hourMs)})
	case "digest":
		if r.Method == http.MethodGet {
			return jsonResponse(http.StatusOK, obj{"text": s.digestText()})
		}
		if err := s.sendDigest(context.Background(), cfg); err != nil {
			return jsonResponse(http.StatusOK, obj{"ok": false, "error": err.Error()})
		}
		return jsonResponse(http.StatusOK, obj{"ok": true})
	case "bar":
		out := barOutput(s.state().Credentials, cfg.AlertBelow, now)
		if v := r.Query["format"]; len(v) > 0 && v[0] == "text" {
			return textResponse("text/plain; charset=utf-8", []byte(out["text"].(string)+"\n"))
		}
		return jsonResponse(http.StatusOK, out)
	case "metrics":
		return textResponse("text/plain; version=0.0.4; charset=utf-8", []byte(metricsText(s.state(), st.wasteSince(0), now)))
	}
	// alerts/test: not recorded in the ring buffer.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	test := alertEvent{At: now, Kind: "test", Name: "quota-ledger", Message: "Test notification"}
	if err := s.sendWebhook(ctx, cfg, test); err != nil {
		return jsonResponse(http.StatusOK, obj{"ok": false, "error": err.Error()})
	}
	return jsonResponse(http.StatusOK, obj{"ok": true})
}
