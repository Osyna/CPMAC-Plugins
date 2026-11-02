package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func testArchive(t *testing.T) *archiveStore {
	t.Helper()
	a, err := openArchive(filepath.Join(t.TempDir(), "archive"), 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.close() })
	return a
}

func testNamespace(t *testing.T, a *archiveStore, caller, workspace string) string {
	t.Helper()
	ns, err := a.namespace(caller, workspace)
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func testDocument(t *testing.T, a *archiveStore, ns, text string) string {
	t.Helper()
	id, err := a.put(ns, "tool", "historical output", text)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestArchiveRestartExactAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	a, err := openArchive(path, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	ns := testNamespace(t, a, "host-authenticated-caller", "/work/project")
	other := testNamespace(t, a, "another-caller", "/work/project")
	workspace := testNamespace(t, a, "host-authenticated-caller", "/work/other")
	if ns == other || ns == workspace {
		t.Fatal("identities collapsed")
	}
	raw := "first\r\nこんにちは 🌎\x00\nlast\n"
	id := testDocument(t, a, ns, raw)
	token, err := a.issueToken(ns)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.saveState(ns, "history-v1", []byte("private-state")); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(path, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(state, []byte(token)) || bytes.Contains(state, []byte("host-authenticated-caller")) {
		t.Fatal("credential stored in plaintext")
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	a, err = openArchive(path, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if a != nil {
			a.close()
		}
	}()
	if got := testNamespace(t, a, "host-authenticated-caller", "/work/project"); got != ns {
		t.Fatal("namespace changed on restart")
	}
	page, err := a.read(ns, id, 0, 0)
	if err != nil || page.Text != raw || page.TotalBytes != len(raw) {
		t.Fatalf("read = %#v, %v", page, err)
	}
	for _, wrong := range []string{other, workspace, archiveHash("unknown")} {
		if _, err = a.read(wrong, id, 0, 0); !errors.Is(err, errArchiveMissing) {
			t.Fatalf("cross-namespace read = %v", err)
		}
	}
	if got, ok := a.tokenNamespace(token); !ok || got != ns {
		t.Fatal("token did not survive restart")
	}
	if err = a.revokeToken(token); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.tokenNamespace(token); ok {
		t.Fatal("revoked token accepted")
	}
	if data, err := a.loadState(ns, "history-v1"); err != nil || string(data) != "private-state" {
		t.Fatalf("state = %q, %v", data, err)
	}
	if _, err = a.loadState(other, "history-v1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cross-namespace state = %v", err)
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	a, err = openArchive(path, 16<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := a.tokenNamespace(token); ok {
		t.Fatal("revocation not persisted")
	}
}

func TestArchiveUnicodePagesAndValidation(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "caller", "")
	raw := "abé界🌎z\n" + strings.Repeat("é🌎界a", 20)
	id := testDocument(t, a, ns, raw)
	for _, limit := range []int{4, 5, 6, 7, 11, 64} {
		var out strings.Builder
		offset := 0
		for {
			page, err := a.read(ns, id, offset, limit)
			if err != nil {
				t.Fatal(err)
			}
			if !utf8.ValidString(page.Text) || len(page.Text) > limit || page.Offset != offset {
				t.Fatalf("invalid page %#v", page)
			}
			out.WriteString(page.Text)
			if page.NextOffset == nil {
				break
			}
			if *page.NextOffset <= offset {
				t.Fatal("pagination made no progress")
			}
			offset = *page.NextOffset
		}
		if out.String() != raw {
			t.Fatalf("limit %d changed text", limit)
		}
	}
	for _, c := range []struct{ offset, limit int }{{-1, 4}, {3, 4}, {len(raw) + 1, 4}, {0, 1}, {0, archiveReadLimit + 1}, {0, -1}} {
		if _, err := a.read(ns, id, c.offset, c.limit); !errors.Is(err, errArchiveInput) {
			t.Fatalf("read(%d,%d) = %v", c.offset, c.limit, err)
		}
	}
	for _, bad := range []string{"../state.json", strings.Repeat("A", 64), ""} {
		if _, err := a.read(ns, bad, 0, 4); err == nil {
			t.Fatal("unsafe ID accepted")
		}
		if err := a.saveState(ns, bad, []byte("x")); bad != strings.Repeat("A", 64) && err == nil {
			t.Fatal("unsafe state key accepted")
		}
	}
	if _, err := a.put(ns, "tool", "invalid", string([]byte{255})); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	if _, err := a.namespace("", "workspace"); err == nil {
		t.Fatal("missing caller accepted")
	}
}

func TestArchiveQuotaRefusesWithoutEviction(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "caller", "workspace")
	other := testNamespace(t, a, "second-caller", "workspace")
	id := testDocument(t, a, ns, "kept forever")
	before := a.usedBytes
	a.maxBytes = before + 16
	if _, err := a.put(ns, "tool", "new", strings.Repeat("x", 100)); !errors.Is(err, errArchiveQuota) {
		t.Fatalf("write at quota = %v", err)
	}
	if _, err := a.put(other, "tool", "new", "other tenant output"); !errors.Is(err, errArchiveQuota) {
		t.Fatalf("global quota bypass = %v", err)
	}
	if err := a.saveState(ns, "layout", make([]byte, 17)); !errors.Is(err, errArchiveQuota) {
		t.Fatalf("state at quota = %v", err)
	}
	if a.usedBytes != before {
		t.Fatal("failed write consumed quota")
	}
	if got, err := a.put(ns, "other", "duplicate metadata", "kept forever"); err != nil || got != id {
		t.Fatalf("dedup = %s %v", got, err)
	}
	if page, err := a.read(ns, id, 0, 0); err != nil || page.Text != "kept forever" {
		t.Fatal("quota evicted existing data")
	}
	a.maxBytes = before + 100
	if err := a.saveState(ns, "layout", []byte("one")); err != nil {
		t.Fatal(err)
	}
	used := a.usedBytes
	if err := a.saveState(ns, "layout", []byte("two")); err != nil || a.usedBytes != used {
		t.Fatal("state replacement miscounted quota")
	}
}

func TestArchiveSearchIsolationAndBounds(t *testing.T) {
	a := testArchive(t)
	ns := testNamespace(t, a, "one", "work")
	other := testNamespace(t, a, "two", "work")
	id := testDocument(t, a, ns, "αβγ Unique NEEDLE result")
	testDocument(t, a, other, "another-tenant-only needle")
	if err := a.saveState(ns, "hidden", []byte("state-secret-needle")); err != nil {
		t.Fatal(err)
	}
	value, err := a.search(ns, "unique needle", 10)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(archiveSearchResult)
	if len(result.Matches) != 1 || result.Matches[0].ID != id || !utf8.ValidString(result.Matches[0].Snippet) {
		t.Fatalf("search = %#v", result)
	}
	for _, query := range []string{"another-tenant-only", "state-secret-needle"} {
		value, err = a.search(ns, query, 10)
		if err != nil || len(value.(archiveSearchResult).Matches) != 0 {
			t.Fatalf("leaked %q", query)
		}
	}
	for _, query := range []string{"", strings.Repeat("x", 257), strings.Repeat("a ", 17)} {
		if _, err = a.search(ns, query, 10); err == nil {
			t.Fatalf("invalid query %q accepted", query)
		}
	}
	if _, err = a.search(ns, "needle", 51); err == nil {
		t.Fatal("unbounded count accepted")
	}
	testDocument(t, a, ns, strings.Repeat("x", archiveSearchBytes+1))
	value, err = a.search(ns, "absent", 10)
	if err != nil || !value.(archiveSearchResult).Truncated {
		t.Fatalf("scan budget = %#v %v", value, err)
	}
}

func TestArchiveRejectsUnsafeStorageAndFailedWrites(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openArchive(link, 1<<20); err == nil {
		t.Fatal("symlinked root accepted")
	}
	a, err := openArchive(real, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ns := testNamespace(t, a, "caller", "work")
	id := testDocument(t, a, ns, "exact")
	name := ns + "-" + id + ".doc"
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(real, name)); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(real, "state.json"), filepath.Join(real, name)); err != nil {
		t.Fatal(err)
	}
	if _, err = openArchive(real, 1<<20); err == nil {
		t.Fatal("symlinked document accepted")
	}
	a = testArchive(t)
	ns = testNamespace(t, a, "caller", "work")
	a.root.Close()
	if _, err = a.put(ns, "tool", "cannot persist", "new raw output"); err == nil {
		t.Fatal("write to closed storage reported durable success")
	}
}

func TestArchiveExclusiveWriterAndCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive")
	a, err := openArchive(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ns := testNamespace(t, a, "caller", "workspace")
	id := testDocument(t, a, ns, "durable raw bytes")
	if second, err := openArchive(path, 1<<20); err == nil {
		second.close()
		t.Fatal("second writer acquired archive")
	}
	if err = a.close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, ns+"-"+id+".doc")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	if second, err := openArchive(path, 1<<20); err == nil {
		second.close()
		t.Fatal("corrupted raw content accepted")
	}
}
