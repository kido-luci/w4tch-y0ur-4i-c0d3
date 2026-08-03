package codegraph

// Code graph — a read-only window onto each repo's .codegraph/codegraph.db,
// the SQLite knowledge graph the codegraph MCP indexer maintains (nodes =
// symbols, edges = calls/imports/…, files). wyac never writes those DBs and
// never runs the indexer; it only renders what's there, plus an honest "how
// old is this" signal so a stale index reads as stale instead of as truth.
//
// Finding a DB is a two-step hop: the scope's Claude folders → each folder's
// most recent session cwd (tracked on Session.CWD) → <cwd>/.codegraph/. The
// handlers only ever open a root that came out of that resolution — a ?repo
// param that isn't in the resolved set is rejected, so the endpoints can't be
// steered at arbitrary filesystem paths.

import (
	"context"
	"database/sql"
	"github.com/go-chi/chi/v5"
	"net/http"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"

	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/repos"
)

// cgFile / cgEdge / cgGraph: the file-level graph payload. Edges are the
// symbol edge table aggregated per (source file, target file, kind), weight =
// how many symbol edges collapsed into it.
type cgFile struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Symbols  int    `json:"symbols"`
	IsTest   bool   `json:"isTest"`
}

type cgEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Kind   string `json:"kind"`
	Weight int    `json:"weight"`
}

type cgGraph struct {
	Files []cgFile `json:"files"`
	Edges []cgEdge `json:"edges"`
}

// cgSymbol is one node row, as a search hit, a file's member, or a
// caller/callee (Via = the edge kind that connected it, on the latter two).
type cgSymbol struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	File      string `json:"file,omitempty"`
	Line      int    `json:"line"`
	EndLine   int    `json:"endLine,omitempty"`
	Signature string `json:"signature,omitempty"`
	Via       string `json:"via,omitempty"`
	Count     int    `json:"count,omitempty"` // >1 = that many parallel edges
}

type cgSymbolDetail struct {
	Node    cgSymbol   `json:"node"`
	Callers []cgSymbol `json:"callers"`
	Callees []cgSymbol `json:"callees"`
}

