package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func archiveTestService(t *testing.T) (*service, settings) {
	t.Helper()
	cfg := defaultSettings()
	cfg.Mode = modeApply
	cfg.ArchiveEnabled = true
	cfg.ArchiveListen = "127.0.0.1:0"
	cfg.DataDir = t.TempDir()
	cfg.OutputLimit = 2048
	cfg.HistoryBudget = 16000
	cfg.HistoryRecent = 4000
	s := newService()
	s.configure(cfg)
	t.Cleanup(s.stop)
	if s.currentContext() == nil {
		t.Fatalf("archive startup: %s", s.err)
	}
	return s, cfg
}

func withRetrieval(body []byte) []byte {
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(body, &doc)
	doc["tools"] = json.RawMessage(`[{"name":"mcp__token_trim__token_trim_read","input_schema":{"type":"object"}},{"name":"mcp__token_trim__token_trim_search","input_schema":{"type":"object"}}]`)
	out, _ := json.Marshal(doc)
	return out
}

func nativeIntercept(t *testing.T, s *service, body []byte, caller, workspace string) []byte {
	t.Helper()
	headers := http.Header{"X-Token-Trim-Workspace": []string{workspace}}
	if c := s.currentContext(); c != nil && caller != "" {
		namespace, err := c.store.namespace(caller, workspace)
		if err != nil {
			t.Fatal(err)
		}
		token, err := c.store.issueToken(namespace)
		if err != nil {
			t.Fatal(err)
		}
		headers.Set("X-Token-Trim-Archive-Token", token)
	}
	raw, _ := json.Marshal(interceptRequest{Body: body, CallerScope: caller, Headers: headers})
	response := s.intercept(raw)
	if out, ok := response["Body"].([]byte); ok {
		return out
	}
	if edits, ok := response["body_edits"].([]edit); ok {
		return applyEdits(body, edits)
	}
	return body
}

func TestArchivedOutputRetrievesExactMiddleAndKeepsViewAfterRestart(t *testing.T) {
	s, cfg := archiveTestService(t)
	text := strings.Repeat("before αβγ ", 500) + "HIDDEN-MIDDLE-42" + strings.Repeat(" after δζη", 500)
	body := withRetrieval(toolBody("claude-x", [2]string{"Bash", text}))
	out := nativeIntercept(t, s, body, "caller-a", "billing")
	if bytes.Equal(out, body) || strings.Contains(string(out), "HIDDEN-MIDDLE-42") || !strings.Contains(string(out), "token-trim archive") {
		t.Fatalf("missing recoverable preview: %s", out)
	}
	c := s.currentContext()
	ns, _ := c.store.namespace("caller-a", "billing")
	id := sha256.Sum256([]byte(text))
	var retrieved strings.Builder
	offset := 0
	for {
		page, err := c.store.read(ns, hex.EncodeToString(id[:]), offset, 1024)
		if err != nil {
			t.Fatal(err)
		}
		retrieved.WriteString(page.Text)
		if page.NextOffset == nil {
			break
		}
		offset = *page.NextOffset
	}
	if retrieved.String() != text {
		t.Fatal("archive did not retain the omitted middle exactly")
	}
	// Two callers with identical output and call IDs still have independent archives/pins.
	other := nativeIntercept(t, s, body, "caller-b", "billing")
	if bytes.Equal(other, body) {
		t.Fatal("another caller's pin suppressed archiving")
	}
	otherNS, _ := c.store.namespace("caller-b", "billing")
	if _, err := c.store.read(otherNS, hex.EncodeToString(id[:]), 0, 1024); err != nil {
		t.Fatal(err)
	}
	s.stop()
	s = newService()
	cfg.OutputLimit = 8192
	cfg.Tests = false
	s.configure(cfg)
	t.Cleanup(s.stop)
	if again := nativeIntercept(t, s, body, "caller-a", "billing"); !bytes.Equal(again, out) {
		t.Fatal("restart/settings change rewrote a previously sent archive view")
	}
}

func TestArchiveOptInAndWriteFailureNeverCreateMissingPointers(t *testing.T) {
	s, _ := archiveTestService(t)
	text := strings.Repeat("plain output ", 1000)
	body := toolBody("claude-x", [2]string{"Bash", text})
	if out := nativeIntercept(t, s, body, "caller-a", "one"); !bytes.Equal(body, out) {
		t.Fatal("shortened without retrieval tools")
	}
	body = withRetrieval(toolBody("claude-x", [2]string{"Bash", text + "new"}))
	if out := nativeIntercept(t, s, body, "", "one"); !bytes.Equal(body, out) {
		t.Fatal("shortened without authenticated identity")
	}
	c := s.currentContext()
	_, _ = c.store.namespace("caller-b", "two")
	c.store.mu.Lock()
	c.store.maxBytes = c.store.usedBytes + 4096
	c.store.mu.Unlock()
	if out := nativeIntercept(t, s, body, "caller-b", "two"); !bytes.Equal(body, out) {
		t.Fatal("archive failure sent shortened content")
	}
	// An orphaned view from an interrupted attempt must not replace an output pinned raw.
	c.store.mu.Lock()
	c.store.maxBytes = 1 << 30
	c.store.mu.Unlock()
	if out := nativeIntercept(t, s, body, "caller-b", "two"); !bytes.Equal(body, out) {
		t.Fatal("failed first sight changed on a later turn")
	}
}

