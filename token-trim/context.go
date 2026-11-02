package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The host, not request content, supplies CallerScope after frontend authentication.
type interceptRequest struct {
	Body        []byte
	CallerScope string
	Headers     http.Header
	Metadata    map[string]any
}

type contextService struct {
	store          *archiveStore
	server         *http.Server
	done           chan struct{}
	mu             sync.Mutex // serialises durable view/layout decisions, never upstream requests
	outputSaved    int64
	historySaved   int64
	requests       int64
	discovered     map[string]discoveredWorkspace
	discoveryOrder []string
}

type discoveredWorkspace struct{ caller, workspace string }

func (c *contextService) discover(caller, workspace string) (string, error) {
	namespace, err := archiveNamespaceID(caller, workspace)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.discovered == nil {
		c.discovered = map[string]discoveredWorkspace{}
	}
	if _, ok := c.discovered[namespace]; !ok {
		if len(c.discoveryOrder) == 128 {
			delete(c.discovered, c.discoveryOrder[0])
			c.discoveryOrder = c.discoveryOrder[1:]
		}
		c.discovered[namespace] = discoveredWorkspace{caller, workspace}
		c.discoveryOrder = append(c.discoveryOrder, namespace)
	}
	return namespace, nil
}

func (c *contextService) namespaceList() []archiveNamespace {
	list := c.store.namespaces().([]archiveNamespace)
	seen := map[string]bool{}
	for _, n := range list {
		seen[n.Namespace] = true
	}
	for _, id := range c.discoveryOrder {
		if !seen[id] {
			list = append(list, archiveNamespace{Namespace: id, Workspace: c.discovered[id].workspace})
		}
	}
	return list
}

func (s *service) startContext(cfg settings) error {
	if !cfg.ArchiveEnabled {
		return nil
	}
	a, err := openArchive(filepath.Join(cfg.DataDir, "archive"), cfg.ArchiveMaxBytes)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.ArchiveListen)
	if err != nil {
		_ = a.close()
		return err
	}
	c := &contextService{store: a, done: make(chan struct{})}
	c.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		a.serveMCP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16384}
	s.contextMu.Lock()
	s.context = c
	s.contextMu.Unlock()
	go func() { defer close(c.done); _ = c.server.Serve(listener) }()
	return nil
}

func (s *service) stopContext() {
	s.contextMu.Lock()
	c := s.context
	s.context = nil
	s.contextMu.Unlock()
	if c != nil {
		_ = c.server.Close()
		<-c.done
		c.mu.Lock()
		_ = c.store.close()
		c.mu.Unlock()
	}
}

func (s *service) currentContext() *contextService {
	s.contextMu.Lock()
	defer s.contextMu.Unlock()
	return s.context
}

func retrievalTools(body []byte) bool {
	type toolDefinition struct {
		Name     string
		Function struct{ Name string }
		Tools    []toolDefinition
	}
	var doc struct{ Tools []toolDefinition }
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	read, search := false, false
	var visit func([]toolDefinition)
	visit = func(tools []toolDefinition) {
		for _, t := range tools {
			name := firstNonEmpty(t.Name, t.Function.Name)
			read = read || name == "token_trim_read" || strings.HasSuffix(name, "__token_trim_read")
			search = search || name == "token_trim_search" || strings.HasSuffix(name, "__token_trim_search")
			visit(t.Tools)
		}
	}
	visit(doc.Tools)
	return read && search
}

