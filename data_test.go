package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fixtureDrawingsDDL is the drawings table as it stands at v4+ (the v1 shape
// plus v4's group_name). Migration fixtures at those versions must carry it
// now that the v→10 step ALTERs it — a version stamp alone no longer passes.
const fixtureDrawingsDDL = `
CREATE TABLE drawings(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	group_name TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	thumb_updated_at INTEGER NOT NULL DEFAULT 0,
	scene BLOB NOT NULL,
	thumb BLOB
);`

// newTestDataDB opens a real data.db in a temp config dir — every store test
// runs over the same engine the app ships.
func newTestDataDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDataDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Pre-v0.35 boards carry a single linkedSessionId and a snapshot with no
// session count; the one-time import must fold the link into the list, count
// the snapshot as the one session it could only ever have covered, rename the
// file away — and never merge a file that reappears afterwards.
func TestImportTodosLegacyAndReappearance(t *testing.T) {
	cfg := t.TempDir()
	legacy := `[
	  {"id":"a","seq":1,"title":"old linked card","status":"done","order":1,
	   "createdAt":"2026-07-01T10:00:00Z","linkedSessionId":"sess-legacy",
	   "snapshot":{"tokens":10,"costUsd":0.5,"agents":1,"durationMs":1000,"takenAt":"2026-07-01T11:00:00Z"}},
	  {"id":"b","seq":2,"title":"never linked","status":"backlog","order":1,
	   "createdAt":"2026-07-01T10:00:00Z"}
	]`
	if err := os.WriteFile(filepath.Join(cfg, "todos.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	importDataOnce(db, cfg)

	byID := map[string]Todo{}
	for _, todo := range NewTodoStore(db).List() {
		byID[todo.ID] = todo
	}
	if len(byID) != 2 {
		t.Fatalf("want 2 todos imported, got %d", len(byID))
	}
	migrated := byID["a"]
	if len(migrated.LinkedSessionIDs) != 1 || migrated.LinkedSessionIDs[0] != "sess-legacy" {
		t.Fatalf("legacy link should backfill into the list, got %#v", migrated.LinkedSessionIDs)
	}
	if migrated.Snapshot == nil || migrated.Snapshot.Tokens != 10 || migrated.Snapshot.Sessions != 1 {
		t.Fatalf("legacy snapshot should carry over with Sessions=1: %#v", migrated.Snapshot)
	}

	// The original is set aside, not deleted.
	if _, err := os.Stat(filepath.Join(cfg, "todos.json")); !os.IsNotExist(err) {
		t.Fatalf("todos.json should be renamed away, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "todos.json.migrated-backup")); err != nil {
		t.Fatalf("migrated backup should exist: %v", err)
	}

	// A reappeared todos.json (an old binary ran) must NOT merge.
	if err := os.WriteFile(filepath.Join(cfg, "todos.json"),
		[]byte(`[{"id":"zzz","seq":9,"title":"stray write","status":"backlog","order":1,"createdAt":"2026-07-02T10:00:00Z"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	importDataOnce(db, cfg)
	if got := len(NewTodoStore(db).List()); got != 2 {
		t.Fatalf("reappeared file must not merge: want 2 todos, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(cfg, "todos.json")); err != nil {
		t.Fatalf("reappeared file should be left in place for review: %v", err)
	}
}

func TestImportDrawingsWithScenesThumbsBackups(t *testing.T) {
	cfg := t.TempDir()
	dir := filepath.Join(cfg, "drawings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	index := `[{"id":"d1","name":"login","createdAt":"2026-07-01T10:00:00Z",
	 "updatedAt":"2026-07-02T10:00:00Z","thumbUpdatedAt":"2026-07-02T10:00:00Z"}]`
	if err := os.WriteFile(filepath.Join(cfg, "drawings.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "d1.excalidraw"), []byte(`{"v":"scene"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "d1.thumb.png"), []byte("png-bytes"), 0o644)
	os.WriteFile(filepath.Join(dir, "d1.excalidraw.bak.1"), []byte(`{"v":"prev"}`), 0o644)

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	importDataOnce(db, cfg)

	ds := NewDrawingStore(db)
	list := ds.List()
	if len(list) != 1 || list[0].Name != "login" {
		t.Fatalf("import lost the drawing: %+v", list)
	}
	if content, err := ds.Content("d1"); err != nil || string(content) != `{"v":"scene"}` {
		t.Fatalf("scene should import byte-for-byte, got %s (%v)", content, err)
	}
	// thumbUpdatedAt == updatedAt in the fixture → the thumbnail imports fresh.
	if b, err := ds.Thumbnail("d1"); err != nil || string(b) != "png-bytes" {
		t.Fatalf("fresh thumbnail should import, got %q (%v)", b, err)
	}
	var bak []byte
	if err := db.QueryRow(`SELECT content FROM drawing_backups WHERE drawing_id='d1' AND slot=1`).Scan(&bak); err != nil || string(bak) != `{"v":"prev"}` {
		t.Fatalf("scene backup should import, got %s (%v)", bak, err)
	}
	if _, err := os.Stat(filepath.Join(cfg, "drawings.json.migrated-backup")); err != nil {
		t.Fatalf("index backup should exist: %v", err)
	}
	if _, err := os.Stat(dir + ".migrated-backup"); err != nil {
		t.Fatalf("scene dir backup should exist: %v", err)
	}
}

func TestBackupDataDB(t *testing.T) {
	cfg := t.TempDir()
	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := NewTodoStore(db).Create("backed up", "", "", ""); err != nil {
		t.Fatal(err)
	}

	backupDataDB(db, cfg)

	bak, err := sql.Open("sqlite", "file:"+filepath.Join(cfg, "data.db.bak"))
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	var n int
	if err := bak.QueryRow(`SELECT COUNT(*) FROM todos`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("backup should hold the board: n=%d err=%v", n, err)
	}
}

// Boot backups rotate three generations (.bak newest, .bak.2, .bak.3) — one
// generation meant any bad state clobbered the only good copy after a single
// boot.
func TestBackupDataDBRotates(t *testing.T) {
	cfg := t.TempDir()
	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ts := NewTodoStore(db)
	// Three "boots", one more card before each: the generations must hold
	// 3 / 2 / 1 cards, newest first.
	for i := 1; i <= 3; i++ {
		if _, err := ts.Create(fmt.Sprintf("card %d", i), "", "", ""); err != nil {
			t.Fatal(err)
		}
		backupDataDB(db, cfg)
	}
	for name, want := range map[string]int{"data.db.bak": 3, "data.db.bak.2": 2, "data.db.bak.3": 1} {
		bak, err := sql.Open("sqlite", "file:"+filepath.Join(cfg, name))
		if err != nil {
			t.Fatal(err)
		}
		var n int
		if err := bak.QueryRow(`SELECT COUNT(*) FROM todos`).Scan(&n); err != nil || n != want {
			t.Fatalf("%s: want %d cards, got %d (%v)", name, want, n, err)
		}
		bak.Close()
	}
	// A fourth boot drops the oldest generation, not the newest.
	if _, err := ts.Create("card 4", "", "", ""); err != nil {
		t.Fatal(err)
	}
	backupDataDB(db, cfg)
	bak, err := sql.Open("sqlite", "file:"+filepath.Join(cfg, "data.db.bak.3"))
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	var n int
	if err := bak.QueryRow(`SELECT COUNT(*) FROM todos`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("oldest generation after 4 boots should hold 2 cards, got %d (%v)", n, err)
	}
}

// The boot that migrates an old data.db forward must first freeze the
// untouched file as data.db.v<N>.bak — the rotating boot snapshots are all
// post-migration, so this is the only copy a buggy migration step can't reach.
// A fresh db (v0) freezes nothing.
func TestPreMigrationBackupFreezesOldSchema(t *testing.T) {
	cfg := t.TempDir()
	raw, err := sql.Open("sqlite", "file:"+filepath.Join(cfg, "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A minimal v5 fixture (see TestMigrateAddsProjectGroupsTable), plus one
	// drawing so the frozen snapshot has content to prove itself with.
	if _, err := raw.Exec(fixtureDrawingsDDL + `
INSERT INTO drawings(id,name,created_at,updated_at,scene) VALUES('d','wf',0,0,'{}');
PRAGMA user_version = 5;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	bak, err := sql.Open("sqlite", "file:"+filepath.Join(cfg, "data.db.v5.bak"))
	if err != nil {
		t.Fatal(err)
	}
	defer bak.Close()
	var v, n int
	if err := bak.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != 5 {
		t.Fatalf("frozen snapshot should still be schema v5, got %d (%v)", v, err)
	}
	if err := bak.QueryRow(`SELECT COUNT(*) FROM drawings`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("frozen snapshot should hold the drawing, got %d (%v)", n, err)
	}

	fresh := t.TempDir()
	db2, err := openDataDB(fresh)
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()
	if m, _ := filepath.Glob(filepath.Join(fresh, "data.db.v*.bak")); len(m) != 0 {
		t.Fatalf("a fresh db must not freeze a pre-migration snapshot, found %v", m)
	}
}

// A pre-v3 data.db (todos table without linked_docs) must gain the column on
// open without losing its cards, and new links must then persist.
func TestMigrateAddsLinkedDocsColumn(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A minimal v2 fixture: the frozen v1 todos + drawings tables +
	// user_version=2, with one card written through the pre-linked_docs column
	// list. (The drawings and docs tables are what a real v2 db has; later
	// steps ALTER them.)
	if _, err := raw.Exec(`
CREATE TABLE todos(
	id TEXT PRIMARY KEY, seq INTEGER NOT NULL, title TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL, ord REAL NOT NULL,
	created_at INTEGER NOT NULL, linked_sessions TEXT NOT NULL DEFAULT '[]',
	linked_drawings TEXT NOT NULL DEFAULT '[]', snapshot TEXT NOT NULL DEFAULT ''
);
CREATE TABLE drawings(
	id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, thumb_updated_at INTEGER NOT NULL DEFAULT 0,
	scene BLOB NOT NULL, thumb BLOB
);
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO todos(id,seq,title,status,ord,created_at) VALUES('old',1,'pre-v3 card','backlog',1,0);
PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open should migrate v2 forward: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d, got %d (%v)", dataSchemaVersion, v, err)
	}
	ts := NewTodoStore(db)
	list := ts.List()
	if len(list) != 1 || list[0].ID != "old" {
		t.Fatalf("pre-v3 card should survive the migration, got %+v", list)
	}
	if len(list[0].LinkedDocIDs) != 0 {
		t.Fatalf("migrated card should read back with no linked docs, got %v", list[0].LinkedDocIDs)
	}
	// The new column is writable and persists across a reload.
	linked, err := ts.Update("old", todoPatch{LinkedDocIDs: &[]string{"docX"}})
	if err != nil {
		t.Fatalf("link doc after migration: %v", err)
	}
	if len(linked.LinkedDocIDs) != 1 || linked.LinkedDocIDs[0] != "docX" {
		t.Fatalf("want [docX], got %v", linked.LinkedDocIDs)
	}
	if got := NewTodoStore(db).List()[0].LinkedDocIDs; len(got) != 1 || got[0] != "docX" {
		t.Fatalf("link should persist across reload, got %v", got)
	}
}

// A crash between the v3 ALTER and its version bump leaves linked_docs present
// but user_version still 2. The next open must recover — skip the ALTER and set
// the version — not re-run ALTER and brick startup with "duplicate column".
func TestMigrateLinkedDocsIdempotentAfterPartialCrash(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The partial-crash state: a todos table that ALREADY has linked_docs, but
	// user_version still says 2. (Plus the drawings and docs tables a real v2
	// db carries.)
	if _, err := raw.Exec(`
CREATE TABLE todos(
	id TEXT PRIMARY KEY, seq INTEGER NOT NULL, title TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '[]', status TEXT NOT NULL, ord REAL NOT NULL,
	created_at INTEGER NOT NULL, linked_sessions TEXT NOT NULL DEFAULT '[]',
	linked_drawings TEXT NOT NULL DEFAULT '[]',
	linked_docs TEXT NOT NULL DEFAULT '[]', snapshot TEXT NOT NULL DEFAULT ''
);
CREATE TABLE drawings(
	id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, thumb_updated_at INTEGER NOT NULL DEFAULT 0,
	scene BLOB NOT NULL, thumb BLOB
);
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO todos(id,seq,title,status,ord,created_at) VALUES('c',1,'card','backlog',1,0);
PRAGMA user_version = 2;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open must recover from a partial v3 migration, got: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d after recovery, got %d (%v)", dataSchemaVersion, v, err)
	}
	// The card and its (already-present) column survived the recovery.
	if got := len(NewTodoStore(db).List()); got != 1 {
		t.Fatalf("want 1 card after recovery, got %d", got)
	}
}

// A data.db written by a NEWER schema must be refused, never "migrated" down.
func TestDataDBRefusesNewerSchema(t *testing.T) {
	cfg := t.TempDir()
	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := openDataDB(cfg); err == nil {
		t.Fatal("newer-schema data.db must refuse to open, not migrate down")
	}
}

// A pre-v4 data.db (drawings table without group_name) must gain the column on
// open without losing its drawings, and moves must then persist.
func TestMigrateAddsGroupColumn(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A v3 fixture: the frozen v1 drawings table (no group_name) + one drawing,
	// at user_version=3. (Plus the docs table a real v3 db carries — the v→5
	// step ALTERs it.)
	if _, err := raw.Exec(`
CREATE TABLE drawings(
	id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL, thumb_updated_at INTEGER NOT NULL DEFAULT 0,
	scene BLOB NOT NULL, thumb BLOB
);
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO drawings(id,name,created_at,updated_at,scene) VALUES('d','pre-v4 wf',0,0,'{}');
PRAGMA user_version = 3;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open should migrate v3→v4: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d, got %d (%v)", dataSchemaVersion, v, err)
	}
	ds := NewDrawingStore(db)
	list := ds.List()
	if len(list) != 1 || list[0].ID != "d" || list[0].Group != "" {
		t.Fatalf("pre-v4 drawing should survive with empty group, got %+v", list)
	}
	if _, err := ds.SetGroup("d", "shop"); err != nil {
		t.Fatalf("set group after migration: %v", err)
	}
	if got := NewDrawingStore(db).List()[0].Group; got != "shop" {
		t.Fatalf("group should persist across reload, got %q", got)
	}
}

// A crash between the v4 ALTER and its version bump leaves group_name present
// but user_version still 3. The next open must recover — skip the ALTER and set
// the version — not re-run ALTER and brick startup with "duplicate column".
func TestMigrateGroupIdempotentAfterPartialCrash(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The partial-crash state: a drawings table that ALREADY has group_name, but
	// user_version still says 3. (Plus the docs table a real v3 db carries.)
	if _, err := raw.Exec(`
CREATE TABLE drawings(
	id TEXT PRIMARY KEY, name TEXT NOT NULL, group_name TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	thumb_updated_at INTEGER NOT NULL DEFAULT 0, scene BLOB NOT NULL, thumb BLOB
);
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO drawings(id,name,created_at,updated_at,scene) VALUES('d','wf',0,0,'{}');
PRAGMA user_version = 3;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open must recover from a partial v4 migration, got: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d after recovery, got %d (%v)", dataSchemaVersion, v, err)
	}
	if got := len(NewDrawingStore(db).List()); got != 1 {
		t.Fatalf("want 1 drawing after recovery, got %d", got)
	}
}

// A pre-v5 data.db (docs table without group_name) must gain the column on open
// without losing its pages, and group moves must then persist.
func TestMigrateAddsDocGroupColumn(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A v4 fixture: the frozen v2 docs table (no group_name) + one page, at
	// user_version=4 — plus the drawings table a real v4 db carries.
	if _, err := raw.Exec(fixtureDrawingsDDL + `
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO docs(id,title,ord,created_at,updated_at) VALUES('p','pre-v5 page',1,0,0);
PRAGMA user_version = 4;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open should migrate v4→v5: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d, got %d (%v)", dataSchemaVersion, v, err)
	}
	ds := NewDocStore(db)
	list := ds.List()
	if len(list) != 1 || list[0].ID != "p" || list[0].Group != "" {
		t.Fatalf("pre-v5 page should survive with empty group, got %+v", list)
	}
	g := "shop"
	if _, err := ds.Update("p", docPatch{Group: &g}); err != nil {
		t.Fatalf("set group after migration: %v", err)
	}
	if got := NewDocStore(db).List()[0].Group; got != "shop" {
		t.Fatalf("group should persist across reload, got %q", got)
	}
}

// A crash between the v5 ALTER and its version bump leaves docs.group_name
// present but user_version still 4. The next open must recover — skip the ALTER
// and set the version — not re-run ALTER and brick startup with "duplicate
// column".
func TestMigrateDocGroupIdempotentAfterPartialCrash(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// The partial-crash state: a docs table that ALREADY has group_name, but
	// user_version still says 4 (drawings present, as on any real v4 db).
	if _, err := raw.Exec(fixtureDrawingsDDL + `
CREATE TABLE docs(
	id TEXT PRIMARY KEY, title TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
	group_name TEXT NOT NULL DEFAULT '', ord REAL NOT NULL,
	created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
INSERT INTO docs(id,title,ord,created_at,updated_at) VALUES('p','page',1,0,0);
PRAGMA user_version = 4;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open must recover from a partial v5 migration, got: %v", err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d after recovery, got %d (%v)", dataSchemaVersion, v, err)
	}
	if got := len(NewDocStore(db).List()); got != 1 {
		t.Fatalf("want 1 page after recovery, got %d", got)
	}
}

// A pre-v6 data.db must gain the project_groups table on open, and groups must
// then persist. (CREATE IF NOT EXISTS makes the step idempotent by itself, so
// there is no partial-crash twin for this one.)
func TestMigrateAddsProjectGroupsTable(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A minimal v5 fixture: the v→6 step touches nothing that exists yet, but
	// the v→10 step ALTERs the drawings table every real v5 db carries.
	if _, err := raw.Exec(fixtureDrawingsDDL + `PRAGMA user_version = 5;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := openDataDB(cfg)
	if err != nil {
		t.Fatalf("open should migrate v5→v6: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d, got %d (%v)", dataSchemaVersion, v, err)
	}
	gs := NewGroupStore(db)
	if _, err := gs.Upsert("studio", []string{"blog", "wyac"}); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	if got := NewGroupStore(db).List(); len(got) != 1 || len(got[0].Projects) != 2 {
		t.Fatalf("group should persist across reload, got %+v", got)
	}
}
