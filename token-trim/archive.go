package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

const (
	archiveReadLimit   = 64 << 10
	archiveSearchBytes = 8 << 20
	archiveStateLimit  = 8 << 20
)

var (
	errArchiveInput   = errors.New("invalid archive argument")
	errArchiveMissing = errors.New("archive document not found")
	errArchiveQuota   = errors.New("archive quota exceeded")
	errArchiveStorage = errors.New("archive storage unavailable")
)

type archivePage struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Text       string `json:"text"`
	TotalBytes int    `json:"total_bytes"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset"`
}

type archiveNamespace struct {
	Namespace string `json:"namespace"`
	Workspace string `json:"workspace"`
}

type archiveState struct {
	Namespaces map[string]archiveNamespace `json:"namespaces"`
	Tokens     map[string]string           `json:"tokens"`
}

type archiveDocument struct {
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Bytes       int    `json:"bytes"`
	headerBytes int
}

type archiveStore struct {
	mu         sync.Mutex
	root       *os.Root
	lock       *os.File
	maxBytes   int64
	usedBytes  int64
	stateBytes int64
	state      archiveState
	docs       map[string]map[string]archiveDocument
	failed     bool
}

func archiveID(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func archiveHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func openArchive(path string, maxBytes int64) (*archiveStore, error) {
	if maxBytes <= 0 {
		return nil, errArchiveInput
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errArchiveStorage
	}
	// Reject symlinked ancestors too: os.Root confines later operations to this directory.
	var created []string
	for p := abs; ; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e == nil && (!st.IsDir() || st.Mode()&os.ModeSymlink != 0) {
			return nil, errArchiveStorage
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, errArchiveStorage
		}
		if errors.Is(e, os.ErrNotExist) {
			created = append(created, p)
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return nil, errArchiveStorage
	}
	for _, p := range created {
		parent, e := os.Open(filepath.Dir(p))
		if e != nil {
			return nil, errArchiveStorage
		}
		e = parent.Sync()
		parent.Close()
		if e != nil {
			return nil, errArchiveStorage
		}
	}
	st, err := os.Lstat(abs)
	if err != nil || st.Mode().Perm()&0077 != 0 {
		return nil, errArchiveStorage
	}
	r, err := os.OpenRoot(abs)
	if err != nil {
		return nil, errArchiveStorage
	}
	a := &archiveStore{root: r, maxBytes: maxBytes, state: archiveState{Namespaces: map[string]archiveNamespace{}, Tokens: map[string]string{}}, docs: map[string]map[string]archiveDocument{}}
	ok := false
	defer func() {
		if !ok {
			a.close()
		}
	}()
	a.lock, err = a.openPrivate(".lock")
	if errors.Is(err, os.ErrNotExist) {
		a.lock, err = r.OpenFile(".lock", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return nil, errArchiveStorage
	}
	if err = syscall.Flock(int(a.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errArchiveStorage
	}
	f, err := a.openPrivate("state.json")
	if err == nil {
		data, e := io.ReadAll(io.LimitReader(f, archiveStateLimit+1))
		f.Close()
		if e != nil || len(data) > archiveStateLimit || json.Unmarshal(data, &a.state) != nil || a.state.Namespaces == nil || a.state.Tokens == nil {
			return nil, errArchiveStorage
		}
		a.stateBytes = int64(len(data))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errArchiveStorage
	}
	for ns, meta := range a.state.Namespaces {
		if !archiveID(ns) || meta.Namespace != ns || len(meta.Workspace) > 512 || !utf8.ValidString(meta.Workspace) {
			return nil, errArchiveStorage
		}
		a.docs[ns] = map[string]archiveDocument{}
	}
	for hash, ns := range a.state.Tokens {
		if !archiveID(hash) || a.docs[ns] == nil {
			return nil, errArchiveStorage
		}
	}
	dir, err := r.Open(".")
	if err != nil {
		return nil, errArchiveStorage
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return nil, errArchiveStorage
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".lock" {
			continue
		}
		f, e := a.openPrivate(name)
		if e != nil {
			return nil, errArchiveStorage
		}
		info, e := f.Stat()
		if e != nil {
			f.Close()
			return nil, errArchiveStorage
		}
		if strings.HasPrefix(name, ".pending-") {
			f.Close()
			if e = r.Remove(name); e != nil {
				return nil, errArchiveStorage
			}
			continue
		}
		a.usedBytes += info.Size()
		if name == "state.json" {
			f.Close()
			continue
		}
		if len(name) == 134 && name[64] == '-' && name[129:] == ".view" && archiveID(name[:64]) && archiveID(name[65:129]) && a.docs[name[:64]] != nil {
			f.Close()
			if info.Size() > archiveStateLimit {
				return nil, errArchiveStorage
			}
			continue
		}
		if len(name) != 133 || name[64] != '-' || name[129:] != ".doc" || !archiveID(name[:64]) || !archiveID(name[65:129]) || a.docs[name[:64]] == nil {
			f.Close()
			return nil, errArchiveStorage
		}
		br := bufio.NewReaderSize(f, 4096)
		header, e := br.ReadSlice('\n')
		var doc archiveDocument
		if e != nil || json.Unmarshal(header, &doc) != nil || !validArchiveMetadata(doc.Kind, doc.Title) || doc.Bytes < 0 || int64(doc.Bytes)+int64(len(header)) != info.Size() {
			f.Close()
			return nil, errArchiveStorage
		}
		h := sha256.New()
		_, e = io.Copy(h, br)
		f.Close()
		if e != nil || hex.EncodeToString(h.Sum(nil)) != name[65:129] {
			return nil, errArchiveStorage
		}
		doc.headerBytes = len(header)
		a.docs[name[:64]][name[65:129]] = doc
	}
	if a.stateBytes == 0 {
		if err = a.persistIndex(a.state); err != nil {
			return nil, err
		}
	}
	if err = a.syncDir(); err != nil {
		return nil, errArchiveStorage
	}
	ok = true
	return a, nil
}

func (a *archiveStore) close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.root == nil {
		return nil
	}
	a.failed = true
	err := a.root.Close()
	a.root = nil
	if a.lock != nil {
		err = errors.Join(err, a.lock.Close())
		a.lock = nil
	}
	return err
}

func (a *archiveStore) openPrivate(name string) (*os.File, error) {
	st, err := a.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return nil, errArchiveStorage
	}
	return a.root.Open(name)
}

func (a *archiveStore) syncDir() error {
	f, err := a.root.Open(".")
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (a *archiveStore) atomicWrite(name string, parts ...string) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errArchiveStorage
	}
	tmp := ".pending-" + hex.EncodeToString(nonce[:])
	f, err := a.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errArchiveStorage
	}
	defer a.root.Remove(tmp)
	for _, part := range parts {
		if _, err = io.WriteString(f, part); err != nil {
			f.Close()
			return errArchiveStorage
		}
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return errArchiveStorage
	}
	if err = f.Close(); err != nil {
		return errArchiveStorage
	}
	if err = a.root.Rename(tmp, name); err != nil {
		return errArchiveStorage
	}
	if err = a.syncDir(); err != nil {
		// A rename with failed directory fsync has uncertain durability; refuse further use.
		a.failed = true
		return errArchiveStorage
	}
	return nil
}

