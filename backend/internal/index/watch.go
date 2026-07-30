package index

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch re-parses sessions whose files change and calls onUpdate with the
// refreshed summary (with agent runs). fsnotify is not recursive, so every
// directory level is watched and new directories are added on Create.
func Watch(ix *Index, onUpdate func(*Session)) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}

	// On macOS a directory watch is kqueue underneath, which holds an open fd
	// for the directory AND every file in it — so Add failing here is the
	// first symptom of fd exhaustion, and one summary line per walk keeps the
	// log bounded even when every single Add fails.
	addTree := func(dir string) {
		var added, failed int
		var firstErr error
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				failed++
				if firstErr == nil {
					firstErr = err
				}
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			if werr := w.Add(path); werr != nil {
				failed++
				if firstErr == nil {
					firstErr = werr
				}
				return nil
			}
			added++
			return nil
		})
		if failed > 0 {
			log.Printf("watch: %d of %d watches failed under %s (first: %v)", failed, added+failed, dir, firstErr)
		}
	}
	addTree(ix.root)

	// Debounce per session: transcripts get many small appends per turn.
	var mu sync.Mutex
	pending := map[string]*time.Timer{}

	fire := func(changedPath string) {
		s := ix.RescanSession(changedPath)
		if s == nil {
			return
		}
		onUpdate(ix.WithStatus(s, time.Now()))
	}

	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					return
				}
				if ev.Has(fsnotify.Create) {
					if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
						addTree(ev.Name)
						continue
					}
				}
				if !ev.Has(fsnotify.Write) && !ev.Has(fsnotify.Create) {
					continue
				}
				name := ev.Name
				if !strings.HasSuffix(name, ".jsonl") && !strings.HasSuffix(name, ".meta.json") {
					continue
				}
				mu.Lock()
				if t, ok := pending[name]; ok {
					t.Stop()
				}
				pending[name] = time.AfterFunc(500*time.Millisecond, func() {
					mu.Lock()
					delete(pending, name)
					mu.Unlock()
					fire(name)
				})
				mu.Unlock()
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Printf("watch: %v", err)
			}
		}
	}()
	return nil
}
