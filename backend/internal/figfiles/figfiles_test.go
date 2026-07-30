package figfiles

import (
	"bytes"
	"errors"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"watch-your-ai-code/internal/repos"
)

// repoWith builds a repo root holding design/ with the given file names, and
// returns it as the resolved repo the handlers would pass in.
func repoWith(t *testing.T, folder string, names ...string) repos.Repo {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, designDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	return repos.Repo{Root: root, Folder: folder}
}

func names(files []File) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

func TestListKeepsOnlyDocuments(t *testing.T) {
	repo := repoWith(t, "proj", "home.fig", "flow.pen", "notes.txt", "README.md", "UPPER.FIG")

	got := names(List([]repos.Repo{repo}))

	want := map[string]bool{"home.fig": true, "flow.pen": true, "UPPER.FIG": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want the three documents", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("List returned %q, which is not a design document", n)
		}
	}
}

// A design/ deep enough to need a recursive walk is not a case this has, so
// nested files are deliberately invisible rather than accidentally missing.
func TestListDoesNotRecurse(t *testing.T) {
	repo := repoWith(t, "proj", "top.fig")
	nested := filepath.Join(repo.Root, designDir, "archive")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "old.fig"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := names(List([]repos.Repo{repo})); len(got) != 1 || got[0] != "top.fig" {
		t.Errorf("got %v, want only [top.fig]", got)
	}
}

func TestListSkipsRepoWithoutDesignDir(t *testing.T) {
	with := repoWith(t, "has", "a.fig")
	without := repos.Repo{Root: t.TempDir(), Folder: "none"}

	if got := names(List([]repos.Repo{without, with})); len(got) != 1 || got[0] != "a.fig" {
		t.Errorf("got %v, want only [a.fig]", got)
	}
}

// A missing design/ is the normal case and stays out of the log; any other
// read error must land in it — an empty answer caused by an error (fd
// exhaustion, permissions) must be distinguishable from an empty library.
func TestListLogsReadErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs unix permission modes")
	}
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	List([]repos.Repo{{Root: t.TempDir(), Folder: "bare"}})
	if buf.Len() != 0 {
		t.Fatalf("missing design/ logged %q, want silence", buf.String())
	}

	repo := repoWith(t, "proj", "home.fig")
	dir := filepath.Join(repo.Root, designDir)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) }) // or TempDir cleanup cannot delete it

	if got := List([]repos.Repo{repo}); len(got) != 0 {
		t.Fatalf("unreadable design/ returned %v, want nothing", got)
	}
	if !strings.Contains(buf.String(), "figfiles: list") {
		t.Fatalf("unreadable design/ logged %q, want a figfiles: list error", buf.String())
	}
}

func TestListSortsNewestFirst(t *testing.T) {
	repo := repoWith(t, "proj", "old.fig", "new.fig")
	old := filepath.Join(repo.Root, designDir, "old.fig")
	stamp := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, stamp, stamp); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	if got := names(List([]repos.Repo{repo})); len(got) != 2 || got[0] != "new.fig" {
		t.Errorf("got %v, want new.fig first", got)
	}
}

func TestListReportsRepoAndPath(t *testing.T) {
	repo := repoWith(t, "memoirme", "home.fig")

	files := List([]repos.Repo{repo})
	if len(files) != 1 {
		t.Fatalf("got %d files, want 1", len(files))
	}
	f := files[0]
	if f.Folder != "memoirme" || f.Root != repo.Root {
		t.Errorf("got root=%q folder=%q, want the repo's", f.Root, f.Folder)
	}
	if want := filepath.Join(repo.Root, designDir, "home.fig"); f.Path != want {
		t.Errorf("got path %q, want %q", f.Path, want)
	}
	if f.Size != 1 {
		t.Errorf("got size %d, want 1", f.Size)
	}
}

// The guard that makes this endpoint safe to expose: `open` is only ever handed
// a path the scope itself produced. Every case below must fail before the
// platform check, so this runs on CI's Linux runner as well as on a Mac.
func TestOpenRejectsPathOutsideScope(t *testing.T) {
	inScope := repoWith(t, "mine", "home.fig")
	elsewhere := repoWith(t, "theirs", "secret.fig")
	rs := []repos.Repo{inScope}

	cases := map[string]string{
		"another scope's repo": filepath.Join(elsewhere.Root, designDir, "secret.fig"),
		"traversal":            filepath.Join(inScope.Root, designDir, "..", "..", "etc", "passwd"),
		"outside design dir":   filepath.Join(inScope.Root, "home.fig"),
		"not listed":           filepath.Join(inScope.Root, designDir, "absent.fig"),
		"empty":                "",
	}
	for name, path := range cases {
		if err := Open(rs, path); !errors.Is(err, ErrUnknownFile) {
			t.Errorf("%s: got %v, want ErrUnknownFile", name, err)
		}
	}
}
