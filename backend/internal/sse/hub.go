// Package sse fans server-sent events out to the connected /api/events
// clients. It knows nothing about what it is carrying: the index, the board
// and the ship watcher all push through the same hub, so nothing that
// produces events needs to depend on anything that consumes them.
package sse

import (
	"encoding/json"
	"sync"
)

// Hub fans update events out to connected /api/events clients.
type Hub struct {
	mu      sync.Mutex
	clients map[chan []byte]bool
}

func New() *Hub {
	return &Hub{clients: map[chan []byte]bool{}}
}

func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 16)
	h.mu.Lock()
	h.clients[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *Hub) Broadcast(event string, payload any) {
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
