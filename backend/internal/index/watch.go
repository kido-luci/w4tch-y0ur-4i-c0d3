package index

import (
	"log"
	"time"
)

// Watch polls the transcript tree and calls onUpdate with each session whose
// on-disk inputs changed. A poll, not fsnotify, on purpose: on macOS a kqueue
// watch holds an open fd for every watched directory AND every file inside
// it, which made the daemon's fd baseline scale with transcript history
// (~4.3k and growing against the process limit). Rescan already stamps every
// session by mtime+size — including the walked subagents tree — and skips the
// unchanged, so one tick is the same cheap stat sweep the old 5-minute
// reconcile always ran, just often enough to feel live. The sweep re-lists
// the tree each tick, so it doubles as the reconcile: new projects, deleted
// files and anything missed while asleep are caught within one interval.
// Returns a stop func so tests can end the loop; main never calls it.
func Watch(ix *Index, every time.Duration, onUpdate func(*Session)) (stop func()) {
	t := time.NewTicker(every)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			ids, err := ix.Rescan()
			if err != nil {
				log.Printf("watch: %v", err)
				continue
			}
			for _, id := range ids {
				if s := ix.Session(id); s != nil {
					onUpdate(s)
				}
			}
		}
	}()
	return func() {
		t.Stop()
		close(done)
	}
}
