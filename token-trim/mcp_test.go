package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func archiveMCPRequest(a *archiveStore, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8319/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.serveMCP(w, req)
	return w
}

func archiveMCPResult(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["jsonrpc"] != "2.0" {
		t.Fatalf("not JSON-RPC: %v", response)
	}
	return response
}

func TestArchiveMCPInitializeListAndTools(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "tenant-one", "workspace")
	token, err := a.issueToken(ns)
	if err != nil {
		t.Fatal(err)
	}
	raw := "raw\r\nUnicode 界🌎\n"
	id := testDocument(t, a, ns, raw)
	init := archiveMCPResult(t, archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if init["result"].(map[string]any)["protocolVersion"] != "2025-03-26" {
		t.Fatal("version negotiation failed")
	}
	w := archiveMCPRequest(a, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if w.Code != http.StatusAccepted || w.Body.Len() != 0 {
		t.Fatalf("notification response = %d %s", w.Code, w.Body.String())
	}
	listed := archiveMCPResult(t, archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`))
	tools := listed["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tool count = %d", len(tools))
	}
	for _, tool := range tools {
		object := tool.(map[string]any)
		if object["inputSchema"].(map[string]any)["additionalProperties"] != false {
			t.Fatal("tool schema accepts namespace injection")
		}
	}
	readBody := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"token_trim_read","arguments":{"id":"` + id + `"}}}`
	read := archiveMCPResult(t, archiveMCPRequest(a, token, readBody))["result"].(map[string]any)
	if read["isError"] != false {
		t.Fatalf("read = %v", read)
	}
	structured := read["structuredContent"].(map[string]any)
	if structured["data"].(map[string]any)["text"] != raw {
		t.Fatal("MCP changed exact document bytes")
	}
	if structured["provenance"].(map[string]any)["current_disk_state_verified"] != false {
		t.Fatal("snapshot metadata missing")
	}
	search := archiveMCPResult(t, archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"token_trim_search","arguments":{"query":"Unicode"}}}`))["result"].(map[string]any)
	matches := search["structuredContent"].(map[string]any)["data"].(map[string]any)["matches"].([]any)
	if len(matches) != 1 || matches[0].(map[string]any)["id"] != id {
		t.Fatalf("search = %v", search)
	}
}

func TestArchiveMCPAuthenticationAndNamespaceIsolation(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "one", "work")
	other := testNamespace(t, a, "two", "work")
	id := testDocument(t, a, other, "secret belonging to tenant two")
	token, err := a.issueToken(ns)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, bad := range []string{"", "tt_" + strings.Repeat("0", 64), "../state.json"} {
		if w := archiveMCPRequest(a, bad, body); w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid token returned %d", w.Code)
		}
	}
	for _, args := range []string{`{"id":"` + id + `"}`, `{"id":"` + id + `","namespace":"` + other + `"}`, `{"id":"../state.json"}`} {
		w := archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"token_trim_read","arguments":`+args+`}}`)
		result := archiveMCPResult(t, w)["result"].(map[string]any)
		if result["isError"] != true || strings.Contains(w.Body.String(), "tenant two") {
			t.Fatalf("cross-namespace access: %s", w.Body.String())
		}
	}
	w := archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"token_trim_search","arguments":{"query":"secret","namespace":"`+other+`"}}}`)
	if result := archiveMCPResult(t, w)["result"].(map[string]any); result["isError"] != true {
		t.Fatal("search namespace injection accepted")
	}
	if err = a.revokeToken(token); err != nil {
		t.Fatal(err)
	}
	if w = archiveMCPRequest(a, token, body); w.Code != http.StatusUnauthorized {
		t.Fatal("revoked token can use MCP")
	}
}

func TestArchiveMCPTransportValidation(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "caller", "work")
	token, err := a.issueToken(ns)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for _, c := range []struct {
		name, method, origin, contentType, version, body string
		status                                           int
	}{
		{"get", http.MethodGet, "", "application/json", "", body, http.StatusMethodNotAllowed},
		{"origin", http.MethodPost, "https://evil.example", "application/json", "", body, http.StatusForbidden},
		{"same-origin-browser", http.MethodPost, "http://127.0.0.1:8319", "application/json", "", body, http.StatusForbidden},
		{"content-type", http.MethodPost, "", "text/plain", "", body, http.StatusUnsupportedMediaType},
		{"version", http.MethodPost, "", "application/json", "not-supported", body, http.StatusBadRequest},
		{"large", http.MethodPost, "", "application/json", "", strings.Repeat(" ", archiveMCPBodyLimit+1), http.StatusRequestEntityTooLarge},
		{"parse", http.MethodPost, "", "application/json", "", "{", http.StatusBadRequest},
		{"batch", http.MethodPost, "", "application/json", "", "[]", http.StatusBadRequest},
		{"invalid-id", http.MethodPost, "", "application/json", "", `{"jsonrpc":"2.0","id":{},"method":"ping"}`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "http://127.0.0.1:8319/mcp", strings.NewReader(c.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", c.contentType)
			if c.origin != "" {
				req.Header.Set("Origin", c.origin)
			}
			if c.version != "" {
				req.Header.Set("MCP-Protocol-Version", c.version)
			}
			w := httptest.NewRecorder()
			a.handler().ServeHTTP(w, req)
			if w.Code != c.status {
				t.Fatalf("status = %d, expected %d: %s", w.Code, c.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("archive response may be cached")
			}
		})
	}
	for _, c := range []struct {
		method string
		code   float64
	}{{"unknown", -32601}, {"tools/call", -32602}} {
		response := archiveMCPResult(t, archiveMCPRequest(a, token, `{"jsonrpc":"2.0","id":1,"method":"`+c.method+`"}`))
		if response["error"].(map[string]any)["code"] != c.code {
			t.Fatalf("wrong JSON-RPC error: %v", response)
		}
	}
}