// cgOpen opens a repo's codegraph.db strictly read-only: the indexer owns
// these files, and anything beyond a short shared read would fight its writes.
func cgOpen(root string) (*sql.DB, error) {
	return sql.Open("sqlite",
		"file:"+repos.DBPath(root)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
}

// cgFillSummary loads the index-size numbers and age for one repo. Any
// failure just demotes it to HasIndex=false — a half-broken DB renders as
// "no index", never as an error page.
func cgFillSummary(r *repos.Repo) {
	db, err := cgOpen(r.Root)
	if err != nil {
		r.HasIndex = false
		return
	}
	defer db.Close()
	var indexedMs int64
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(indexed_at),0) FROM files`).
		Scan(&r.Files, &indexedMs); err != nil {
		r.HasIndex = false
		return
	}
	if db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&r.Nodes) != nil ||
		db.QueryRow(`SELECT COUNT(*) FROM edges WHERE kind <> 'contains'`).Scan(&r.Edges) != nil {
		r.HasIndex = false
		return
	}
	if indexedMs > 0 {
		r.IndexedAt = time.UnixMilli(indexedMs)
		r.CommitsSince = cgCommitsSince(r.Root, r.IndexedAt)
	}
}

// cgCommitsSince counts commits newer than t — the "how stale is this index"
// signal. Best-effort: any git hiccup (not a repo, no git, timeout) is -1,
// never an error surfaced to the page.
func cgCommitsSince(root string, t time.Time) int {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", root,
		"rev-list", "--count", "HEAD", "--since="+t.Format(time.RFC3339)).Output()
	if err != nil {
		return -1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return -1
	}
	return n
}

// cgIsTestPath flags test files so the page can keep them off the graph by
// default: they dominate edge counts without saying anything about runtime
// structure, and the known false-edge class (browser-API calls resolving into
// a localStorage mock) lives inside them.
func cgIsTestPath(p string) bool {
	base := path.Base(p) // DB paths are slash-separated
	if strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, "_test.dart") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, d := range []string{"tests/", "test/", "__tests__/", "testdata/", "integration_test/"} {
		if strings.HasPrefix(p, d) || strings.Contains(p, "/"+d) {
			return true
		}
	}
	return false
}

// cgFileGraph aggregates the symbol-level edge table up to file level.
// Dropped on purpose: contains edges (file→its own symbols — structure, not
// architecture), same-file edges, and cross-language "calls" — measured on
// real indexes those are always name-collision noise (a TS localStorage call
// resolving into a Go main, and the like).
func cgFileGraph(db *sql.DB) (*cgGraph, error) {
	g := &cgGraph{Files: []cgFile{}, Edges: []cgEdge{}}
	rows, err := db.Query(`SELECT path, language, node_count FROM files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f cgFile
		if err := rows.Scan(&f.Path, &f.Language, &f.Symbols); err != nil {
			rows.Close()
			return nil, err
		}
		f.IsTest = cgIsTestPath(f.Path)
		g.Files = append(g.Files, f)
	}
	rows.Close()
	rows, err = db.Query(`
SELECT ns.file_path, nt.file_path, e.kind, COUNT(*)
FROM edges e
JOIN nodes ns ON ns.id = e.source
JOIN nodes nt ON nt.id = e.target
WHERE ns.file_path <> nt.file_path
  AND e.kind <> 'contains'
  AND NOT (e.kind = 'calls' AND ns.language <> nt.language)
GROUP BY ns.file_path, nt.file_path, e.kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e cgEdge
		if err := rows.Scan(&e.From, &e.To, &e.Kind, &e.Weight); err != nil {
			return nil, err
		}
		g.Edges = append(g.Edges, e)
	}
	return g, rows.Err()
}

// cgFileSymbols lists one file's symbols, top-to-bottom. Import nodes and the
// file's own file-node are noise in a member list and stay out.
func cgFileSymbols(db *sql.DB, file string) ([]cgSymbol, error) {
	rows, err := db.Query(`
SELECT id, name, kind, start_line, end_line, COALESCE(signature,'')
FROM nodes WHERE file_path = ? AND kind NOT IN ('import','file') ORDER BY start_line`, file)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cgSymbol{}
	for rows.Next() {
		var s cgSymbol
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.Line, &s.EndLine, &s.Signature); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// cgSearch matches the FTS index over names / qualified names / signatures /
// docstrings. The input is quoted into a single prefix phrase so user text
// can never become FTS syntax.
func cgSearch(db *sql.DB, q string) ([]cgSymbol, error) {
	match := `"` + strings.ReplaceAll(q, `"`, `""`) + `"*`
	rows, err := db.Query(`
SELECT n.id, n.name, n.kind, n.file_path, n.start_line, COALESCE(n.signature,'')
FROM nodes_fts JOIN nodes n ON n.rowid = nodes_fts.rowid
WHERE nodes_fts MATCH ? AND n.kind NOT IN ('import','file')
ORDER BY rank LIMIT 20`, match)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []cgSymbol{}
	for rows.Next() {
		var s cgSymbol
		if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.File, &s.Line, &s.Signature); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// cgSymbolByID returns one symbol with its callers and callees (call /
// instantiates / extends edges; Via says which). nil, nil = no such id.
func cgSymbolByID(db *sql.DB, id string) (*cgSymbolDetail, error) {
	d := &cgSymbolDetail{Callers: []cgSymbol{}, Callees: []cgSymbol{}}
	err := db.QueryRow(`
SELECT id, name, kind, file_path, start_line, end_line, COALESCE(signature,'')
FROM nodes WHERE id = ?`, id).
		Scan(&d.Node.ID, &d.Node.Name, &d.Node.Kind, &d.Node.File,
			&d.Node.Line, &d.Node.EndLine, &d.Node.Signature)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	load := func(dir string, dst *[]cgSymbol) error {
		other, mine := "e.source", "e.target"
		if dir == "out" {
			other, mine = "e.target", "e.source"
		}
		rows, err := db.Query(`
SELECT n.id, n.name, n.kind, n.file_path, n.start_line, e.kind, COUNT(*)
FROM edges e JOIN nodes n ON n.id = `+other+`
WHERE `+mine+` = ? AND e.kind IN ('calls','instantiates','extends')
GROUP BY n.id, e.kind
ORDER BY n.file_path, n.start_line LIMIT 50`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s cgSymbol
			if err := rows.Scan(&s.ID, &s.Name, &s.Kind, &s.File, &s.Line, &s.Via, &s.Count); err != nil {
				return err
			}
			*dst = append(*dst, s)
		}
		return rows.Err()
	}
	if err := load("in", &d.Callers); err != nil {
		return nil, err
	}
	if err := load("out", &d.Callees); err != nil {
		return nil, err
	}
	return d, nil
}

// Register wires the three code-graph read endpoints.
func Register(router chi.Router, rr *repos.Resolver) {
	// openScoped validates ?repo against the scope's resolved roots and opens
	// its DB; nil means the response was already written.
	openScoped := func(w http.ResponseWriter, r *http.Request) *sql.DB {
		root := r.URL.Query().Get("repo")
		ok := false
		for _, rp := range rr.Bound(r.URL.Query().Get("scope")) {
			if rp.Root == root && rp.HasIndex {
				ok = true
				break
			}
		}
		if !ok {
			httpx.WriteJSONError(w, http.StatusNotFound, "no code graph for that repo")
			return nil
		}
		db, err := cgOpen(root)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return nil
		}
		return db
	}

	// The page load: the scope's repos with their summaries, plus the file
	// graph of ?repo (or the first indexed repo when unset).
	router.Get("/api/codegraph", func(w http.ResponseWriter, r *http.Request) {
		repoList := rr.Bound(r.URL.Query().Get("scope"))
		for i := range repoList {
			if repoList[i].HasIndex {
				cgFillSummary(&repoList[i])
			}
		}
		want := r.URL.Query().Get("repo")
		active := -1
		for i := range repoList {
			if want == "" && repoList[i].HasIndex || want != "" && repoList[i].Root == want && repoList[i].HasIndex {
				active = i
				break
			}
		}
		resp := struct {
			Repos  []repos.Repo `json:"repos"`
			Active string       `json:"active"`
			Graph  *cgGraph     `json:"graph"`
		}{Repos: repoList}
		if active >= 0 {
			if db, err := cgOpen(repoList[active].Root); err == nil {
				defer db.Close()
				if g, err := cgFileGraph(db); err == nil {
					resp.Active = repoList[active].Root
					resp.Graph = g
				}
			}
		}
		httpx.WriteJSON(w, resp)
	})

	// One file's symbols (the click-a-node drill-down).
	router.Get("/api/codegraph/file", func(w http.ResponseWriter, r *http.Request) {
		db := openScoped(w, r)
		if db == nil {
			return
		}
		defer db.Close()
		syms, err := cgFileSymbols(db, r.URL.Query().Get("path"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, syms)
	})

	// Symbol lookup: ?id= → detail with callers/callees, else ?q= → FTS hits.
	router.Get("/api/codegraph/symbols", func(w http.ResponseWriter, r *http.Request) {
		db := openScoped(w, r)
		if db == nil {
			return
		}
		defer db.Close()
		if id := r.URL.Query().Get("id"); id != "" {
			d, err := cgSymbolByID(db, id)
			if err != nil {
				httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if d == nil {
				httpx.WriteJSONError(w, http.StatusNotFound, "no such symbol")
				return
			}
			httpx.WriteJSON(w, d)
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			httpx.WriteJSONError(w, http.StatusBadRequest, "q or id required")
			return
		}
		syms, err := cgSearch(db, q)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, syms)
	})
}