func (a *archiveStore) persistIndex(state archiveState) error {
	data, err := json.Marshal(state)
	if err != nil || len(data) > archiveStateLimit {
		return errArchiveQuota
	}
	if next := a.usedBytes - a.stateBytes + int64(len(data)); next > a.maxBytes && next > a.usedBytes {
		return errArchiveQuota
	}
	if err = a.atomicWrite("state.json", string(data)); err != nil {
		return err
	}
	a.usedBytes += int64(len(data)) - a.stateBytes
	a.stateBytes = int64(len(data))
	a.state = state
	return nil
}

func (a *archiveStore) copyState() archiveState {
	state := archiveState{Namespaces: make(map[string]archiveNamespace, len(a.state.Namespaces)), Tokens: make(map[string]string, len(a.state.Tokens))}
	for k, v := range a.state.Namespaces {
		state.Namespaces[k] = v
	}
	for k, v := range a.state.Tokens {
		state.Tokens[k] = v
	}
	return state
}

func archiveNamespaceID(caller, workspace string) (string, error) {
	if caller == "" || len(caller) > 4096 || !utf8.ValidString(caller) || len(workspace) > 512 || !utf8.ValidString(workspace) {
		return "", errArchiveInput
	}
	if workspace == "" {
		workspace = "default"
	}
	identity, _ := json.Marshal([2]string{caller, workspace})
	return archiveHash(string(identity)), nil
}

func (a *archiveStore) namespace(caller, workspace string) (string, error) {
	ns, err := archiveNamespaceID(caller, workspace)
	if err != nil {
		return "", err
	}
	if workspace == "" {
		workspace = "default"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return "", errArchiveStorage
	}
	if _, ok := a.state.Namespaces[ns]; ok {
		return ns, nil
	}
	state := a.copyState()
	state.Namespaces[ns] = archiveNamespace{Namespace: ns, Workspace: workspace}
	if err := a.persistIndex(state); err != nil {
		return "", err
	}
	a.docs[ns] = map[string]archiveDocument{}
	return ns, nil
}

