package ships

// Ship history: what actually went out, across every solo project.
//
// Since Actions went off (2026-07-17), `make check` and `make release` run
// locally and used to leave no trace. Now each run drops one JSON file into
// the ships dir (default ~/.wyac/ships, see scripts/wyac-ship) — exit code,
// duration, log tail, success or failure. The drop files are the source of
// truth exactly like the JSONL transcripts are for sessions: this file scans
// them into the `ships` table of index.db (an index, wiped and rebuilt at
// will), watches the dir for new drops, and prunes rows whose file is gone.
// The viewer being down never loses a record — the file just waits.
//
// Not to be confused with the cost-per-outcome insights at /api/ledger.

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watch-your-ai-code/internal/index"
)

// maxShipFile bounds one drop record; the writer tails its log to ~200 lines,
// so anything bigger is not ours and gets skipped rather than swallowed.
const maxShipFile = 1 << 20

// ShipRecord is one recorded run. File doubles as its identity — the basename
// in the ships dir — so re-scans dedupe naturally.
type ShipRecord struct {
	File       string    `json:"file"`
	Project    string    `json:"project"`
	Kind       string    `json:"kind"` // check | release
	Version    string    `json:"version,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Exit       int       `json:"exit"`
	DurationMs int64     `json:"durationMs"`
	Ts         time.Time `json:"ts"`
	Log        string    `json:"log,omitempty"`
	// SessionID/SessionTitle name the session that was running this project
	// when the run happened — joined at read time against the in-memory
	// session index, never stored: the drop-file format stays as it is, and
	// a record from outside any session simply carries neither.
	SessionID    string `json:"sessionId,omitempty"`
	SessionTitle string `json:"sessionTitle,omitempty"`
}

// ShipsResult is a capped list that must never read as the whole answer:
// Total counts everything the filter matched.
type ShipsResult struct {
	Ships []ShipRecord `json:"ships"`
	Total int          `json:"total"`
}

// DefaultDir is where drop records land unless -ships-dir says otherwise.
// A fixed, HOME-anchored path on purpose: every project's Makefile must be
// able to name it without knowing this app's config dir.
func DefaultDir() string {
	return filepath.Join(os.Getenv("HOME"), ".wyac", "ships")
}

// Sessions is the slice of the session index that ship records join
// against — which session was running a project when a run happened. Taking
// this rather than the whole *Index lets ship records live outside the
// index's package.
type Sessions interface {
	Snapshot() []*index.Session
}

// Store is the ships table of index.db (whose schema the index owns, see
// db.go) plus the session join. A nil db means the index cache is disabled:
// every method then answers "no rows" rather than failing.
type Store struct {
	db       *sql.DB
	sessions Sessions
}

func New(db *sql.DB, ss Sessions) *Store {
	return &Store{db: db, sessions: ss}
}

// Scan reconciles the ships table with dir: new *.json files are ingested,
// rows whose file is gone are pruned. Returns the newly ingested records.
// Safe to call repeatedly — known files are skipped by name.
func (st *Store) Scan(dir string) []*ShipRecord {
	if st.db == nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no dir yet: nothing shipped anywhere, not an error
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		onDisk[name] = true
	}

	known := map[string]bool{}
	if rows, err := st.db.Query(`SELECT file FROM ships`); err == nil {
		for rows.Next() {
			var f string
			if rows.Scan(&f) == nil {
				known[f] = true
			}
		}
		rows.Close()
	}

	var ingested []*ShipRecord
	for name := range onDisk {
		if known[name] {
			continue
		}
		if r := st.ingest(dir, name); r != nil {
			ingested = append(ingested, r)
		}
	}
	for f := range known {
		if !onDisk[f] {
			st.db.Exec(`DELETE FROM ships WHERE file=?`, f)
		}
	}
	return ingested
}

// ingestShip reads one drop file into the ships table. A file that isn't a
// valid record (foreign JSON, missing fields, oversized) is skipped with a
// log line — one bad drop must never sink the scan.
func (st *Store) ingest(dir, name string) *ShipRecord {
	if st.db == nil {
		return nil
	}
	fi, err := os.Stat(filepath.Join(dir, name))
	if err != nil || fi.Size() > maxShipFile {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil
	}
	var r ShipRecord
	if err := json.Unmarshal(raw, &r); err != nil || r.Project == "" || r.Kind == "" {
		log.Printf("ships: skipping %s: not a ship record", name)
		return nil
	}
	r.File = name
	if _, err := st.db.Exec(
		`INSERT OR REPLACE INTO ships(file,project,kind,version,sha,exit,duration_ms,ts,log) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.File, r.Project, r.Kind, r.Version, r.SHA, r.Exit, r.DurationMs, r.Ts.UnixNano(), r.Log); err != nil {
		log.Printf("ships: ingest %s: %v", name, err)
		return nil
	}
	return &r
}

