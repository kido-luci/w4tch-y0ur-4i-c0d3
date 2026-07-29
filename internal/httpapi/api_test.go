package httpapi

import (
	"testing"
	"time"
)

// A burst of kicks while a rescan runs must fold into exactly ONE follow-up:
// N events cost two parses, not N. Kicks for another path run independently,
// and the first kick of a burst starts immediately (no timer latency).
func TestRescanCoalescer(t *testing.T) {
	started := make(chan string, 16)
	release := make(chan struct{})
	rc := newRescanCoalescer(func(path string) {
		started <- path
		<-release
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
	release <- struct{}{} // finish a's first parse
	if got := <-started; got != "a" {
		t.Fatalf("the burst should fold into one follow-up for a, got %q", got)
	}
	release <- struct{}{} // finish a's follow-up
	release <- struct{}{} // finish b's parse

	// No further runs are owed.
	select {
	case p := <-started:
		t.Fatalf("unexpected extra rescan of %q", p)
	case <-time.After(100 * time.Millisecond):
	}
}
