package board

// data.db: the durable half of the storage story. index.db (db.go) is a
// disposable cache over files that can always be re-read; data.db holds the
// things that CANNOT be rebuilt — the todo board and the design library.
// Different lifecycle, different rules:
//
//   - NEVER wiped. Schema changes are additive migrations gated on
//     user_version; there is no "generation" here and no rebuild path.
//   - Writes touch explicit column lists, never DELETE-and-reinsert, so a
//     binary that predates a column leaves that column alone — the trap where
//     an old binary silently wiped fields it didn't know about dies here.
//   - Every boot snapshots the whole file to data.db.bak (VACUUM INTO, which
//     is WAL-safe where a plain file copy is not), rotating the two previous
//     snapshots to .bak.2/.bak.3 — one generation meant any bad state
//     clobbered the only good copy after a single boot. A boot that is about
//     to migrate first freezes the untouched file as data.db.v<N>.bak: the
//     rotating snapshots are all post-migration, so without it the one state
//     a buggy migration step can't be undone from would never be captured.
//
// The one-time import from the pre-v0.45 files (todos.json, drawings.json +
// drawings/) renames the originals to *.migrated-backup rather than deleting
// them. If todos.json ever reappears afterwards, a pre-v0.45 binary has been
// run against this config dir: its writes are NOT merged, only reported.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

const dataSchemaVersion = 12

// OpenDB opens (creating if needed) <cfgDir>/data.db and migrates its
// schema forward. Unlike the index cache, an error here is fatal to the
// caller: refusing to start beats starting over the user's board.
func OpenDB(cfgDir string) (*sql.DB, error) {
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(cfgDir, "data.db")
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	if err := preMigrationBackup(db, cfgDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("data.db pre-migration backup: %w", err)
	}
	if err := migrateDataDB(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("data.db migrate: %w", err)
	}
	return db, nil
}

// preMigrationBackup freezes data.db as data.db.v<N>.bak when this boot is
// about to migrate schema v<N> forward. The rotating boot snapshots are all
// taken AFTER migration, so this is the only copy of the state a buggy
// migration step would mangle. One file per schema version, never rotated
// away; a fresh db (v0) has nothing worth freezing. Failure is fatal to the
// open, like migration itself: upgrading without the net defeats the net.
func preMigrationBackup(db *sql.DB, cfgDir string) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v == 0 || v >= dataSchemaVersion {
		return nil
	}
	bak := filepath.Join(cfgDir, fmt.Sprintf("data.db.v%d.bak", v))
	if _, err := os.Stat(bak); err == nil {
		return nil // a crash mid-migration already froze this version
	}
	return vacuumInto(db, bak)
}

