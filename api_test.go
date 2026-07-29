package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"watch-your-ai-code/internal/httpx"
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

// hostGuard must block DNS rebinding (foreign Host) and blind cross-origin
// writes (foreign Origin on state-changing methods) while leaving every
// legitimate caller alone: the app's own frontend on any loopback spelling,
// and Origin-less non-browser clients (curl, MCP, the hook command, the Vite
// dev proxy after it strips the header).
func TestHostGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	guard := httpx.HostGuard("127.0.0.1:4777", ok)

	cases := []struct {
		name   string
		method string
		host   string
		origin string
		want   int
	}{
		{"no-origin POST (curl/MCP/hook)", "POST", "127.0.0.1:4777", "", http.StatusNoContent},
		{"localhost host variant", "GET", "localhost:4777", "", http.StatusNoContent},
		{"ipv6 loopback host", "GET", "[::1]:4777", "", http.StatusNoContent},
		{"same-origin write", "POST", "127.0.0.1:4777", "http://127.0.0.1:4777", http.StatusNoContent},
		{"localhost-origin write", "PUT", "127.0.0.1:4777", "http://localhost:4777", http.StatusNoContent},
		{"cross-origin GET (unreadable anyway)", "GET", "127.0.0.1:4777", "https://evil.example", http.StatusNoContent},

		{"dns rebinding host", "GET", "evil.example:4777", "", http.StatusForbidden},
		{"rebinding host without port", "POST", "evil.example", "", http.StatusForbidden},
		{"blind cross-origin POST", "POST", "127.0.0.1:4777", "https://evil.example", http.StatusForbidden},
		{"wrong-port origin write", "POST", "127.0.0.1:4777", "http://127.0.0.1:5173", http.StatusForbidden},
		{"null origin write", "POST", "127.0.0.1:4777", "null", http.StatusForbidden},
		{"cross-origin DELETE", "DELETE", "127.0.0.1:4777", "https://evil.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(tc.method, "/api/todos", nil)
		r.Host = tc.host
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		guard.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, w.Code, tc.want)
		}
	}
}

// A custom -addr keeps working: the bound address itself is always allowed.
func TestHostGuardCustomAddr(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	guard := httpx.HostGuard("192.168.1.5:4779", ok)

	r := httptest.NewRequest("GET", "/api/todos", nil)
	r.Host = "192.168.1.5:4779"
	w := httptest.NewRecorder()
	guard.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Errorf("bound addr should be allowed, got %d", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/todos", nil)
	r.Host = "192.168.1.6:4779"
	w = httptest.NewRecorder()
	guard.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Errorf("foreign host should be refused, got %d", w.Code)
	}
}
