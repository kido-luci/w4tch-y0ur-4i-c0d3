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
