package main

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
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// cgRepo is one repo a scope resolves to: where it lives, whether it carries
// an index, and — when it does — the index's size and age plus how many
// commits the repo has seen since it was written (-1 = git couldn't answer).
type cgRepo struct {
	Root         string    `json:"root"`
	Folder       string    `json:"folder"` // the Claude folder that led here
	HasIndex     bool      `json:"hasIndex"`
	IndexedAt    time.Time `json:"indexedAt"`
	Files        int       `json:"files"`
	Nodes        int       `json:"nodes"`
	Edges        int       `json:"edges"`
	CommitsSince int       `json:"commitsSince"`
}

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

func cgDBPath(root string) string {
	return filepath.Join(root, ".codegraph", "codegraph.db")
}

// cgRepos maps the scope's folders to on-disk repo roots. For each folder the
// candidates are its sessions' cwds, newest first; the first one that still
// exists wins, preferring one that carries an index — so a leftover worktree
// path or a since-moved checkout can't shadow the real repo. A folder with no
// live session cwd of its own then falls back to finding an indexed directory
// of that name near a known workspace cwd (see findIndexedDir), so an infra
// sub-repo you've indexed surfaces without ever opening Claude in it. Roots are
// deduped across folders, indexed repos sort first.
func (ix *Index) cgRepos(project string) []cgRepo {
	var want map[string]bool
	if list := splitProjects(project); list != nil {
		want = make(map[string]bool, len(list))
		for _, p := range list {
			want[p] = true
		}
	}
	type cand struct {
		cwd string
		at  time.Time
	}
	byFolder := map[string][]cand{}
	ix.mu.RLock()
	for _, s := range ix.sessions {
		if s.CWD == "" || (want != nil && !want[s.Project]) {
			continue
		}
		byFolder[s.Project] = append(byFolder[s.Project], cand{s.CWD, s.EndedAt})
	}
	ix.mu.RUnlock()

	out := []cgRepo{}
	seen := map[string]bool{}
	for folder, cands := range byFolder {
		sort.Slice(cands, func(i, j int) bool { return cands[i].at.After(cands[j].at) })
		root, hasIndex := "", false
		for _, c := range cands {
			fi, err := os.Stat(c.cwd)
			if err != nil || !fi.IsDir() {
				continue
			}
			if _, err := os.Stat(cgDBPath(c.cwd)); err == nil {
				root, hasIndex = c.cwd, true
				break
			}
			if root == "" {
				root = c.cwd
			}
		}
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, cgRepo{Root: root, Folder: folder, HasIndex: hasIndex, CommitsSince: -1})
	}

	// Fallback for a scoped folder that resolved to nothing above — an infra
	// sub-repo you indexed but never opened Claude in, so it has no session cwd
	// of its own. Locate its OWN repo dir by matching the folder name against
	// the children and siblings of every known session cwd, and keep it only if
	// it carries a .codegraph. This never borrows a parent's index — it finds
	// the sub-repo's own — so a nested checkout surfaces without a session there.
	if want != nil {
		resolved := make(map[string]bool, len(out))
		for _, r := range out {
			resolved[r.Folder] = true
		}
		var missing []string
		for folder := range want {
			if !resolved[folder] && folder != "" && !strings.ContainsAny(folder, `/\`) && folder != ".." {
				missing = append(missing, folder)
			}
		}
		if len(missing) > 0 {
			anchors := ix.sessionDirs()
			for _, folder := range missing {
				root := findIndexedDir(folder, anchors)
				if root != "" && !seen[root] {
					seen[root] = true
					out = append(out, cgRepo{Root: root, Folder: folder, HasIndex: true, CommitsSince: -1})
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].HasIndex != out[j].HasIndex {
			return out[i].HasIndex
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// sessionDirs is the distinct set of session working directories that still
// exist on disk — the anchors the fallback resolution searches from.
func (ix *Index) sessionDirs() []string {
	uniq := map[string]bool{}
	ix.mu.RLock()
	for _, s := range ix.sessions {
		if s.CWD != "" {
			uniq[s.CWD] = true
		}
	}
	ix.mu.RUnlock()
	out := make([]string, 0, len(uniq))
	for d := range uniq {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// findIndexedDir looks for a directory named `folder` that carries its own
// .codegraph, sitting as a child or sibling of one of the anchor cwds (or being
// one). "" if none — the fallback only surfaces a sub-repo that's actually been
// indexed, never a bare directory.
func findIndexedDir(folder string, anchors []string) string {
	indexed := func(d string) string {
		if _, err := os.Stat(cgDBPath(d)); err != nil {
			return ""
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return ""
		}
		return d
	}
	for _, a := range anchors {
		if r := indexed(filepath.Join(a, folder)); r != "" { // child of a workspace root
			return r
		}
		if r := indexed(filepath.Join(filepath.Dir(a), folder)); r != "" { // sibling of a subdir
			return r
		}
		if filepath.Base(a) == folder {
			if r := indexed(a); r != "" {
				return r
			}
		}
	}
	return ""
}

// cgOpen opens a repo's codegraph.db strictly read-only: the indexer owns
// these files, and anything beyond a short shared read would fight its writes.
func cgOpen(root string) (*sql.DB, error) {
	return sql.Open("sqlite",
		"file:"+cgDBPath(root)+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
}

// cgFillSummary loads the index-size numbers and age for one repo. Any
// failure just demotes it to HasIndex=false — a half-broken DB renders as
// "no index", never as an error page.
func cgFillSummary(r *cgRepo) {
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

// registerCodegraphAPI wires the three code-graph read endpoints.
func registerCodegraphAPI(mux *http.ServeMux, ix *Index) {
	// openScoped validates ?repo against the scope's resolved roots and opens
	// its DB; nil means the response was already written.
	openScoped := func(w http.ResponseWriter, r *http.Request) *sql.DB {
		root := r.URL.Query().Get("repo")
		ok := false
		for _, rp := range ix.cgRepos(r.URL.Query().Get("project")) {
			if rp.Root == root && rp.HasIndex {
				ok = true
				break
			}
		}
		if !ok {
			writeJSONError(w, http.StatusNotFound, "no code graph for that repo")
			return nil
		}
		db, err := cgOpen(root)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return nil
		}
		return db
	}

	// The page load: the scope's repos with their summaries, plus the file
	// graph of ?repo (or the first indexed repo when unset).
	mux.HandleFunc("GET /api/codegraph", func(w http.ResponseWriter, r *http.Request) {
		repos := ix.cgRepos(r.URL.Query().Get("project"))
		for i := range repos {
			if repos[i].HasIndex {
				cgFillSummary(&repos[i])
			}
		}
		want := r.URL.Query().Get("repo")
		active := -1
		for i := range repos {
			if want == "" && repos[i].HasIndex || want != "" && repos[i].Root == want && repos[i].HasIndex {
				active = i
				break
			}
		}
		resp := struct {
			Repos  []cgRepo `json:"repos"`
			Active string   `json:"active"`
			Graph  *cgGraph `json:"graph"`
		}{Repos: repos}
		if active >= 0 {
			if db, err := cgOpen(repos[active].Root); err == nil {
				defer db.Close()
				if g, err := cgFileGraph(db); err == nil {
					resp.Active = repos[active].Root
					resp.Graph = g
				}
			}
		}
		writeJSON(w, resp)
	})

	// One file's symbols (the click-a-node drill-down).
	mux.HandleFunc("GET /api/codegraph/file", func(w http.ResponseWriter, r *http.Request) {
		db := openScoped(w, r)
		if db == nil {
			return
		}
		defer db.Close()
		syms, err := cgFileSymbols(db, r.URL.Query().Get("path"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, syms)
	})

	// Symbol lookup: ?id= → detail with callers/callees, else ?q= → FTS hits.
	mux.HandleFunc("GET /api/codegraph/symbols", func(w http.ResponseWriter, r *http.Request) {
		db := openScoped(w, r)
		if db == nil {
			return
		}
		defer db.Close()
		if id := r.URL.Query().Get("id"); id != "" {
			d, err := cgSymbolByID(db, id)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if d == nil {
				writeJSONError(w, http.StatusNotFound, "no such symbol")
				return
			}
			writeJSON(w, d)
			return
		}
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			writeJSONError(w, http.StatusBadRequest, "q or id required")
			return
		}
		syms, err := cgSearch(db, q)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, syms)
	})
}
