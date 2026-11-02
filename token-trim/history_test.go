package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAPIArchiveTokenMustMatchAuthenticatedWorkspaceAndNeverGoesUpstream(t *testing.T) {
	s, _ := archiveTestService(t)
	body := withRetrieval(toolBody("claude-x", [2]string{"Bash", strings.Repeat("unstructured evidence ", 1000)}))
	c := s.currentContext()
	wrong, _ := c.store.namespace("different-caller", "billing")
	token, _ := c.store.issueToken(wrong)
	for _, bearer := range []string{"", token} {
		raw, _ := json.Marshal(interceptRequest{Body: body, CallerScope: "actual-caller", Headers: http.Header{"X-Token-Trim-Workspace": []string{"billing"}, "X-Token-Trim-Archive-Token": []string{bearer}}})
		out := s.intercept(raw)
		if _, ok := out["Body"]; ok {
			t.Fatal("archived with absent or another caller's token")
		}
		cleared, _ := out["ClearHeaders"].([]string)
		if len(cleared) != 2 || cleared[0] != "X-Token-Trim-Archive-Token" {
			t.Fatal("archive token could reach model provider")
		}
	}
}

func TestHistoryIndexFlagsLaterFileChangesAndArchivesSignedBytes(t *testing.T) {
	s, _ := archiveTestService(t)
	c := s.currentContext()
	ns, _ := c.store.namespace("caller", "repo")
	entries := []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"first"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"exact thought","signature":"opaque-signature"},{"type":"tool_use","id":"read-a","name":"Read","input":{"file_path":"a.go"}}]}`),
		json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-a","content":"old contents"}]}`),
		json.RawMessage(`{"role":"user","content":"continue"}`),
		json.RawMessage(`{"role":"assistant","content":[{"type":"tool_use","id":"edit-a","name":"Edit","input":{"file_path":"a.go"}}]}`),
	}
	index, err := c.historyIndex(ns, entries, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "file may have changed later") {
		t.Fatal("old file read lacked stale warning")
	}
	id, err := c.store.put(ns, "message", "assistant", string(entries[1]))
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.store.read(ns, id, 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal([]byte(page.Text), entries[1]) {
		t.Fatal("signed thinking bytes were modified")
	}
}