// Ships lists recorded runs, newest first, filtered by project and window.
// `project` may name several projects comma-separated: the scoped ships tab
// sends its whole scope set, so a small project's old records aren't starved
// out of the capped newest-first slice by other projects' volume (which is
// what client-side scope filtering over that slice did).
// Logs ride along only when withLog asks — they are the payload's whole weight.
func (st *Store) List(project string, days, limit int, withLog bool) ShipsResult {
	res := ShipsResult{Ships: []ShipRecord{}}
	if st.db == nil {
		return res
	}
	var cutoff int64
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days).UnixNano()
	}
	if limit <= 0 {
		limit = -1
	}
	var names []any
	for _, p := range strings.Split(project, ",") {
		if p = strings.TrimSpace(p); p != "" {
			names = append(names, p)
		}
	}
	where := `ts>=?`
	args := []any{cutoff}
	if len(names) > 0 {
		where = `project IN (?` + strings.Repeat(`,?`, len(names)-1) + `) AND ` + where
		args = append(append([]any{}, names...), args...)
	}
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM ships WHERE `+where, args...).Scan(&res.Total); err != nil {
		return res
	}
	rows, err := st.db.Query(
		`SELECT file,project,kind,version,sha,exit,duration_ms,ts,log FROM ships
		 WHERE `+where+` ORDER BY ts DESC LIMIT ?`,
		append(append([]any{}, args...), limit)...)
	if err != nil {
		return res
	}
	defer rows.Close()
	for rows.Next() {
		var r ShipRecord
		var ns int64
		if rows.Scan(&r.File, &r.Project, &r.Kind, &r.Version, &r.SHA, &r.Exit, &r.DurationMs, &ns, &r.Log) != nil {
			continue
		}
		if ns != 0 {
			r.Ts = time.Unix(0, ns)
		}
		if !withLog {
			r.Log = ""
		}
		res.Ships = append(res.Ships, r)
	}
	st.joinSessions(res.Ships)
	return res
}

// joinShipSessions attaches to each record the session that was running its
// project when the run happened: same project, StartedAt ≤ ts ≤ EndedAt plus
// a small slack, since the drop file lands moments after the transcript's
// last line. Overlapping sessions on one repo resolve to the latest-started —
// in practice the one that actually ran the command.
func (st *Store) joinSessions(ships []ShipRecord) {
	const slack = 5 * time.Minute
	sessions := st.sessions.Snapshot()
	for i := range ships {
		r := &ships[i]
		if r.Ts.IsZero() {
			continue
		}
		var best *index.Session
		for _, s := range sessions {
			if s.Project != r.Project || r.Ts.Before(s.StartedAt) || r.Ts.After(s.EndedAt.Add(slack)) {
				continue
			}
			if best == nil || s.StartedAt.After(best.StartedAt) {
				best = s
			}
		}
		if best != nil {
			r.SessionID = best.ID
			r.SessionTitle = best.Title
		}
	}
}

// Watch polls for new drop records and calls onShip with each. Scan is
// already the reconcile — ReadDir plus a known-files set — so the poll just
// runs it on a short tick; no fsnotify, whose kqueue backend held an fd per
// file in the dir. A half-written foreign drop fails to ingest and is retried
// whole on the next tick, which is what the old debounce approximated;
// deletions reconcile here too, so no separate periodic Scan is needed.
func Watch(st *Store, dir string, every time.Duration, onShip func(*ShipRecord)) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	go func() {
		for range time.Tick(every) {
			for _, r := range st.Scan(dir) {
				onShip(r)
			}
		}
	}()
	return nil
}
