package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceDiscoveryIsBoundedAndDoesNotSpendArchiveQuota(t *testing.T) {
	s, _ := archiveTestService(t)
	c := s.currentContext()
	before := c.store.usedBytes
	for i := range 180 {
		request, _ := json.Marshal(interceptRequest{CallerScope: "unprivileged-caller", Body: []byte(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`), Headers: http.Header{"X-Token-Trim-Workspace": []string{fmt.Sprint("workspace-", i)}}})
		s.intercept(request)
	}
	if c.store.usedBytes != before || len(c.store.namespaces().([]archiveNamespace)) != 0 || len(c.discoveryOrder) > 128 {
		t.Fatal("unauthorized discovery persisted data or grew without bound")
	}
	id := c.discoveryOrder[len(c.discoveryOrder)-1]
	body, _ := json.Marshal(map[string]string{"namespace": id})
	response := s.manageArchive("archive/token", managementRequest{Method: "POST", Body: body})
	if response["StatusCode"] != 200 || len(c.store.namespaces().([]archiveNamespace)) != 1 {
		t.Fatal("admin could not provision discovered workspace")
	}
}

func TestOverQuotaArchiveStillRetrievesAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	a, err := openArchive(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ns := testNamespace(t, a, "caller", "workspace")
	text := strings.Repeat("exact archive ", 1000)
	id := testDocument(t, a, ns, text)
	token, err := a.issueToken(ns)
	if err != nil {
		t.Fatal(err)
	}
	limit := a.usedBytes - 1
	_ = a.close()
	a, err = openArchive(path, limit)
	if err != nil {
		t.Fatal("quota reduction disabled old retrieval", err)
	}
	defer a.close()
	page, err := a.read(ns, id, 0, 0)
	if err != nil || page.Text != text {
		t.Fatal("old output unreachable over quota")
	}
	if _, err = a.put(ns, "tool", "new", "new bytes"); err == nil {
		t.Fatal("over-quota archive accepted growth")
	}
	if err = a.revokeToken(token); err != nil {
		t.Fatal("could not revoke token while over quota", err)
	}
}

func TestArchivedViewsSurviveNewSkipRules(t *testing.T) {
	s, cfg := archiveTestService(t)
	body := withRetrieval(toolBody("claude-x", [2]string{"Bash", strings.Repeat("retain first preview ", 1000)}))
	out := nativeIntercept(t, s, body, "caller", "project")
	if bytes.Equal(out, body) {
		t.Fatal("preview was not created")
	}
	cfg.SkipTools = []string{"Bash"}
	cfg.SkipModels = []string{"claude-x"}
	s.configure(cfg)
	if next := nativeIntercept(t, s, body, "caller", "project"); !bytes.Equal(next, out) {
		t.Fatal("new opt-out changed already-sent preview")
	}
	for _, name := range []string{"read_file", "ReadFile", "functions.read", "mcp__files__get_file"} {
		if archiveEligible(target{tool: name, text: strings.Repeat("unnumbered source text ", 1000)}, cfg) {
			t.Fatalf("source-reading tool %s was eligible for a cap", name)
		}
	}
}

func TestHistoryForkDoesNotOverwriteAnotherBranchesLayout(t *testing.T) {
	s, _ := archiveTestService(t)
	body := longConversation("claude")
	out := nativeIntercept(t, s, body, "caller", "forks")
	variant := bytes.Replace(body, []byte("detail-0"), []byte("branch-0"), 1)
	_ = nativeIntercept(t, s, variant, "caller", "forks")
	key, entries, _ := historyEntries(body)
	entries = append(entries, json.RawMessage(`{"role":"user","content":"next"}`), json.RawMessage(`{"role":"assistant","content":"ack"}`))
	grown, _ := replaceEntries(body, key, entries)
	next := nativeIntercept(t, s, grown, "caller", "forks")
	_, before, _ := historyEntries(out)
	_, after, _ := historyEntries(next)
	if !bytes.Equal(before[1], after[1]) {
		t.Fatal("another branch replaced this branch's stable index")
	}
}

func TestAbortedArchiveRequestDoesNotCountSavingsOrActivateOrphanViews(t *testing.T) {
	s, _ := archiveTestService(t)
	c := s.currentContext()
	_, _ = c.store.namespace("caller", "quota")
	c.store.mu.Lock()
	c.store.maxBytes = c.store.usedBytes + 9000
	c.store.mu.Unlock()
	body := withRetrieval(toolBody("claude-x", [2]string{"Bash", strings.Repeat("first ", 700)}, [2]string{"Bash", strings.Repeat("second ", 2000)}))
	out := nativeIntercept(t, s, body, "caller", "quota")
	if !bytes.Equal(out, body) || c.outputSaved != 0 || c.historySaved != 0 {
		t.Fatal("aborted transformation was reported as savings")
	}
	c.store.mu.Lock()
	c.store.maxBytes = 1 << 30
	c.store.mu.Unlock()
	if next := nativeIntercept(t, s, body, "caller", "quota"); !bytes.Equal(next, body) {
		t.Fatal("uncommitted preview replayed after recovery")
	}
}

func TestHistoryProtectsNonTextInputs(t *testing.T) {
	for _, kind := range []string{"input_file", "file", "document", "input_audio", "video"} {
		entry, _ := json.Marshal(map[string]any{"role": "user", "content": []any{map[string]any{"type": kind, "file_id": "private-input"}}})
		if !containsImage(entry) {
			t.Fatalf("non-text input %s was archivable as plain text", kind)
		}
	}
	// Group-readable pre-existing archive roots remain rejected rather than chmodded silently.
	path := filepath.Join(t.TempDir(), "unsafe")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if a, err := openArchive(path, 1<<20); err == nil {
		a.close()
		t.Fatal("unsafe existing root accepted")
	}
}

func TestNamespacedRetrievalSupportsRecoveryButShellReadsRemainExact(t *testing.T) {
	s, cfg := archiveTestService(t)
	body := toolBody("claude-x", [2]string{"Bash", strings.Repeat("large service result ", 1000)})
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(body, &doc)
	doc["tools"] = json.RawMessage(`[{"type":"namespace","name":"archive","tools":[{"type":"function","name":"token_trim_read"},{"type":"function","name":"token_trim_search"}]}]`)
	body, _ = json.Marshal(doc)
	if out := nativeIntercept(t, s, body, "caller", "namespace"); bytes.Equal(out, body) || !bytes.Contains(out, []byte("token-trim archive")) {
		t.Fatal("namespaced retrieval tools did not permit recoverable preview")
	}
	for _, command := range []string{"cat source.go", "git show HEAD:source.go", "sed -n '1,300p' source.go"} {
		if archiveEligible(target{tool: "Bash", command: command, text: strings.Repeat("unnumbered source ", 1000)}, cfg) {
			t.Fatalf("source read %q eligible for a cap", command)
		}
	}
}

func TestHistoryDoesNotArchiveOptedOutTools(t *testing.T) {
	s, _ := archiveTestService(t)
	s.mu.Lock()
	s.cfg.SkipTools = []string{"SecretTool"}
	s.mu.Unlock()
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(withRetrieval(toolBody("claude-x", [2]string{"SecretTool", "private output remains in view"})), &doc)
	var messages []json.RawMessage
	_ = json.Unmarshal(doc["messages"], &messages)
	for i := range 20 {
		message, _ := json.Marshal(map[string]any{"role": "user", "content": fmt.Sprint("next task ", i)})
		answer, _ := json.Marshal(map[string]any{"role": "assistant", "content": strings.Repeat("large historical detail ", 150)})
		messages = append(messages, message, answer)
	}
	doc["messages"], _ = json.Marshal(messages)
	body, _ := json.Marshal(doc)
	if out := nativeIntercept(t, s, body, "caller", "protected"); !bytes.Equal(out, body) {
		t.Fatal("history hid an opted-out tool's result")
	}
}
