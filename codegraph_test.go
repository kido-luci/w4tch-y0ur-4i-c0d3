package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newCGFixture creates a repo root carrying a .codegraph/codegraph.db with the
// real indexer's shape (the columns our queries touch) and a tiny two-language
// corpus: a.go/b.go calling each other, a.ts importing b.ts, one cross-language
// call (must be dropped), and a helper_test.go.
func newCGFixture(t *testing.T) string {
	return newCGFixtureAt(t, t.TempDir())
}

func newCGFixtureAt(t *testing.T, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+cgDBPath(root))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE files(path TEXT PRIMARY KEY, language TEXT, node_count INTEGER, indexed_at INTEGER)`,
		`CREATE TABLE nodes(id TEXT PRIMARY KEY, kind TEXT, name TEXT, qualified_name TEXT,
			docstring TEXT, signature TEXT, file_path TEXT, language TEXT,
			start_line INTEGER, end_line INTEGER)`,
		`CREATE TABLE edges(id INTEGER PRIMARY KEY AUTOINCREMENT, source TEXT, target TEXT, kind TEXT)`,
		`CREATE VIRTUAL TABLE nodes_fts USING fts5(id, name, qualified_name, docstring, signature,
			content='nodes', content_rowid='rowid')`,
		`INSERT INTO files VALUES
			('a.go','go',2,1700000000000),('b.go','go',1,1700000000000),
			('a.ts','typescript',1,1700000000000),('b.ts','typescript',1,1700000000000),
			('helper_test.go','go',1,1700000000000)`,
		`INSERT INTO nodes(id,kind,name,qualified_name,docstring,signature,file_path,language,start_line,end_line) VALUES
			('n1','function','doThing','pkg.doThing','does the thing','func doThing()','a.go','go',10,20),
			('n2','function','helper','pkg.helper','','func helper()','b.go','go',5,9),
			('n3','function','render','ui.render','','function render()','a.ts','typescript',1,4),
			('n4','function','mount','ui.mount','','function mount()','b.ts','typescript',1,3),
			('n5','function','TestThing','pkg.TestThing','','func TestThing(t *testing.T)','helper_test.go','go',1,8),
			('n6','import','react','react','','','a.ts','typescript',1,1),
			('n7','file','a.ts','a.ts','','','a.ts','typescript',1,4)`,
		`INSERT INTO nodes_fts(nodes_fts) VALUES('rebuild')`,
		`INSERT INTO edges(source,target,kind) VALUES
			('n1','n2','calls'),('n1','n2','calls'),
			('n3','n4','imports'),
			('n3','n2','calls'),
			('n5','n2','calls'),
			('n1','n1','calls'),
			('n1','n2','contains')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("fixture %q: %v", s[:30], err)
		}
	}
	return root
}