// migrateDataDB walks user_version up one step at a time. Steps are additive
// only — a wipe is never a migration.
func migrateDataDB(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v > dataSchemaVersion {
		return fmt.Errorf("data.db is schema v%d, this binary knows v%d — refusing to touch newer data", v, dataSchemaVersion)
	}
	if v < 1 {
		if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS todos(
	id TEXT PRIMARY KEY,
	seq INTEGER NOT NULL,
	title TEXT NOT NULL,
	note TEXT NOT NULL DEFAULT '',
	repo TEXT NOT NULL DEFAULT '',
	labels TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL,
	ord REAL NOT NULL,
	created_at INTEGER NOT NULL,
	linked_sessions TEXT NOT NULL DEFAULT '[]',
	linked_drawings TEXT NOT NULL DEFAULT '[]',
	snapshot TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS drawings(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	thumb_updated_at INTEGER NOT NULL DEFAULT 0,
	scene BLOB NOT NULL,
	thumb BLOB
);
CREATE TABLE IF NOT EXISTS drawing_backups(
	drawing_id TEXT NOT NULL,
	slot INTEGER NOT NULL,
	content BLOB NOT NULL,
	PRIMARY KEY(drawing_id, slot)
);
PRAGMA user_version = 1;`); err != nil {
			return err
		}
	}
	if v < 2 {
		// The docs wiki (`#/docs`): a tree of markdown pages. parent_id ""
		// = a root page; ord sorts siblings. body is the page's markdown,
		// written through its own column (never DELETE-and-reinsert) and
		// rotated into doc_backups on every overwrite, same as drawings.
		if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS docs(
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	ord REAL NOT NULL,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	body TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS doc_backups(
	doc_id TEXT NOT NULL,
	slot INTEGER NOT NULL,
	content TEXT NOT NULL,
	PRIMARY KEY(doc_id, slot)
);
PRAGMA user_version = 2;`); err != nil {
			return err
		}
	}
	if v < 3 {
		// Board cards can link docs, the way they already link wireframes — one
		// more additive column on the existing todos table (existing cards read
		// back "[]", the empty list). ALTER TABLE ADD COLUMN is not idempotent
		// the way CREATE TABLE IF NOT EXISTS is, so guard it on the column's
		// absence: a crash between the ALTER and the version bump must not re-run
		// the ALTER on the next boot (it would fail "duplicate column" and brick
		// every startup after). With the guard, re-entry just sets the version.
		has, err := columnExists(db, "todos", "linked_docs")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE todos ADD COLUMN linked_docs TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
			return err
		}
	}
	if v < 4 {
		// Designs can be split into tabs (`#/design`): each drawing carries an
		// optional group label — a project name or a free-text custom tab. One
		// more additive column on the drawings table (existing drawings read
		// back "", the Ungrouped tab). Guarded on the column's absence like the
		// linked_docs ALTER above, so a crash between the ALTER and the version
		// bump doesn't re-run it and brick every later boot.
		has, err := columnExists(db, "drawings", "group_name")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE drawings ADD COLUMN group_name TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
			return err
		}
	}
	if v < 5 {
		// Docs can carry a project scope (`#/docs` under the global project
		// switcher): each page gets an optional group label, same shape as
		// drawings.group_name — meaningful on root pages, the subtree follows
		// its root. Existing pages read back "", unscoped. Guarded on the
		// column's absence like the ALTERs above.
		has, err := columnExists(db, "docs", "group_name")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE docs ADD COLUMN group_name TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
			return err
		}
	}
	if v < 6 {
		// Project groups (the nav's global scope): one row per named set of
		// project names, members as a JSON array — the todos.labels idiom. A
		// brand-new table, so CREATE IF NOT EXISTS is idempotent on its own
		// and no ALTER guard is needed.
		if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS project_groups(
	name TEXT PRIMARY KEY,
	projects TEXT NOT NULL DEFAULT '[]'
);
PRAGMA user_version = 6;`); err != nil {
			return err
		}
	}
	if v < 7 {
		// Project registry (the nav's global scope, decoupled from the raw
		// ~/.claude scan): one row per user-owned project. folders is the JSON
		// array of Claude session cwd-basenames it owns (the todos.labels
		// idiom); hidden keeps it off the rail without losing its data; ord is
		// the rail order. Seeded at boot from today's taxonomy (projects.go),
		// so an empty table is the correct starting point. Brand-new table, so
		// CREATE IF NOT EXISTS is idempotent on its own.
		if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS projects(
	name TEXT PRIMARY KEY,
	folders TEXT NOT NULL DEFAULT '[]',
	hidden INTEGER NOT NULL DEFAULT 0,
	ord INTEGER NOT NULL DEFAULT 0
);
PRAGMA user_version = 7;`); err != nil {
			return err
		}
	}
	if v < 8 {
		// Per-project logo: the image bytes, their content-type, and the write
		// time (a cache-buster the client puts in the img URL). All nullable /
		// default-empty; guarded ADD COLUMNs so a crash between them and the
		// version bump replays cleanly.
		for _, col := range []struct{ name, ddl string }{
			{"logo", `ALTER TABLE projects ADD COLUMN logo BLOB`},
			{"logo_type", `ALTER TABLE projects ADD COLUMN logo_type TEXT NOT NULL DEFAULT ''`},
			{"logo_updated_at", `ALTER TABLE projects ADD COLUMN logo_updated_at INTEGER NOT NULL DEFAULT 0`},
		} {
			has, err := columnExists(db, "projects", col.name)
			if err != nil {
				return err
			}
			if !has {
				if _, err := db.Exec(col.ddl); err != nil {
					return err
				}
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 8`); err != nil {
			return err
		}
	}
	if v < 9 {
		// Project parent: an optional tree edge — a project can nest under
		// another (holding the parent's name), so e.g. per-repo projects hang
		// under one umbrella project in the rail. Empty = top-level. Guarded ADD
		// COLUMN, like the logo columns, so a crash before the version bump
		// replays cleanly.
		has, err := columnExists(db, "projects", "parent")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE projects ADD COLUMN parent TEXT NOT NULL DEFAULT ''`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 9`); err != nil {
			return err
		}
	}
	if v < 10 {
		// Drawing topics (`#/design`): free-text tags, a JSON array (the
		// todos.labels idiom) — many-to-many, unlike group_name's one tab.
		// The scoped grid renders one section per topic, a drawing under each
		// of its tags. Existing drawings read back "[]", untagged. Guarded ADD
		// COLUMN like the ones above.
		has, err := columnExists(db, "drawings", "topics")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE drawings ADD COLUMN topics TEXT NOT NULL DEFAULT '[]'`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
			return err
		}
	}
	if v < 11 {
		// Drawing publish state (`#/design` share): the UpdatedAt that was last
		// pushed to the review backend, unix-nanos, 0 = never published. Same
		// freshness idiom as thumb_updated_at — the published copy is current
		// iff published_at == updated_at. Guarded ADD COLUMN like the ones above.
		has, err := columnExists(db, "drawings", "published_at")
		if err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(`ALTER TABLE drawings ADD COLUMN published_at INTEGER NOT NULL DEFAULT 0`); err != nil {
				return err
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 11`); err != nil {
			return err
		}
	}
	if v < 12 {
		// Board depth (`#/board`): custom workflow columns, card hierarchy,
		// cycles, an append-only event log and saved views. Four new tables plus
		// five guarded ADD COLUMNs on todos.
		//
		// todo_states is SEEDED with the three ids the old enum used, so `status`
		// becomes a reference into this table without migrating a single card:
		// every existing row, REST body and MCP call keeps its exact string. The
		// seed is INSERT OR IGNORE so a re-entered migration (crash between the
		// CREATE and the version bump) neither fails nor resurrects a column the
		// user renamed.
		//
		// todo_events is the only history the board has: current state alone
		// cannot draw a burndown, because it never says WHEN a card crossed into
		// done. It is append-only and never updated, which is also what makes it
		// safe to replay.
		if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS todo_states(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	category TEXT NOT NULL,
	ord REAL NOT NULL,
	wip_limit INTEGER NOT NULL DEFAULT 0,
	repo TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO todo_states(id,name,category,ord) VALUES
	('backlog','Backlog','todo',0),
	('doing','Doing','started',1),
	('done','Done','done',2);
CREATE TABLE IF NOT EXISTS cycles(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	goal TEXT NOT NULL DEFAULT '',
	starts_at INTEGER NOT NULL,
	ends_at INTEGER NOT NULL,
	closed_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS todo_events(
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	todo_id TEXT NOT NULL,
	ts INTEGER NOT NULL,
	kind TEXT NOT NULL,
	from_val TEXT NOT NULL DEFAULT '',
	to_val TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS todo_events_todo ON todo_events(todo_id);
CREATE INDEX IF NOT EXISTS todo_events_ts ON todo_events(ts);
CREATE TABLE IF NOT EXISTS board_views(
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	repo TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	query TEXT NOT NULL DEFAULT '{}',
	ord REAL NOT NULL
);`); err != nil {
			return err
		}
		// The ALTERs are skipped outright when todos is absent. A step must not
		// assume a table an earlier step created: a db repaired by hand, or a
		// fixture that only ever held drawings, still has to migrate forward
		// rather than failing every boot on a table it never had.
		hasTodos, err := tableExists(db, "todos")
		if err != nil {
			return err
		}
		if hasTodos {
			for _, col := range []struct{ name, ddl string }{
				{"parent_id", `ALTER TABLE todos ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`},
				{"kind", `ALTER TABLE todos ADD COLUMN kind TEXT NOT NULL DEFAULT 'task'`},
				{"priority", `ALTER TABLE todos ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`},
				{"estimate", `ALTER TABLE todos ADD COLUMN estimate REAL NOT NULL DEFAULT 0`},
				{"cycle_id", `ALTER TABLE todos ADD COLUMN cycle_id TEXT NOT NULL DEFAULT ''`},
			} {
				has, err := columnExists(db, "todos", col.name)
				if err != nil {
					return err
				}
				if !has {
					if _, err := db.Exec(col.ddl); err != nil {
						return err
					}
				}
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
			return err
		}
	}
	return nil
}

// tableExists reports whether the named table is present, so a migration step
// can skip an ALTER rather than fail on a db that never had the table.
func tableExists(db *sql.DB, table string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// columnExists reports whether table has a column named col — used to keep an
// ADD COLUMN migration idempotent across a crash between the ALTER and its
// version bump.
func columnExists(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Backup snapshots data.db to data.db.bak, rotating the two previous
// snapshots to .bak.2/.bak.3 (oldest dropped) — three boots of history instead
// of one, so a bad state can't clobber the only good copy on the next boot.
func Backup(db *sql.DB, cfgDir string) {
	bak := filepath.Join(cfgDir, "data.db.bak")
	tmp := bak + ".tmp"
	_ = os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		log.Printf("data.db backup: %v", err)
		return
	}
	// Rotate only once the new snapshot is safely on disk — a failed VACUUM
	// must not shuffle the good generations around a hole.
	_ = os.Remove(bak + ".3")
	_ = os.Rename(bak+".2", bak+".3")
	_ = os.Rename(bak, bak+".2")
	if err := os.Rename(tmp, bak); err != nil {
		log.Printf("data.db backup: %v", err)
	}
}

// vacuumInto snapshots db into dst through a temp file: VACUUM INTO writes a
// consistent copy even mid-WAL, and the rename keeps a crash from leaving a
// half-written file under the final name.
func vacuumInto(db *sql.DB, dst string) error {
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	if _, err := db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// ImportOnce moves the pre-v0.45 file stores into data.db, exactly once.
// Empty table + file present = import then rename the file away; non-empty
// table + file present = an old binary ran and wrote a fresh file — report
// loudly, merge nothing.
func ImportOnce(db *sql.DB, cfgDir string) {
	importTodosOnce(db, cfgDir)
	importDrawingsOnce(db, cfgDir)
}

func tableEmpty(db *sql.DB, table string) bool {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		return false // when in doubt, do not import over it
	}
	return n == 0
}

func importTodosOnce(db *sql.DB, cfgDir string) {
	path := filepath.Join(cfgDir, "todos.json")
	if _, err := os.Stat(path); err != nil {
		return // nothing to import
	}
	if !tableEmpty(db, "todos") {
		log.Printf("data.db: todos.json REAPPEARED after migration — a pre-v0.45 binary ran against this config dir; its board writes are NOT merged (file left in place for manual review)")
		return
	}
	// Read through the legacy loader: it carries the pre-v0.35 backfills and
	// the corrupt-file set-aside, so the import sees exactly what the old
	// binary would have served.
	ts := &TodoStore{}
	ts.loadFromFile(path)
	tx, err := db.Begin()
	if err != nil {
		log.Printf("data.db: import todos: %v", err)
		return
	}
	for _, t := range ts.todos {
		if err := upsertTodoTx(tx, t); err != nil {
			tx.Rollback()
			log.Printf("data.db: import todos: %v — todos.json left untouched", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("data.db: import todos: %v", err)
		return
	}
	if err := os.Rename(path, path+".migrated-backup"); err != nil {
		log.Printf("data.db: import todos: backup rename: %v", err)
		return
	}
	log.Printf("data.db: imported %d todos from todos.json (original kept as todos.json.migrated-backup)", len(ts.todos))
}

func importDrawingsOnce(db *sql.DB, cfgDir string) {
	indexPath := filepath.Join(cfgDir, "drawings.json")
	dir := filepath.Join(cfgDir, "drawings")
	if _, err := os.Stat(indexPath); err != nil {
		return
	}
	if !tableEmpty(db, "drawings") {
		log.Printf("data.db: drawings.json REAPPEARED after migration — a pre-v0.45 binary ran against this config dir; its writes are NOT merged")
		return
	}
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		return
	}
	var metas []*Drawing
	if err := json.Unmarshal(raw, &metas); err != nil {
		bad := indexPath + ".corrupt"
		_ = os.Rename(indexPath, bad)
		log.Printf("data.db: drawings.json is corrupt (%v), moved to %s", err, bad)
		return
	}
	tx, err := db.Begin()
	if err != nil {
		log.Printf("data.db: import drawings: %v", err)
		return
	}
	for _, d := range metas {
		scene, err := os.ReadFile(filepath.Join(dir, d.ID+".excalidraw"))
		if err != nil {
			scene = []byte(emptyScene) // same degradation Content() always had
		}
		var thumb []byte // nil is fine: stale-thumbnail logic regenerates
		if b, err := os.ReadFile(filepath.Join(dir, d.ID+".thumb.png")); err == nil {
			thumb = b
		}
		if _, err := tx.Exec(
			`INSERT INTO drawings(id,name,created_at,updated_at,thumb_updated_at,scene,thumb) VALUES(?,?,?,?,?,?,?)`,
			d.ID, d.Name, d.CreatedAt.UnixNano(), d.UpdatedAt.UnixNano(),
			timeToNano(d.ThumbUpdatedAt), scene, thumb); err != nil {
			tx.Rollback()
			log.Printf("data.db: import drawings: %v — files left untouched", err)
			return
		}
		for slot := 1; slot <= maxSceneBackups; slot++ {
			b, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%s.excalidraw.bak.%d", d.ID, slot)))
			if err != nil {
				continue
			}
			if _, err := tx.Exec(
				`INSERT INTO drawing_backups(drawing_id,slot,content) VALUES(?,?,?)`,
				d.ID, slot, b); err != nil {
				tx.Rollback()
				log.Printf("data.db: import drawings: %v — files left untouched", err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("data.db: import drawings: %v", err)
		return
	}
	if err := os.Rename(indexPath, indexPath+".migrated-backup"); err != nil {
		log.Printf("data.db: import drawings: backup rename: %v", err)
		return
	}
	if err := os.Rename(dir, dir+".migrated-backup"); err != nil {
		log.Printf("data.db: import drawings: dir backup rename: %v", err)
	}
	log.Printf("data.db: imported %d drawings (originals kept as *.migrated-backup)", len(metas))
}

// timeToNano stores the zero time as 0, round-tripping IsZero through int64.
func timeToNano(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func nanoToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}
