// Package httpx holds the HTTP plumbing every endpoint family shares: the two
// JSON writers, and the guard that wraps the whole server. It exists so the
// git, GitHub, code-graph, board and MCP endpoints can answer in one voice
// without any of them importing another.
package httpx

import (
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
)

func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func WriteJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// HostGuard rejects requests that did not come to this server through its own
// address. The loopback bind is necessary but not sufficient: two browser-side
// attacks still reach a local server. DNS rebinding — a hostile page whose
// domain re-resolves to 127.0.0.1 — arrives carrying the attacker's Host, and
// with it a page could READ transcripts, sessions and the board under its own
// origin; checking Host kills it. Blind CSRF — any web page firing "simple"
// cross-origin POSTs it can never read the answers to — arrives with the
// attacker's Origin; refusing a foreign Origin on state-changing methods kills
// that. Requests without an Origin header pass: curl, MCP clients, the hook
// command, and the Vite dev proxy (which strips the one it forwards) are not
// browsers, and CSRF needs a browser.
func HostGuard(addr string, next http.Handler) http.Handler {
	allowed := map[string]bool{strings.ToLower(addr): true}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		allowed["127.0.0.1:"+port] = true
		allowed["localhost:"+port] = true
		allowed["[::1]:"+port] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			WriteJSONError(w, http.StatusForbidden, "unexpected Host header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(o)
			if err != nil || u.Scheme != "http" || !allowed[strings.ToLower(u.Host)] {
				WriteJSONError(w, http.StatusForbidden, "cross-origin writes are not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WithinRoot reports whether p resolves to a location inside root. The hook
// endpoint is loopback-only, but a local browser page could still POST to it,
// so we never rescan a path that points outside the watched projects dir.
func WithinRoot(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