func TestCGIsTestPath(t *testing.T) {
	cases := map[string]bool{
		"a/b/foo_test.go":                     true,
		"src/thing.test.ts":                   true,
		"src/thing.spec.ts":                   true,
		"tests/setup.ts":                      true,
		"pkg/__tests__/x.ts":                  true,
		"a/testdata/fix.json":                 true,
		"admin-frontend/tests/setup.ts":       true,
		"integration_test/app_flow_test.dart": true,
		"lib/widgets/foo_test.dart":           true,
		"src/main.go":                         false,
		"src/protest.ts":                      false, // "test" inside a word is not a test dir
		"contest/rank.go":                     false,
		"lib/latest.dart":                     false, // no underscore — not the _test.dart suffix
	}
	for p, want := range cases {
		if got := cgIsTestPath(p); got != want {
			t.Errorf("cgIsTestPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCGFileGraph(t *testing.T) {
	root := newCGFixture(t)
	db, err := cgOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	g, err := cgFileGraph(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Files) != 5 {
		t.Fatalf("files = %d, want 5", len(g.Files))
	}
	for _, f := range g.Files {
		if want := f.Path == "helper_test.go"; f.IsTest != want {
			t.Errorf("%s IsTest = %v", f.Path, f.IsTest)
		}
	}
	// Expected edges: a.go→b.go calls w=2 (aggregated), a.ts→b.ts imports w=1,
	// helper_test.go→b.go calls w=1. Dropped: the cross-language a.ts→b.go
	// call, the self-edge, the contains edge.
	want := map[string]int{
		"a.go→b.go|calls":           2,
		"a.ts→b.ts|imports":         1,
		"helper_test.go→b.go|calls": 1,
	}
	if len(g.Edges) != len(want) {
		t.Fatalf("edges = %+v, want %d", g.Edges, len(want))
	}
	for _, e := range g.Edges {
		if want[e.From+"→"+e.To+"|"+e.Kind] != e.Weight {
			t.Errorf("unexpected edge %+v", e)
		}
	}
}

func TestCGSummaryAndSymbols(t *testing.T) {
	root := newCGFixture(t)
	r := cgRepo{Root: root, HasIndex: true, CommitsSince: -1}
	cgFillSummary(&r)
	if !r.HasIndex || r.Files != 5 || r.Nodes != 7 {
		t.Fatalf("summary = %+v", r)
	}
	if r.Edges != 6 { // 7 raw minus the contains edge
		t.Errorf("edges = %d, want 6", r.Edges)
	}
	if r.IndexedAt.IsZero() {
		t.Error("IndexedAt not set")
	}

	db, err := cgOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	syms, err := cgFileSymbols(db, "a.ts")
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 || syms[0].Name != "render" { // import + file nodes stay out
		t.Fatalf("a.ts symbols = %+v", syms)
	}

	hits, err := cgSearch(db, "doTh")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "n1" {
		t.Fatalf("search hits = %+v", hits)
	}
	// FTS syntax must not leak in from user input.
	if _, err := cgSearch(db, `helper" OR x`); err != nil {
		t.Fatalf("quoted search errored: %v", err)
	}

	d, err := cgSymbolByID(db, "n2")
	if err != nil {
		t.Fatal(err)
	}
	// n2's callers: n1 (its two call edges grouped, count 2), n3 (cross-
	// language, kept at symbol level on purpose: the file graph filters it,
	// the drill-down shows raw truth), n5. No callees.
	if d == nil || d.Node.Name != "helper" || len(d.Callees) != 0 {
		t.Fatalf("detail = %+v", d)
	}
	if len(d.Callers) != 3 {
		t.Fatalf("callers = %+v", d.Callers)
	}
	for _, c := range d.Callers {
		if want := map[string]int{"n1": 2, "n3": 1, "n5": 1}[c.ID]; c.Count != want {
			t.Errorf("caller %s count = %d, want %d", c.ID, c.Count, want)
		}
	}
	if missing, _ := cgSymbolByID(db, "nope"); missing != nil {
		t.Error("expected nil for unknown id")
	}
}

func TestCGRepos(t *testing.T) {
	indexed := newCGFixture(t)
	plain := t.TempDir()
	now := time.Now()
	sessions := fakeSessions{
		// Newest session points at a plain dir, an older one at the indexed
		// repo — the indexed root must win anyway.
		{ID: "s1", Project: "proj", CWD: plain, EndedAt: now},
		{ID: "s2", Project: "proj", CWD: indexed, EndedAt: now.Add(-time.Hour)},
		// A folder whose cwd no longer exists resolves to nothing.
		{ID: "s3", Project: "gone", CWD: filepath.Join(plain, "deleted"), EndedAt: now},
		// Out of scope.
		{ID: "s4", Project: "other", CWD: plain, EndedAt: now},
	}
	repos := newRepoResolver(sessions).Repos("proj,gone")
	if len(repos) != 1 {
		t.Fatalf("repos = %+v", repos)
	}
	if repos[0].Root != indexed || !repos[0].HasIndex || repos[0].Folder != "proj" {
		t.Errorf("repo = %+v", repos[0])
	}
	// Unscoped resolves every folder; "other" has no index but a live dir.
	all := newRepoResolver(sessions).Repos("")
	if len(all) != 2 {
		t.Fatalf("all = %+v", all)
	}
	if !all[0].HasIndex || all[1].HasIndex {
		t.Errorf("indexed repos must sort first: %+v", all)
	}
}

// A scoped folder with no session cwd of its own still resolves when an indexed
// directory of that name sits under a known workspace cwd (the fallback path).
func TestCGReposFallback(t *testing.T) {
	ws := t.TempDir() // a workspace root that HAS a session
	sub := filepath.Join(ws, "admin-frontend")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	newCGFixtureAt(t, sub) // the sub-repo carries its OWN index

	sessions := fakeSessions{
		// Only the workspace root has a session; "admin-frontend" has none.
		{ID: "s1", Project: filepath.Base(ws), CWD: ws, EndedAt: time.Now()},
	}
	repos := newRepoResolver(sessions).Repos("admin-frontend")
	if len(repos) != 1 {
		t.Fatalf("fallback repos = %+v", repos)
	}
	if repos[0].Root != sub || !repos[0].HasIndex || repos[0].Folder != "admin-frontend" {
		t.Errorf("repo = %+v, want indexed %s", repos[0], sub)
	}
	// A folder with no matching indexed dir stays unresolved.
	if got := newRepoResolver(sessions).Repos("nonesuch"); len(got) != 0 {
		t.Errorf("nonesuch resolved unexpectedly: %+v", got)
	}
}
