package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// sseHub fans session-update events out to connected /api/events clients.
type sseHub struct {
	mu      sync.Mutex
	clients map[chan []byte]bool
}

func newSSEHub() *sseHub {
	return &sseHub{clients: map[chan []byte]bool{}}
}

func (h *sseHub) subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *sseHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *sseHub) broadcast(event string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	msg := []byte("event: " + event + "\ndata: " + string(data) + "\n\n")
	h.mu.Lock()
	for ch := range h.clients {
		select {
		case ch <- msg:
		default: // slow client: drop rather than block the watcher
		}
	}
	h.mu.Unlock()
}

// watch re-parses sessions whose files change and broadcasts the refreshed
// summary (with agent runs) over SSE. fsnotify is not recursive, so every
// directory level is watched and new directories are added on Create.
func watch(ix *Index, hub *sseHub) error {
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
		hub.broadcast("session-updated", ix.withStatus(s, time.Now()))
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
