package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dbRoot writes one minimal session under root/proj-db and returns its path.
func dbRoot(t *testing.T, root, uuid, title string) string {
	t.Helper()
	dir := filepath.Join(root, "proj-db")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"custom-title","customTitle":"` + title + `"}`,
		`{"type":"user","uuid":"u1","timestamp":"2026-07-17T09:00:00.000Z","cwd":"/x/proj-db","message":{"role":"user","content":[{"type":"text","text":"hello database"}]}}`,
	}
	path := filepath.Join(dir, uuid+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func countRows(t *testing.T, ix *Index, table string) int {
	t.Helper()
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The core promise of the cache: a second boot serves the stored parse and
// never re-reads the transcript. Proven by corrupting the file in place
// (same size, same mtime) — a re-parse would see garbage, the cache doesn't.
func TestWarmLoadServesCacheWithoutReparse(t *testing.T) {
	root := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "cfg")
	path := dbRoot(t, root, "11111111-aaaa-bbbb-cccc-000000000001", "warm title")

	db1, err := openIndexDB(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	ix1 := NewIndex(root)
	ix1.db = db1
	updated, err := ix1.Rescan()
	if err != nil || len(updated) != 1 {
		t.Fatalf("first scan: updated=%v err=%v, want 1 parse", updated, err)
	}
	db1.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	garbage := strings.Repeat("x", int(fi.Size()))
	if err := os.WriteFile(path, []byte(garbage), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}

	db2, err := openIndexDB(cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	ix2 := NewIndex(root)
	ix2.db = db2
	updated, err = ix2.Rescan()
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 0 {
		t.Errorf("warm boot re-parsed %d sessions, want 0", len(updated))
	}
	s := ix2.Session("11111111-aaaa-bbbb-cccc-000000000001")
	if s == nil || s.Title != "warm title" {
		t.Fatalf("cached session lost: %+v", s)
	}
}

func TestStampChangeReparses(t *testing.T) {
	root := t.TempDir()
	path := dbRoot(t, root, "22222222-aaaa-bbbb-cccc-000000000002", "before")
	ix := newSearchIndex(t, root)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"type":"custom-title","customTitle":"after"}` + "\n")
	f.Close()

	updated, err := ix.Rescan()
	if err != nil || len(updated) != 1 {
		t.Fatalf("changed file: updated=%v err=%v, want 1 re-parse", updated, err)
	}
	if s := ix.Session("22222222-aaaa-bbbb-cccc-000000000002"); s == nil || s.Title != "after" {
		t.Fatalf("re-parse didn't land: %+v", s)
	}
}

// A different generation — here, a different transcript root — must wipe the
// cache: cached rows from another corpus answering for this one would be
// silent nonsense.
func TestGenerationChangeWipes(t *testing.T) {
	rootA := t.TempDir()
	cfg := filepath.Join(t.TempDir(), "cfg")
	dbRoot(t, rootA, "33333333-aaaa-bbbb-cccc-000000000003", "gen A")

	db1, err := openIndexDB(cfg, rootA)
	if err != nil {
		t.Fatal(err)
	}
	ix1 := NewIndex(rootA)
	ix1.db = db1
	if _, err := ix1.Rescan(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ix1, "sessions"); n != 1 {
		t.Fatalf("persisted rows = %d, want 1", n)
	}
	db1.Close()

	db2, err := openIndexDB(cfg, t.TempDir()) // same file, different root
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	ix2 := NewIndex("unused")
	ix2.db = db2
	if n := countRows(t, ix2, "sessions"); n != 0 {
		t.Errorf("generation change kept %d stale rows, want 0", n)
	}
}

// A deleted transcript must not resurrect from the cache on the next boot.
func TestPruneDropsDeletedSessions(t *testing.T) {
	root := t.TempDir()
	dbRoot(t, root, "44444444-aaaa-bbbb-cccc-000000000014", "keep me")
	gone := dbRoot(t, root, "55555555-aaaa-bbbb-cccc-000000000015", "delete me")
	ix := newSearchIndex(t, root)
	if n := countRows(t, ix, "sessions"); n != 2 {
		t.Fatalf("persisted rows = %d, want 2", n)
	}

	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Rescan(); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, ix, "sessions"); n != 1 {
		t.Errorf("pruned rows = %d, want 1", n)
	}
	var n int
	if err := ix.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id=?`,
		"55555555-aaaa-bbbb-cccc-000000000015").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("deleted session left %d text rows behind", n)
	}
}