func validArchiveMetadata(kind, title string) bool {
	return len(kind) <= 64 && len(title) <= 512 && utf8.ValidString(kind) && utf8.ValidString(title)
}

func (a *archiveStore) put(namespace, kind, title, text string) (string, error) {
	if !archiveID(namespace) || !validArchiveMetadata(kind, title) || !utf8.ValidString(text) {
		return "", errArchiveInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return "", errArchiveStorage
	}
	docs := a.docs[namespace]
	if docs == nil {
		return "", errArchiveMissing
	}
	id := archiveHash(text)
	if _, ok := docs[id]; ok {
		return id, nil
	}
	doc := archiveDocument{Kind: kind, Title: title, Bytes: len(text)}
	header, _ := json.Marshal(doc)
	header = append(header, '\n')
	size := int64(len(header)) + int64(len(text))
	if size > a.maxBytes-a.usedBytes {
		return "", errArchiveQuota
	}
	if err := a.atomicWrite(namespace+"-"+id+".doc", string(header), text); err != nil {
		return "", err
	}
	doc.headerBytes = len(header)
	docs[id] = doc
	a.usedBytes += size
	return id, nil
}

func (a *archiveStore) read(namespace, id string, offset, limit int) (archivePage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return archivePage{}, errArchiveStorage
	}
	if !archiveID(namespace) || !archiveID(id) || offset < 0 || limit < 0 || limit > archiveReadLimit || limit > 0 && limit < utf8.UTFMax {
		return archivePage{}, errArchiveInput
	}
	if limit == 0 {
		limit = 16 << 10
	}
	doc, ok := a.docs[namespace][id]
	if !ok {
		return archivePage{}, errArchiveMissing
	}
	if offset > doc.Bytes {
		return archivePage{}, errArchiveInput
	}
	f, err := a.openPrivate(namespace + "-" + id + ".doc")
	if err != nil {
		return archivePage{}, errArchiveStorage
	}
	defer f.Close()
	size := min(limit, doc.Bytes-offset)
	// Read past the byte limit only to find its preceding rune boundary.
	data := make([]byte, min(size+utf8.UTFMax-1, doc.Bytes-offset))
	if n, e := f.ReadAt(data, int64(doc.headerBytes+offset)); n != len(data) || e != nil && e != io.EOF {
		return archivePage{}, errArchiveStorage
	}
	if len(data) > 0 && !utf8.RuneStart(data[0]) {
		return archivePage{}, errArchiveInput
	}
	end := min(size, len(data))
	for end > 0 && end < len(data) && !utf8.RuneStart(data[end]) {
		end--
	}
	if !utf8.Valid(data[:end]) {
		return archivePage{}, errArchiveStorage
	}
	page := archivePage{ID: id, Kind: doc.Kind, Title: doc.Title, Text: string(data[:end]), TotalBytes: doc.Bytes, Offset: offset}
	if next := offset + end; next < doc.Bytes {
		page.NextOffset = &next
	}
	return page, nil
}

type archiveMatch struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
}

type archiveSearchResult struct {
	Matches   []archiveMatch `json:"matches"`
	Truncated bool           `json:"truncated"`
}

func (a *archiveStore) search(namespace, query string, limit int) (any, error) {
	if !archiveID(namespace) || !utf8.ValidString(query) || len(query) > 256 || limit < 0 || limit > 50 {
		return nil, errArchiveInput
	}
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 || len(terms) > 16 {
		return nil, errArchiveInput
	}
	if limit == 0 {
		limit = 10
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return nil, errArchiveStorage
	}
	docs := a.docs[namespace]
	if docs == nil {
		return nil, errArchiveMissing
	}
	ids := make([]string, 0, len(docs))
	for id := range docs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := archiveSearchResult{Matches: []archiveMatch{}}
	remaining := archiveSearchBytes
	for i, id := range ids {
		if remaining == 0 || i >= 1024 {
			result.Truncated = true
			break
		}
		doc := docs[id]
		f, err := a.openPrivate(namespace + "-" + id + ".doc")
		if err != nil {
			return nil, errArchiveStorage
		}
		size := min(doc.Bytes, remaining)
		data := make([]byte, size)
		n, e := f.ReadAt(data, int64(doc.headerBytes))
		f.Close()
		if n != len(data) || e != nil && e != io.EOF {
			return nil, errArchiveStorage
		}
		remaining -= size
		for len(data) > 0 && !utf8.Valid(data) {
			data = data[:len(data)-1]
			if size-len(data) > utf8.UTFMax {
				return nil, errArchiveStorage
			}
		}
		body := string(data)
		text := strings.ToLower(body)
		metadata := strings.ToLower(doc.Kind + " " + doc.Title)
		match := true
		for _, term := range terms {
			if !strings.Contains(metadata, term) && !strings.Contains(text, term) {
				match = false
				break
			}
		}
		if size < doc.Bytes {
			result.Truncated = true
		}
		if !match {
			continue
		}
		// Case folding preserves rune count, but may change byte offsets.
		start := 0
		if pos := strings.Index(text, terms[0]); pos >= 0 {
			runes := utf8.RuneCountInString(text[:pos])
			for pos := range body {
				if runes == 0 {
					start = max(0, pos-80)
					break
				}
				runes--
			}
		}
		for start > 0 && !utf8.RuneStart(data[start]) {
			start--
		}
		end := min(len(data), start+320)
		for end > start && end < len(data) && !utf8.RuneStart(data[end]) {
			end--
		}
		result.Matches = append(result.Matches, archiveMatch{ID: id, Kind: doc.Kind, Title: doc.Title, Snippet: body[start:end]})
		if len(result.Matches) == limit {
			result.Truncated = result.Truncated || i+1 < len(ids)
			break
		}
	}
	return result, nil
}

