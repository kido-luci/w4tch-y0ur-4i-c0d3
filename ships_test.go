package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"watch-your-ai-code/internal/index"
)

// newShipsStore builds a ship store over an Index whose only job is the ships
// table — an empty transcript root, a real index.db. TestShipsJoinSessions
// swaps in a fakeSessions for the join case rather than reaching into this
// index's (always-empty) session set, which now lives in another package.
func newShipsStore(t *testing.T) *shipStore {
	t.Helper()
	root := t.TempDir()
	db, err := index.OpenDB(filepath.Join(t.TempDir(), "cfg"), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ix := index.New(root)
	ix.UseCache(db)
	return newShipStore(ix.DB(), ix)
}

// fakeSessions satisfies any of the narrow Snapshot() []*index.Session
// interfaces this package declares (shipSessions here, repoSessions in
// codegraph.go — also used by codegraph_test.go). It hands a test exact
// session data without needing write access to an Index's session map, which
// is unexported and now lives in another package.
type fakeSessions []*index.Session

func (f fakeSessions) Snapshot() []*index.Session { return f }

func writeShip(t *testing.T, dir, name string, r ShipRecord) {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShipsLifecycle(t *testing.T) {
	sh := newShipsStore(t)
	dir := t.TempDir()
	now := time.Now()
	writeShip(t, dir, "100-1-proj-a-release.json", ShipRecord{
		Project: "proj-a", Kind: "release", Version: "v1.2.0", SHA: "abc123",
		Exit: 0, DurationMs: 90000, Ts: now.Add(-time.Hour), Log: "all green",
	})
	writeShip(t, dir, "200-1-proj-a-check.json", ShipRecord{
		Project: "proj-a", Kind: "check", Exit: 1, DurationMs: 30000,
		Ts: now.Add(-30 * time.Minute), Log: "gofmt failed",
	})

	if n := sh.Scan(dir); n != 2 {
		t.Fatalf("ingested = %d, want 2", n)
	}
	if n := sh.Scan(dir); n != 0 {
		t.Errorf("re-scan ingested %d, want 0 (dedupe by file name)", n)
	}

	res := sh.List("", 0, 10, false)
	if res.Total != 2 || len(res.Ships) != 2 {
		t.Fatalf("Total=%d len=%d, want 2/2", res.Total, len(res.Ships))
	}
	// Newest first: the failed check (30m ago) before the release (1h ago).
	if res.Ships[0].Kind != "check" || res.Ships[0].Exit != 1 {
		t.Errorf("newest = %+v, want the failed check", res.Ships[0])
	}
	if res.Ships[1].Version != "v1.2.0" || res.Ships[1].SHA != "abc123" {
		t.Errorf("release record lost fields: %+v", res.Ships[1])
	}
	if res.Ships[0].Log != "" {
		t.Error("logs must not ride along unless asked")
	}
	if withLog := sh.List("", 0, 10, true); withLog.Ships[0].Log != "gofmt failed" {
		t.Errorf("withLog lost the log: %+v", withLog.Ships[0])
	}

	// A deleted drop file leaves the history on the next reconcile.
	if err := os.Remove(filepath.Join(dir, "100-1-proj-a-release.json")); err != nil {
		t.Fatal(err)
	}
	sh.Scan(dir)
	if res := sh.List("", 0, 10, false); res.Total != 1 {
		t.Errorf("after prune Total=%d, want 1", res.Total)
	}
}

func TestShipsSkipsForeignFiles(t *testing.T) {
	sh := newShipsStore(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o644)
	os.WriteFile(filepath.Join(dir, "foreign.json"), []byte(`{"hello":"world"}`), 0o644)
	os.WriteFile(filepath.Join(dir, ".tmp.hidden.json"), []byte(`{}`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a record"), 0o644)

	if n := sh.Scan(dir); n != 0 {
		t.Errorf("ingested %d foreign files, want 0", n)
	}
	if res := sh.List("", 0, 10, false); res.Total != 0 {
		t.Errorf("Total=%d, want 0", res.Total)
	}
}

func TestShipsFilters(t *testing.T) {
	sh := newShipsStore(t)
	dir := t.TempDir()
	now := time.Now()
	writeShip(t, dir, "1-1-proj-a-release.json", ShipRecord{
		Project: "proj-a", Kind: "release", Version: "v1.0.0", Ts: now.Add(-48 * time.Hour),
	})
	writeShip(t, dir, "2-1-proj-b-release.json", ShipRecord{
		Project: "proj-b", Kind: "release", Version: "v2.0.0", Ts: now.Add(-time.Minute),
	})
	sh.Scan(dir)

	if res := sh.List("proj-a", 0, 10, false); res.Total != 1 || res.Ships[0].Version != "v1.0.0" {
		t.Errorf("project filter: %+v", res)
	}
	if res := sh.List("", 1, 10, false); res.Total != 1 || res.Ships[0].Project != "proj-b" {
		t.Errorf("days filter should keep only the recent record: %+v", res)
	}
	if res := sh.List("", 0, 1, false); res.Total != 2 || len(res.Ships) != 1 {
		t.Errorf("limit must cap Ships but not Total: Total=%d len=%d", res.Total, len(res.Ships))
	}
	// A comma-separated project list matches any of its names (the scoped
	// ships tab sends its whole scope set); unknown names and blanks are inert.
	if res := sh.List("proj-a, proj-b", 0, 10, false); res.Total != 2 {
		t.Errorf("multi-project filter should match both: %+v", res)
	}
	if res := sh.List("proj-a,,nope ", 0, 10, false); res.Total != 1 || res.Ships[0].Project != "proj-a" {
		t.Errorf("multi-project filter with junk names: %+v", res)
	}
}

func TestShipsJoinSessions(t *testing.T) {
	sh := newShipsStore(t)
	dir := t.TempDir()
	now := time.Now()
	// Ran mid-session: joined. Ran an hour after any session ended: not.
	writeShip(t, dir, "100-1-proj-a-release.json", ShipRecord{
		Project: "proj-a", Kind: "release", Version: "v1.0.0", Exit: 0,
		DurationMs: 1000, Ts: now.Add(-time.Hour),
	})
	writeShip(t, dir, "200-1-proj-a-check.json", ShipRecord{
		Project: "proj-a", Kind: "check", Exit: 0, DurationMs: 1000, Ts: now,
	})
	// Same window, wrong project: never joined.
	writeShip(t, dir, "300-1-proj-b-check.json", ShipRecord{
		Project: "proj-b", Kind: "check", Exit: 0, DurationMs: 1000,
		Ts: now.Add(-time.Hour),
	})
	sh.Scan(dir)

	// Two overlapping proj-a sessions cover the release; the later-started one
	// must win. Both ended well before the second record's ts.
	sh.sessions = fakeSessions{
		{ID: "s-early", Project: "proj-a", Title: "early",
			StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-50 * time.Minute)},
		{ID: "s-late", Project: "proj-a", Title: "late",
			StartedAt: now.Add(-2 * time.Hour), EndedAt: now.Add(-50 * time.Minute)},
	}

	res := sh.List("", 0, 10, false)
	byFile := map[string]ShipRecord{}
	for _, r := range res.Ships {
		byFile[r.File] = r
	}
	if r := byFile["100-1-proj-a-release.json"]; r.SessionID != "s-late" || r.SessionTitle != "late" {
		t.Errorf("release should join the latest-started covering session, got %q", r.SessionID)
	}
	if r := byFile["200-1-proj-a-check.json"]; r.SessionID != "" {
		t.Errorf("a run outside every session window joined %q", r.SessionID)
	}
	if r := byFile["300-1-proj-b-check.json"]; r.SessionID != "" {
		t.Errorf("a run from another project joined %q", r.SessionID)
	}
}
