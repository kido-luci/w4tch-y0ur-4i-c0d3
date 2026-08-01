package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"watch-your-ai-code/internal/board"
)

// A burst of kicks while a rescan runs must fold into exactly ONE follow-up:
// N events cost two parses, not N. Kicks for another path run independently,
// and the first kick of a burst starts immediately (no timer latency).
func TestRescanCoalescer(t *testing.T) {
	started := make(chan string, 16)
	// One release channel PER PATH, not one shared. Two callbacks are parked on
	// it at once here, and a send on a shared unbuffered channel wakes whichever
	// of them happened to park first — which is not the order they were started
	// in. On this machine "a" always parked first and the test passed; on a
	// contended CI runner "b" won the race, finished, exited, and left "a"
	// blocked forever with the test waiting on a follow-up that could never
	// come. Ten-minute timeout, no output. Keep these separate.
	release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	rc := newRescanCoalescer(func(path string) {
		started <- path
		<-release[path]
	})

	rc.kick("a")
	if got := <-started; got != "a" {
		t.Fatalf("first kick should run immediately, got %q", got)
	}
	// Burst while the first parse runs — and an independent path alongside.
	rc.kick("a")
	rc.kick("a")
	rc.kick("a")
	rc.kick("b")
	if got := <-started; got != "b" {
		t.Fatalf("an independent path should start immediately, got %q", got)
	}
	release["a"] <- struct{}{} // finish a's first parse
	if got := <-started; got != "a" {
		t.Fatalf("the burst should fold into one follow-up for a, got %q", got)
	}
	release["a"] <- struct{}{} // finish a's follow-up
	release["b"] <- struct{}{} // finish b's parse

	// No further runs are owed.
	select {
	case p := <-started:
		t.Fatalf("unexpected extra rescan of %q", p)
	case <-time.After(100 * time.Millisecond):
	}
}

// presentationFixture builds the guard over one showable folder ("open-folder",
// whose repo is public) and one that is not ("secret-folder"), with a recording
// handler behind it that captures the project param each request arrived with.
// The visibility answer is a plain func here for the same reason it is one in
// production: whether a folder may be shown is a question about its REPO, and
// the guard's job is only to apply the answer.
func presentationFixture(t *testing.T) (settings *board.SettingsStore, guard http.Handler, got *string) {
	t.Helper()
	db, err := board.OpenDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	settings = board.NewSettingsStore(db)
	got = new(string)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.URL.Query().Get("project")
	})
	public := func() map[string]bool { return map[string]bool{"open-folder": true} }
	return settings, PresentationGuard(settings, public, next), got
}

func TestPresentationGuardRewritesTheProjectParam(t *testing.T) {
	settings, guard, got := presentationFixture(t)
	serve := func(method, target string) {
		*got = "unset"
		guard.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, target, nil))
	}

	// Mode off: everything passes through untouched.
	serve("GET", "/api/sessions")
	if *got != "" {
		t.Fatalf("mode off must not rewrite, handler saw %q", *got)
	}

	if err := settings.SetPresentationHidden(true); err != nil {
		t.Fatal(err)
	}

	// An empty param means "all folders" — it becomes the public ones.
	serve("GET", "/api/sessions")
	if *got != "open-folder" {
		t.Fatalf("empty param should become the public folders, handler saw %q", *got)
	}

	// A scoped param loses its private folders and keeps the rest.
	serve("GET", "/api/stats?project=open-folder,secret-folder")
	if *got != "open-folder" {
		t.Fatalf("scoped param should lose the private folder, handler saw %q", *got)
	}

	// A folder whose repo is not public (or that resolves to no repo at all)
	// never survives, however it got into the param — subtracting the known
	// private ones instead left every unclaimed folder, and its sessions under
	// its raw directory name, on screen mid-demo.
	serve("GET", "/api/stats?project=open-folder,loose-folder")
	if *got != "open-folder" {
		t.Fatalf("an unowned folder must not survive, handler saw %q", *got)
	}

	// A param left with nothing must NOT become the all-folders empty string.
	serve("GET", "/api/stats?project=secret-folder")
	if *got == "" || strings.Contains(*got, "secret-folder") {
		t.Fatalf("an emptied filter must match nothing, handler saw %q", *got)
	}

	// Ships speak Makefile names, not folders — the guard leaves them alone.
	serve("GET", "/api/ships")
	if *got != "" {
		t.Fatalf("/api/ships must pass through untouched, handler saw %q", *got)
	}

	// Writes and non-API paths pass through untouched.
	serve("POST", "/api/todos")
	if *got != "" {
		t.Fatalf("a write must pass through untouched, handler saw %q", *got)
	}
	serve("GET", "/assets/index.js")
	if *got != "" {
		t.Fatalf("a static path must pass through untouched, handler saw %q", *got)
	}
}