func (a *archiveStore) namespaces() any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]archiveNamespace, 0, len(a.state.Namespaces))
	for _, ns := range a.state.Namespaces {
		out = append(out, ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

func (a *archiveStore) issueToken(namespace string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return "", errArchiveStorage
	}
	if !archiveID(namespace) || a.docs[namespace] == nil {
		return "", errArchiveInput
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errArchiveStorage
	}
	token := "tt_" + hex.EncodeToString(raw[:])
	state := a.copyState()
	state.Tokens[archiveHash(token)] = namespace
	if err := a.persistIndex(state); err != nil {
		return "", err
	}
	return token, nil
}

func (a *archiveStore) revokeToken(token string) error {
	if len(token) != 67 || !strings.HasPrefix(token, "tt_") || !archiveID(token[3:]) {
		return errArchiveInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return errArchiveStorage
	}
	state := a.copyState()
	delete(state.Tokens, archiveHash(token))
	return a.persistIndex(state)
}

func (a *archiveStore) tokenNamespace(token string) (string, bool) {
	if len(token) != 67 || !strings.HasPrefix(token, "tt_") || !archiveID(token[3:]) {
		return "", false
	}
	hash := archiveHash(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return "", false
	}
	var ns string
	for stored, namespace := range a.state.Tokens {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(stored)) == 1 {
			ns = namespace
		}
	}
	return ns, ns != ""
}

// Keep formatting errors independent of filesystem paths exposed by os errors.
func archiveToolError(err error) string {
	if errors.Is(err, errArchiveInput) || errors.Is(err, errArchiveMissing) || errors.Is(err, errArchiveQuota) {
		return err.Error()
	}
	return errArchiveStorage.Error()
}

func archiveStateKey(key string) bool {
	if key == "" || len(key) > 128 || key == "." || key == ".." {
		return false
	}
	for _, c := range key {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (a *archiveStore) loadState(namespace, key string) ([]byte, error) {
	if !archiveID(namespace) || !archiveStateKey(key) {
		return nil, errArchiveInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return nil, errArchiveStorage
	}
	if a.docs[namespace] == nil {
		return nil, errArchiveMissing
	}
	f, err := a.openPrivate(namespace + "-" + archiveHash(key) + ".view")
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, errArchiveStorage
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, archiveStateLimit+1))
	if err != nil || len(data) > archiveStateLimit {
		return nil, errArchiveStorage
	}
	return data, nil
}

func (a *archiveStore) saveState(namespace, key string, data []byte) error {
	if !archiveID(namespace) || !archiveStateKey(key) || len(data) > archiveStateLimit {
		return errArchiveInput
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return errArchiveStorage
	}
	if a.docs[namespace] == nil {
		return errArchiveMissing
	}
	name := namespace + "-" + archiveHash(key) + ".view"
	var oldBytes int64
	f, err := a.openPrivate(name)
	if err == nil {
		info, e := f.Stat()
		f.Close()
		if e != nil {
			return errArchiveStorage
		}
		oldBytes = info.Size()
	} else if !errors.Is(err, os.ErrNotExist) {
		return errArchiveStorage
	}
	if next := a.usedBytes - oldBytes + int64(len(data)); next > a.maxBytes && next > a.usedBytes {
		return errArchiveQuota
	}
	if err = a.atomicWrite(name, string(data)); err != nil {
		return err
	}
	a.usedBytes += int64(len(data)) - oldBytes
	return nil
}