func (s *service) intercept(raw []byte) (out obj) {
	defer func() {
		if recover() != nil {
			out = obj{}
		}
		if out == nil {
			out = obj{}
		}
		out["ClearHeaders"] = []string{"X-Token-Trim-Archive-Token", "X-Token-Trim-Workspace"}
	}()
	var r interceptRequest
	if json.Unmarshal(raw, &r) != nil || len(r.Body) == 0 {
		return obj{}
	}
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	if cfg.Mode == modeOff {
		return obj{}
	}
	c := s.currentContext()
	if c == nil || r.CallerScope == "" {
		return patchResponse(s.patch(r.Body))
	}
	workspace := r.Headers.Get("X-Token-Trim-Workspace")
	if workspace == "" {
		workspace = "default"
	}
	namespace, err := c.discover(r.CallerScope, workspace)
	tokenNamespace, authorized := c.store.tokenNamespace(r.Headers.Get("X-Token-Trim-Archive-Token"))
	if err != nil || !authorized || tokenNamespace != namespace || !retrievalTools(r.Body) {
		return patchResponse(s.patch(r.Body))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	res, err := trim(r.Body, cfg.rules(), s, scopedPins{s, namespace})
	if err != nil || cfg.Mode != modeApply {
		scopeFresh(namespace, &res)
		s.record(res, err, false)
		return obj{}
	}
	body, err := c.reduceScopedOutputs(namespace, r.Body, res, cfg, s)
	outputBytes := max(len(r.Body)-len(body), 0)
	beforeHistory := len(body)
	if err == nil {
		body, err = c.fitHistory(namespace, r.Body, body, cfg, r)
	}
	if err != nil {
		res.Unsafe = "archive unavailable; request left unchanged"
		res.Edits, res.Saved, res.Trimmed = nil, 0, 0
		clear(res.Rules)
		for i := range res.fresh {
			res.fresh[i].form = formRaw
		}
		scopeFresh(namespace, &res)
		s.record(res, nil, true)
		return obj{}
	}
	scopeFresh(namespace, &res)
	s.record(res, nil, true)
	c.requests++
	c.outputSaved += int64(outputBytes)
	c.historySaved += int64(max(beforeHistory-len(body), 0))
	if bytes.Equal(body, r.Body) {
		return obj{}
	}
	return obj{"Body": body}
}

func modelOf(body []byte) string {
	var d struct{ Model string }
	_ = json.Unmarshal(body, &d)
	return d.Model
}
func patchResponse(v obj) obj {
	if edits, ok := v["edits"]; ok {
		return obj{"body_edits": edits}
	}
	return obj{}
}

type scopedPins struct {
	service   *service
	namespace string
}

func scopedPinKey(namespace string, key [32]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(namespace))
	h.Write(key[:])
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func (p scopedPins) pinned(key [32]byte) (form, bool) {
	if f, ok := p.service.pinned(scopedPinKey(p.namespace, key)); ok {
		return f, true
	}
	return p.service.pinned(key) // preserve first-sent forms from before archive integration
}

func scopeFresh(namespace string, res *result) {
	for i := range res.fresh {
		res.fresh[i].key = scopedPinKey(namespace, res.fresh[i].key)
	}
}

func applyEdits(body []byte, edits []edit) []byte {
	if len(edits) == 0 {
		return body
	}
	var b bytes.Buffer
	b.Grow(len(body))
	prev := 0
	for _, e := range edits {
		b.Write(body[prev:e.Start])
		b.WriteString(e.Replacement)
		prev = e.End
	}
	b.Write(body[prev:])
	return b.Bytes()
}

type outputView struct {
	Text string `json:"text"`
}

const formArchived form = 1 << 14

func (c *contextService) reduceScopedOutputs(namespace string, original []byte, res result, cfg settings, s *service) ([]byte, error) {
	body := applyEdits(original, res.Edits)
	_, _, before, err := locate(original)
	if err != nil {
		return nil, err
	}
	_, _, after, err := locate(body)
	if err != nil || len(before) != len(after) {
		return nil, errors.New("output shape changed")
	}
	var edits []edit
	for i, t := range before {
		key := pinKey(sha256.Sum256([]byte(t.text)), t.id)
		pinned, known := (scopedPins{s, namespace}).pinned(key)
		if known && pinned&formArchived == 0 {
			continue
		}
		stateKey := "view-" + hex.EncodeToString(key[:])
		stored, err := c.store.loadState(namespace, stateKey)
		out := after[i].text
		if err == nil {
			var view outputView
			if json.Unmarshal(stored, &view) != nil {
				return nil, errors.New("invalid saved output view")
			}
			out = view.Text
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		} else {
			if known {
				return nil, errors.New("pinned archive view missing")
			}
			if !archiveEligible(t, cfg) || cfg.rules().SkipModels.match(res.Model) {
				continue
			}
			if len(out) > cfg.OutputLimit {
				out = outputPreview(out, cfg.OutputLimit)
			}
			if out == t.text || len(t.text)-len(out) < 768 {
				continue
			}
			id, err := c.store.put(namespace, "tool", t.tool, t.text)
			if err != nil {
				return nil, err
			}
			out += fmt.Sprintf("\n[token-trim archive %s; retrieve with token_trim_read {\"id\":\"%s\"}; historical output, do not rerun just to read it]", id, id)
			if len(out) >= len(t.text) {
				continue
			}
			raw, err := json.Marshal(outputView{Text: out})
			if err != nil {
				return nil, err
			}
			if err = c.store.saveState(namespace, stateKey, raw); err != nil {
				return nil, err
			}
		}
		for j := range res.fresh {
			if res.fresh[j].key == key {
				res.fresh[j].form |= formArchived
			}
		}
		if out != after[i].text {
			edits = append(edits, edit{Start: after[i].start, End: after[i].end, Replacement: encodeString(out)})
		}
	}
	if err := verifyEdits(body, edits); err != nil {
		return nil, err
	}
	return applyEdits(body, edits), nil
}

func archiveEligible(t target, cfg settings) bool {
	if t.tool == "" || protectedFoldContext(t) || cfg.rules().SkipTools.match(t.tool) {
		return false
	}
	name := strings.ToLower(t.tool)
	for _, p := range []string{"read", "file", "edit", "write", "patch", "token_trim_search"} {
		if strings.Contains(name, p) {
			return false
		}
	}
	// Unstructured shell errors may lack protocol error metadata.
	for _, line := range strings.Split(t.text, "\n") {
		v := strings.ToLower(strings.TrimSpace(line))
		if strings.Contains(v, "error:") || strings.HasPrefix(v, "fail") || strings.HasPrefix(v, "traceback ") || strings.Contains(v, " failed") {
			return false
		}
	}
	return !strings.Contains(t.text, "*** Begin Patch") && !strings.Contains(t.text, "diff --git ")
}

func outputPreview(text string, limit int) string {
	head, tail := limit*2/3, limit/3
	for head > 0 && !utf8.RuneStart(text[head]) {
		head--
	}
	start := len(text) - tail
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[:head] + fmt.Sprintf("\n[… %d bytes omitted by token-trim; original archived]\n", start-head) + text[start:]
}

func (s *service) manageArchive(route string, r managementRequest) obj {
	c := s.currentContext()
	if c == nil {
		return jsonResponse(http.StatusServiceUnavailable, obj{"error": "archive disabled or MCP listener unavailable"})
	}
	switch {
	case route == "archive/namespaces" && r.Method == http.MethodGet:
		c.mu.Lock()
		status := obj{"namespaces": c.namespaceList(), "requests": c.requests, "output_bytes_removed": c.outputSaved, "history_bytes_removed": c.historySaved}
		c.mu.Unlock()
		return jsonResponse(http.StatusOK, status)
	case route == "archive/token" && r.Method == http.MethodPost:
		var in struct {
			Namespace string `json:"namespace"`
		}
		if len(r.Body) > 4096 || json.Unmarshal(r.Body, &in) != nil || in.Namespace == "" {
			return jsonResponse(400, obj{"error": "namespace is required"})
		}
		c.mu.Lock()
		if pending, ok := c.discovered[in.Namespace]; ok {
			if _, err := c.store.namespace(pending.caller, pending.workspace); err != nil {
				c.mu.Unlock()
				return jsonResponse(400, obj{"error": "archive registration unavailable"})
			}
		}
		token, err := c.store.issueToken(in.Namespace)
		c.mu.Unlock()
		if err != nil {
			return jsonResponse(400, obj{"error": "unknown namespace or storage unavailable"})
		}
		return jsonResponse(200, obj{"token": token, "namespace": in.Namespace, "notice": "Shown once. Configure this bearer only for this workspace MCP server."})
	case route == "archive/revoke" && r.Method == http.MethodPost:
		var in struct {
			Token string `json:"token"`
		}
		if len(r.Body) > 4096 || json.Unmarshal(r.Body, &in) != nil || in.Token == "" {
			return jsonResponse(400, obj{"error": "token is required"})
		}
		if err := c.store.revokeToken(in.Token); err != nil {
			return jsonResponse(400, obj{"error": "token revoke failed"})
		}
		return jsonResponse(200, obj{"revoked": true})
	}
	return jsonResponse(404, obj{"error": "not found"})
}