func TestArchiveProtectsReadsPatchesAndErrorResults(t *testing.T) {
	s, _ := archiveTestService(t)
	for i, call := range [][2]string{{"Read", strings.Repeat("source contents ", 1000)}, {"apply_patch", "*** Begin Patch\n" + strings.Repeat("+code\n", 1000)}, {"Bash", "error: exact failing evidence\n" + strings.Repeat("trace ", 1000)}} {
		body := withRetrieval(toolBody("claude-x", call))
		out := nativeIntercept(t, s, body, fmt.Sprint("caller", i), "safety")
		if strings.Contains(string(out), "token-trim archive") {
			t.Fatal("capped protected output")
		}
	}
}

func longConversation(format string) []byte {
	entries := []any{map[string]any{"role": "user", "content": "Keep refund window at 14 days"}, map[string]any{"role": "assistant", "content": "ack"}}
	for i := range 30 {
		entries = append(entries, map[string]any{"role": "user", "content": fmt.Sprintf("work item %d", i)}, map[string]any{"role": "assistant", "content": fmt.Sprintf("detail-%d ", i) + strings.Repeat("historical detail ", 160)})
	}
	doc := map[string]any{"model": "model-x", "tools": []any{map[string]any{"name": "token_trim_read"}, map[string]any{"name": "token_trim_search"}}}
	if format == "responses" {
		doc["input"] = entries
		doc["instructions"] = "unchanged provider instructions"
	} else if format == "chat" {
		doc["messages"] = append([]any{map[string]any{"role": "system", "content": "unchanged provider system"}}, entries...)
	} else {
		doc["messages"] = entries
		doc["system"] = "unchanged provider system"
	}
	body, _ := json.Marshal(doc)
	return body
}

func TestHistoryKeepsRecentEntriesAndStableLayoutAcrossFormats(t *testing.T) {
	for _, format := range []string{"claude", "chat", "responses"} {
		t.Run(format, func(t *testing.T) {
			s, cfg := archiveTestService(t)
			body := longConversation(format)
			out := nativeIntercept(t, s, body, "caller-a", format)
			if len(out) >= len(body)/2 || !strings.Contains(string(out), "historical conversation index") {
				t.Fatalf("history did not shrink: %d -> %d", len(body), len(out))
			}
			key, before, _ := historyEntries(body)
			_, after, _ := historyEntries(out)
			first := 0
			if format == "chat" {
				first = 1
			}
			for i := range first + 1 {
				if !bytes.Equal(before[i], after[i]) {
					t.Fatal("system or first user changed")
				}
			}
			for i := 1; i <= 8; i++ {
				if !bytes.Equal(before[len(before)-i], after[len(after)-i]) {
					t.Fatal("recent entry changed")
				}
			}
			var src, dst map[string]json.RawMessage
			_ = json.Unmarshal(body, &src)
			_ = json.Unmarshal(out, &dst)
			for field, value := range src {
				if field != key && !bytes.Equal(value, dst[field]) {
					t.Fatalf("changed top-level %s", field)
				}
			}
			// The layout boundary holds while new work remains inside the byte budget.
			before = append(before, json.RawMessage(`{"role":"user","content":"continue"}`), json.RawMessage(`{"role":"assistant","content":"done"}`))
			grown, _ := replaceEntries(body, key, before)
			again := nativeIntercept(t, s, grown, "caller-a", format)
			_, grownAfter, _ := historyEntries(again)
			if !bytes.Equal(after[first+1], grownAfter[first+1]) {
				t.Fatal("index changed on a warm append")
			}
			s.stop()
			s = newService()
			s.configure(cfg)
			t.Cleanup(s.stop)
			if restarted := nativeIntercept(t, s, grown, "caller-a", format); !bytes.Equal(restarted, again) {
				t.Fatal("history boundary lost on restart")
			}
		})
	}
}

func TestHistoryNeverSplitsToolPairsOrDropsVisualContent(t *testing.T) {
	entries := []json.RawMessage{json.RawMessage(`{"role":"user","content":"first"}`), json.RawMessage(`{"role":"assistant","content":[{"type":"thinking","thinking":"exact thought","signature":"signed-value"},{"type":"tool_use","id":"call-a","name":"Read","input":{"file_path":"a.go"}}]}`), json.RawMessage(`{"role":"user","content":"interleaved user"}`), json.RawMessage(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-a","content":"source"}]}`)}
	if safeHistoryCut(entries, 1, 2) {
		t.Fatal("split an outstanding tool call")
	}
	if !safeHistoryCut(entries, 1, 4) {
		t.Fatal("rejected complete signed tool group")
	}
	entries[3] = json.RawMessage(`{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"AA=="}}]}`)
	if safeHistoryCut(entries, 1, 4) {
		t.Fatal("archived unrecoverable visual content")
	}
	body := []byte(`{"model":"m","previous_response_id":"resp-existing","input":[{"role":"user","content":"new"}]}`)
	if key, _, _ := historyEntries(body); key != "" {
		t.Fatal("rewrote a server-side continuation")
	}
}
