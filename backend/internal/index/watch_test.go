package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The poller's whole contract: a transcript created after the watcher starts
// is picked up, and a later append to the same file fires again — the case a
// dir-mtime poll would miss (an append moves the file's mtime and size, not
// its directory's).
func TestWatchPollsCreatesAndAppends(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "-users-x-dev-proj-w")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	ix := New(root)
	if _, err := ix.Rescan(); err != nil {
		t.Fatal(err)
	}

	got := make(chan *Session, 8)
	stop := Watch(ix, 20*time.Millisecond, func(s *Session) { got <- s })
	defer stop()

	main := filepath.Join(proj, "77777777-aaaa-bbbb-cccc-000000000007.jsonl")
	line1 := `{"type":"user","uuid":"m1","timestamp":"2026-07-17T10:00:00.000Z","cwd":"/Users/x/dev/proj-w","message":{"role":"user","content":[{"type":"text","text":"go"}]}}` + "\n"
	if err := os.WriteFile(main, []byte(line1), 0o644); err != nil {
		t.Fatal(err)
	}

	var first *Session
	select {
	case first = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no update within 5s of the transcript appearing")
	}
	if first.ID != "77777777-aaaa-bbbb-cccc-000000000007" {
		t.Fatalf("update for %q, want the new session", first.ID)
	}

	f, err := os.OpenFile(main, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	line2 := `{"type":"user","uuid":"m2","timestamp":"2026-07-17T10:05:00.000Z","cwd":"/Users/x/dev/proj-w","message":{"role":"user","content":[{"type":"text","text":"more"}]}}` + "\n"
	if _, err := f.WriteString(line2); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Drain until an update reflects the append; a leftover pre-append update
	// carries the old EndedAt and must not satisfy this.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-got:
			if s.EndedAt.After(first.EndedAt) {
				return
			}
		case <-deadline:
			t.Fatal("append never re-scanned within 5s")
		}
	}
}
