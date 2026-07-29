package index

// The on-disk index cache: parsed sessions plus searchable message text, one
// SQLite file per config dir (<config-dir>/index.db). The cache is disposable
// by design — the JSONL transcripts stay the source of truth, and anything
// suspicious (schema bump, different binary, different root, a blob that no
// longer decodes) answers with a wipe and a re-parse, never a migration.

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// indexSchemaVersion participates in the generation stamp below. The binary
// identity already invalidates on every rebuild, so this only matters for a
// wipe that must happen even when the binary didn't change — belt, suspender.
const indexSchemaVersion = 1

// OpenDB opens <cfgDir>/index.db, creating or wiping as needed. A file
// that can't even be initialized is removed and recreated once — a corrupt
// cache must never keep the viewer from starting.
func OpenDB(cfgDir, root string) (*sql.DB, error) {
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(cfgDir, "index.db")
	db, err := tryOpenIndexDB(path, root)
	if err == nil {
		return db, nil
	}
	if rmErr := os.Remove(path); rmErr != nil {
		return nil, err
	}
	// Take the WAL siblings with it: a stale index.db-wal left beside a
	// recreated file would be replayed into the fresh database on open.
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
	log.Printf("index cache: unusable (%v), recreating", err)
	return tryOpenIndexDB(path, root)
}

func tryOpenIndexDB(path, root string) (*sql.DB, error) {
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	if err := initIndexDB(db, root); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// initIndexDB creates the schema and wipes cached rows when the generation —
// schema version, binary identity, transcript root — changed. Correct beats
// clever: the worst case is one full re-parse per release.
func initIndexDB(db *sql.DB, root string) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(
	path TEXT PRIMARY KEY,
	id TEXT NOT NULL,
	project TEXT NOT NULL,
	stamp_mod INTEGER NOT NULL,
	stamp_size INTEGER NOT NULL,
	stamp_sub TEXT NOT NULL,
	parsed BLOB NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_id ON sessions(id);
CREATE VIRTUAL TABLE IF NOT EXISTS messages USING fts5(
	text, session_id UNINDEXED, role UNINDEXED, ts UNINDEXED,
	tokenize='unicode61 remove_diacritics 2'
);
CREATE TABLE IF NOT EXISTS ships(
	file TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	kind TEXT NOT NULL,
	version TEXT NOT NULL DEFAULT '',
	sha TEXT NOT NULL DEFAULT '',
	exit INTEGER NOT NULL,
	duration_ms INTEGER NOT NULL,
	ts INTEGER NOT NULL,
	log TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ships_project_ts ON ships(project, ts);`)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("%d|%s|%s", indexSchemaVersion, binaryIdentity(), root)
	var have string
	err = db.QueryRow(`SELECT value FROM meta WHERE key='generation'`).Scan(&have)
	if err == nil && have == want {
		return nil
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	if _, err := db.Exec(`DELETE FROM messages`); err != nil {
		return err
	}
	// Ships re-ingest from their drop files on the next ScanShips.
	if _, err := db.Exec(`DELETE FROM ships`); err != nil {
		return err
	}
	if have != "" {
		log.Printf("index cache: generation changed, rebuilding from transcripts")
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO meta(key,value) VALUES('generation',?)`, want)
	return err
}

// binaryIdentity fingerprints the running executable (mtime+size). Any rebuilt
// binary — a release or an air rebuild — invalidates the whole cache: parse
// output may have changed shape or meaning, and stale-but-plausible numbers
// are worse than a one-second rescan.
func binaryIdentity() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}

// subSigHash collapses the (unbounded) subagent signature to a fixed column.
func subSigHash(sig string) string {
	h := sha256.Sum256([]byte(sig))
	return hex.EncodeToString(h[:])
}

// dbLookup returns the cached parse for path if the stored stamp matches st.
// Any failure — missing row, stale stamp, gob drift — is just a cache miss.
func (ix *Index) dbLookup(path string, st sessionStamp) (*Session, bool) {
	if ix.db == nil {
		return nil, false
	}
	var blob []byte
	err := ix.db.QueryRow(
		`SELECT parsed FROM sessions WHERE path=? AND stamp_mod=? AND stamp_size=? AND stamp_sub=?`,
		path, st.mainMod.UnixNano(), st.mainSz, subSigHash(st.subSig),
	).Scan(&blob)
	if err != nil {
		return nil, false
	}
	var s Session
	if gob.NewDecoder(bytes.NewReader(blob)).Decode(&s) != nil {
		return nil, false
	}
	return &s, true
}

// persistReq carries one freshly parsed session to the cache: the blob and the
// searchable text rows that replace whatever the cache held for it.
type persistReq struct {
	path  string
	stamp sessionStamp
	sess  *Session
	texts []textTuple
}

// dbPersist consumes a Rescan's parse results in one transaction. Persist
// failures only cost the cache, never the scan — the session is already live
// in memory by the time it arrives here.
func (ix *Index) dbPersist(reqs <-chan persistReq) {
	tx, err := ix.db.Begin()
	if err != nil {
		for range reqs {
		}
		log.Printf("index cache: persist skipped: %v", err)
		return
	}
	for r := range reqs {
		if err := persistOne(tx, r); err != nil {
			log.Printf("index cache: persist %s: %v", r.sess.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("index cache: commit: %v", err)
	}
}

func persistOne(tx *sql.Tx, r persistReq) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(r.sess); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO sessions(path,id,project,stamp_mod,stamp_size,stamp_sub,parsed) VALUES(?,?,?,?,?,?,?)`,
		r.path, r.sess.ID, r.sess.Project, r.stamp.mainMod.UnixNano(), r.stamp.mainSz,
		subSigHash(r.stamp.subSig), buf.Bytes()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id=?`, r.sess.ID); err != nil {
		return err
	}
	for _, t := range r.texts {
		if _, err := tx.Exec(`INSERT INTO messages(text,session_id,role,ts) VALUES(?,?,?,?)`,
			t.Text, r.sess.ID, t.Role, t.Ts.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}

// dbPersistOne is the watcher-path variant: one session, its own transaction.
func (ix *Index) dbPersistOne(r persistReq) {
	if ix.db == nil {
		return
	}
	tx, err := ix.db.Begin()
	if err != nil {
		log.Printf("index cache: persist %s: %v", r.sess.ID, err)
		return
	}
	if err := persistOne(tx, r); err != nil {
		tx.Rollback()
		log.Printf("index cache: persist %s: %v", r.sess.ID, err)
		return
	}
	if err := tx.Commit(); err != nil {
		log.Printf("index cache: persist %s: %v", r.sess.ID, err)
	}
}

// dbPrune drops cached sessions whose transcript no longer exists on disk —
// without this, a deleted session would resurrect from the cache forever.
func (ix *Index) dbPrune(keep map[string]bool) {
	if ix.db == nil {
		return
	}
	rows, err := ix.db.Query(`SELECT path, id FROM sessions`)
	if err != nil {
		return
	}
	type orphan struct{ path, id string }
	var gone []orphan
	for rows.Next() {
		var o orphan
		if rows.Scan(&o.path, &o.id) == nil && !keep[o.path] {
			gone = append(gone, o)
		}
	}
	rows.Close()
	for _, o := range gone {
		ix.db.Exec(`DELETE FROM sessions WHERE path=?`, o.path)
		ix.db.Exec(`DELETE FROM messages WHERE session_id=?`, o.id)
	}
}
