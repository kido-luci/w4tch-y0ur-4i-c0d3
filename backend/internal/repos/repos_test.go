package repos

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"watch-your-ai-code/internal/index"
)

// fakeSessions satisfies the Sessions interface (Snapshot() []*index.Session)
// this package declares. It hands a test exact session data without needing
// a real Index's session map.
type fakeSessions []*index.Session

func (f fakeSessions) Snapshot() []*index.Session { return f }

// newIndexedRoot creates dir (if needed) carrying a .codegraph/codegraph.db
// marker file, so it counts as "has an index" to Repos' os.Stat(DBPath(...))
// check — resolution only checks that the path exists, it never opens it.
func newIndexedRoot(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".codegraph"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DBPath(dir), []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCGRepos(t *testing.T) {
	indexed := newIndexedRoot(t, t.TempDir())
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
	repos := New(sessions).Repos("proj,gone")
	if len(repos) != 1 {
		t.Fatalf("repos = %+v", repos)
	}
	if repos[0].Root != indexed || !repos[0].HasIndex || repos[0].Folder != "proj" {
		t.Errorf("repo = %+v", repos[0])
	}
	// Unscoped resolves every folder; "other" has no index but a live dir.
	all := New(sessions).Repos("")
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
	newIndexedRoot(t, sub) // the sub-repo carries its OWN index

	sessions := fakeSessions{
		// Only the workspace root has a session; "admin-frontend" has none.
		{ID: "s1", Project: filepath.Base(ws), CWD: ws, EndedAt: time.Now()},
	}
	repos := New(sessions).Repos("admin-frontend")
	if len(repos) != 1 {
		t.Fatalf("fallback repos = %+v", repos)
	}
	if repos[0].Root != sub || !repos[0].HasIndex || repos[0].Folder != "admin-frontend" {
		t.Errorf("repo = %+v, want indexed %s", repos[0], sub)
	}
	// A folder with no matching indexed dir stays unresolved.
	if got := New(sessions).Repos("nonesuch"); len(got) != 0 {
		t.Errorf("nonesuch resolved unexpectedly: %+v", got)
	}
}
