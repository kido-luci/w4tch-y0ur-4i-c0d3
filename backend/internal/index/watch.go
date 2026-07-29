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

	addTree := func(dir string) {
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				w.Add(path)
			}
			return nil
		})
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
