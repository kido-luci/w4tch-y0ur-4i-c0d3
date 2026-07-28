package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// hostGuard rejects requests that did not come to this server through its own
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
func hostGuard(addr string, next http.Handler) http.Handler {
	allowed := map[string]bool{strings.ToLower(addr): true}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		allowed["127.0.0.1:"+port] = true
		allowed["localhost:"+port] = true
		allowed["[::1]:"+port] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[strings.ToLower(r.Host)] {
			writeJSONError(w, http.StatusForbidden, "unexpected Host header")
			return
		}
		if o := r.Header.Get("Origin"); o != "" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			u, err := url.Parse(o)
			if err != nil || u.Scheme != "http" || !allowed[strings.ToLower(u.Host)] {
				writeJSONError(w, http.StatusForbidden, "cross-origin writes are not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// withinRoot reports whether p resolves to a location inside root. The hook
// endpoint is loopback-only, but a local browser page could still POST to it,
// so we never rescan a path that points outside the watched projects dir.
func withinRoot(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// rescanCoalescer collapses hook-event bursts into at most one running and
// one pending rescan per transcript path. Each rescan is a full re-parse of
// the file, and hooks fire once per tool use — so a tool-heavy turn used to
// pay a whole parse per event, concurrently. With the coalescer the first
// event of a burst parses immediately (live updates stay instant — the point
// of hooks), and everything landing while that parse runs folds into a single
// follow-up: N events cost two parses, not N. No timer, no added latency;
// the file-watcher path keeps its own 500ms debounce.
type rescanCoalescer struct {
	mu      sync.Mutex
	running map[string]bool // path -> a rescan goroutine is active
	pending map[string]bool // path -> events arrived mid-parse; one follow-up owed
	run     func(path string)
}

func newRescanCoalescer(run func(string)) *rescanCoalescer {
	return &rescanCoalescer{running: map[string]bool{}, pending: map[string]bool{}, run: run}
}

// kick requests a rescan of path and returns immediately.
func (rc *rescanCoalescer) kick(path string) {
	rc.mu.Lock()
	if rc.running[path] {
		rc.pending[path] = true
		rc.mu.Unlock()
		return
	}
	rc.running[path] = true
	rc.mu.Unlock()
	go func() {
		for {
			rc.run(path)
			rc.mu.Lock()
			if !rc.pending[path] {
				delete(rc.running, path)
				rc.mu.Unlock()
				return
			}
			delete(rc.pending, path)
			rc.mu.Unlock()
		}
	}()
}

type statsResponse struct {
	Sessions    int     `json:"sessions"`
	TotalTokens int64   `json:"totalTokens"` // main + agents
	TotalCost   float64 `json:"totalCostUsd"`
	AgentSpawns int     `json:"agentSpawns"`
	Running     int     `json:"running"`
}

// activityDay is one calendar day's rollup for the GitHub-style heatmap.
type activityDay struct {
	Date     string  `json:"date"` // YYYY-MM-DD, in the server's local zone
	Sessions int     `json:"sessions"`
	Tokens   int64   `json:"tokens"` // main + agents
	CostUSD  float64 `json:"costUsd"`
}

// docWithBody is the GET /api/docs/{id} payload: a page's metadata plus its
// markdown body (the List payload carries metadata only, to keep the tree light).
type docWithBody struct {
	Doc
	Body string `json:"body"`
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// unknownDrawingID returns the first id that no longer exists in the library,
// so a typo'd link is rejected at the edge instead of rendering as a dead chip
// on the card. "" means every id checks out. Shared by the REST PATCH handler
// and the MCP update_todo tool.
func unknownDrawingID(drawings *DrawingStore, ids []string) string {
	for _, did := range ids {
		if did = strings.TrimSpace(did); did == "" {
			continue
		}
		if _, err := drawings.Get(did); err != nil {
			return did
		}
	}
	return ""
}

// unknownDocID is the docs-wiki analogue of unknownDrawingID: the first linked
// doc id that no longer exists, or "" when they all check out. Shared by the
// REST PATCH handler and the MCP update_todo tool.
func unknownDocID(docs *DocStore, ids []string) string {
	for _, did := range ids {
		if did = strings.TrimSpace(did); did == "" {
			continue
		}
		if _, err := docs.Get(did); err != nil {
			return did
		}
	}
	return ""
}

// refreezeTodo keeps a card's cost snapshot in step with the status it just
// moved to: landing in a done-category column freezes its linked sessions'
// summed numbers onto the card, leaving one thaws it (the numbers re-freeze on
// the next done). Shared by the REST PATCH handler and the MCP update_todo tool
// so a card costs the same however it was moved. Sessions missing from the
// index (their transcript is gone) are skipped rather than counted as zero.
//
// Since data.db v12 the test is the column's category, not the literal string
// "done" — a workflow whose last column is "Shipped" freezes just the same.
func refreezeTodo(todos *TodoStore, ix *Index, todo Todo, status string) Todo {
	if !todos.IsDoneStatus(status) {
		if todo.Snapshot == nil {
			return todo
		}
		if thawed, err := todos.SetSnapshot(todo.ID, nil); err == nil {
			return thawed
		}
		return todo
	}
	if todo.Snapshot != nil {
		return todo // already frozen; a re-done doesn't move the numbers
	}
	snap := TodoSnapshot{TakenAt: time.Now()}
	for _, sid := range todo.LinkedSessionIDs {
		s := ix.Session(sid)
		if s == nil {
			continue
		}
		snap.Tokens += s.TotalTokens + s.AgentTokens
		snap.CostUSD += s.CostUSD + s.AgentCostUSD
		snap.Agents += s.AgentCount
		snap.DurationMs += s.DurationMs
		snap.Sessions++
	}
	if snap.Sessions == 0 {
		return todo // nothing linked (or nothing left on disk) — no snapshot
	}
	if frozen, err := todos.SetSnapshot(todo.ID, &snap); err == nil {
		return frozen
	}
	return todo
}

func registerAPI(mux *http.ServeMux, ix *Index, hub *sseHub, su *Summarizer, todos *TodoStore, states *StateStore, cycles *CycleStore, events *EventStore, views *ViewStore, drawings *DrawingStore, docs *DocStore, groups *GroupStore, projects *ProjectStore) {
	// Every scope question below goes through here, so a group label expands the
	// same way the rail expands it — see scope.go.
	scopeOf := func(r *http.Request) scopeSet {
		return resolveScope(strings.TrimSpace(r.URL.Query().Get("repo")), groups, projects)
	}

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		writeJSON(w, ix.Sessions(days, r.URL.Query().Get("project"), r.URL.Query().Get("status")))
	})

	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(r.PathValue("id"))
		if s == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, s)
	})

	// Cached milestone-group summaries. `fresh` is false when the session has
	// grown past the cached hash (or nothing is cached yet) — the UI then shows
	// the summarize button; stale summaries still render, prefix-aligned.
	mux.HandleFunc("GET /api/sessions/{id}/summaries", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(r.PathValue("id"))
		if s == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		sums, fresh := su.Cached(s.ID, s.Milestones)
		writeJSON(w, map[string]any{"summaries": sums, "fresh": fresh})
	})

	// Generate summaries for the session's milestone groups — the one endpoint
	// that leaves the machine (via the user's own `claude` CLI). Idempotent: a
	// fresh cache is returned without a new claude call.
	mux.HandleFunc("POST /api/sessions/{id}/summarize", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(r.PathValue("id"))
		if s == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		if len(s.MilestoneGroups) == 0 {
			writeJSONError(w, http.StatusBadRequest, "no milestones to summarize")
			return
		}
		sums, err := su.Summarize(r.Context(), s.ID, s.MilestoneGroups, s.Milestones)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, map[string]any{"summaries": sums, "fresh": true})
	})

	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, ix.Projects())
	})

	// --- todo board (local todos.json; the server is the single writer). Every
	// mutation broadcasts the fresh column-ordered list so other tabs stay in sync.

	mux.HandleFunc("GET /api/todos", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, todos.List())
	})

	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {
		var in todoCreate
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		todo, err := todos.CreateFull(in)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("todos-updated", todos.List())
		writeJSON(w, todo)
	})

	mux.HandleFunc("PATCH /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p todoPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		// Linked drawings must exist — a typo'd id would render as a dead chip.
		if p.LinkedDrawingIDs != nil {
			if bad := unknownDrawingID(drawings, *p.LinkedDrawingIDs); bad != "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown drawing id %q", bad))
				return
			}
		}
		// Linked docs must exist too, for the same reason.
		if p.LinkedDocIDs != nil {
			if bad := unknownDocID(docs, *p.LinkedDocIDs); bad != "" {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown doc id %q", bad))
				return
			}
		}
		todo, err := todos.Update(r.PathValue("id"), p)
		if errors.Is(err, errTodoNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if p.Status != nil {
			todo = refreezeTodo(todos, ix, todo, *p.Status)
		}
		hub.broadcast("todos-updated", todos.List())
		writeJSON(w, todo)
	})

	mux.HandleFunc("DELETE /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := todos.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		// The card is gone, so its history can name nothing — the one place
		// the append-only log is allowed to shrink.
		events.PurgeTodo(id)
		hub.broadcast("todos-updated", todos.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// One card's activity feed, oldest first.
	mux.HandleFunc("GET /api/todos/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		evs, err := events.ForTodo(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, evs)
	})

	// The board-wide feed, newest first. `?limit=` caps at 500.
	mux.HandleFunc("GET /api/board/events", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		evs, err := events.Recent(n)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, evs)
	})

	// --- workflow columns (states.go). `?repo=` narrows to what one scope
	// sees: the shared columns plus that project's own.

	mux.HandleFunc("GET /api/board/states", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, states.ListForScope(scopeOf(r)))
	})

	mux.HandleFunc("POST /api/board/states", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string `json:"name"`
			Category string `json:"category"`
			Repo     string `json:"repo"`
			WIPLimit int    `json:"wipLimit"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		st, err := states.Create(in.Name, in.Category, in.Repo, in.WIPLimit)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("board-states-updated", states.List())
		writeJSON(w, st)
	})

	mux.HandleFunc("PATCH /api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p statePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		st, err := states.Update(r.PathValue("id"), p)
		if errors.Is(err, errStateNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A recategorised column changes what counts as done, so the board's
		// order and its snapshots both need re-reading.
		hub.broadcast("board-states-updated", states.List())
		hub.broadcast("todos-updated", todos.List())
		writeJSON(w, st)
	})

	// Deleting a column that still holds cards would strand them in a status
	// nothing renders, so the count is checked here — the side that can see
	// the cards — rather than inside the state store.
	mux.HandleFunc("DELETE /api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Builtin first: "this column can never be deleted" is the real reason,
		// and reporting "move the cards out" instead sends the user off to do
		// work that changes nothing.
		if builtinStates[id] {
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("%q is a builtin column and cannot be deleted", id))
			return
		}
		n := 0
		for _, t := range todos.List() {
			if t.Status == id {
				n++
			}
		}
		if n > 0 {
			writeJSONError(w, http.StatusConflict,
				fmt.Sprintf("%d card(s) are still in this column — move them first", n))
			return
		}
		if err := states.Delete(id); err != nil {
			if errors.Is(err, errStateNotFound) {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("board-states-updated", states.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// --- cycles (cycles.go): the sprints cards are planned into, plus the two
	// reports that make the bookkeeping pay for itself.

	mux.HandleFunc("GET /api/cycles", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, cycles.ListForScope(scopeOf(r)))
	})

	mux.HandleFunc("POST /api/cycles", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string    `json:"name"`
			Repo     string    `json:"repo"`
			Goal     string    `json:"goal"`
			StartsAt time.Time `json:"startsAt"`
			EndsAt   time.Time `json:"endsAt"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		c, err := cycles.Create(in.Name, in.Repo, in.Goal, in.StartsAt, in.EndsAt)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("cycles-updated", cycles.List())
		writeJSON(w, c)
	})

	mux.HandleFunc("PATCH /api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p cyclePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		c, err := cycles.Update(r.PathValue("id"), p)
		if errors.Is(err, errCycleNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("cycles-updated", cycles.List())
		writeJSON(w, c)
	})

	// Deleting a cycle keeps its cards — they fall back out of any cycle, the
	// way a deleted drawing is unlinked rather than taking the card with it.
	mux.HandleFunc("DELETE /api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := cycles.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if todos.UnlinkCycle(id) {
			hub.broadcast("todos-updated", todos.List())
		}
		hub.broadcast("cycles-updated", cycles.List())
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/cycles/{id}/burndown", func(w http.ResponseWriter, r *http.Request) {
		c, ok := cycles.Get(r.PathValue("id"))
		in := scopeOf(r)
		// A drill-down validates its target against the resolved scope, the way
		// the git tab's endpoints validate ?repo: without it this charted a
		// cycle that GET /api/cycles at the same scope says does not exist, so a
		// shared URL rendered a report for something the page cannot list.
		if !ok || !in.coversOwner(c.Repo) {
			writeJSONError(w, http.StatusNotFound, "cycle not found")
			return
		}
		bd, err := ComputeBurndown(c, todos, events, time.Now(), in)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, bd)
	})

	mux.HandleFunc("GET /api/cycles/velocity", func(w http.ResponseWriter, r *http.Request) {
		in := scopeOf(r)
		writeJSON(w, Velocity(cycles.ListForScope(in), todos, in))
	})

	// --- saved views (boardviews.go): a named filter plus the shape it draws.

	mux.HandleFunc("GET /api/board/views", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, views.ListForScope(scopeOf(r)))
	})

	mux.HandleFunc("POST /api/board/views", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string          `json:"name"`
			Repo  string          `json:"repo"`
			Kind  string          `json:"kind"`
			Query json.RawMessage `json:"query"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		v, err := views.Create(in.Name, in.Repo, in.Kind, in.Query)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("board-views-updated", views.List())
		writeJSON(w, v)
	})

	mux.HandleFunc("PATCH /api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p viewPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		v, err := views.Update(r.PathValue("id"), p)
		if errors.Is(err, errViewNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("board-views-updated", views.List())
		writeJSON(w, v)
	})

	mux.HandleFunc("DELETE /api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := views.Delete(r.PathValue("id")); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.broadcast("board-views-updated", views.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// --- design library (local drawings.json + drawings/*.excalidraw; the
	// server is the single writer). Mutations broadcast the fresh metadata list
	// so other tabs' libraries stay in sync; scene content itself is not
	// broadcast — concurrent editors are last-writer-wins.

	// Built here rather than in the composition root: publishing is
	// self-contained (env-configured, no shared state) and only the design
	// routes use it — keep the surface local to them.
	publisher := newPublisherFromEnv()

	mux.HandleFunc("GET /api/drawings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, drawings.List())
	})

	mux.HandleFunc("POST /api/drawings", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := drawings.Create(in.Name, in.Group)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("GET /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := drawings.Content(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(content)
	})

	// Scene saves can be big (pasted images arrive as data URLs in `files`),
	// so this endpoint gets its own generous cap instead of the 1MB one.
	// X-Base-Updated-At (the updatedAt the client last saw, RFC3339Nano) makes
	// the write conditional: a stale base gets 409 instead of clobbering a
	// save that happened elsewhere (another tab, an MCP client).
	mux.HandleFunc("PUT /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "scene too large (20MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				writeJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := drawings.SetContent(r.PathValue("id"), content, base)
		if errors.Is(err, errDrawingNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, errDrawingConflict) {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("PATCH /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Pointers so a metadata edit can carry any field: name renames,
		// group moves the drawing to a tab, topics replaces its tag set (and
		// ""/[] are real values — back to Ungrouped / untagged — distinct
		// from "not provided").
		var in struct {
			Name   *string   `json:"name"`
			Group  *string   `json:"group"`
			Topics *[]string `json:"topics"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		// The steps apply in sequence, so a failure midway can leave earlier
		// fields already persisted — track whether ANY step landed and
		// broadcast for it even on the error path, or other tabs would stay
		// stale about a change that did happen.
		applied := false
		d, err := drawings.Get(id)
		if err == nil && in.Name != nil {
			if d, err = drawings.Rename(id, *in.Name); err == nil {
				applied = true
			}
		}
		if err == nil && in.Group != nil {
			if d, err = drawings.SetGroup(id, *in.Group); err == nil {
				applied = true
			}
		}
		if err == nil && in.Topics != nil {
			if d, err = drawings.SetTopics(id, *in.Topics); err == nil {
				applied = true
			}
		}
		if applied {
			hub.broadcast("drawings-updated", drawings.List())
		}
		if errors.Is(err, errDrawingNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, d)
	})

	mux.HandleFunc("POST /api/drawings/{id}/duplicate", func(w http.ResponseWriter, r *http.Request) {
		d, err := drawings.Duplicate(r.PathValue("id"))
		if errors.Is(err, errDrawingNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		writeJSON(w, d)
	})

	// Publish is an explicit user action: push the current scene to the review
	// backend, then stamp PublishedAt with the version that was sent (the
	// ThumbUpdatedAt freshness idiom — edits after publish show as stale).
	mux.HandleFunc("POST /api/drawings/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		d, err := drawings.Get(id)
		if errors.Is(err, errDrawingNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		content, err := drawings.Content(id)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err := publisher.Publish(id, d.Name, content); err != nil {
			if errors.Is(err, errPublishNotConfigured) {
				writeJSONError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			// The backend, not this app, is unreachable/unhappy — 502 keeps
			// that distinction visible in the UI's error message.
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		out, err := drawings.MarkPublished(id, d.UpdatedAt)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		writeJSON(w, struct {
			Drawing
			ReviewURL string `json:"reviewUrl"`
		}{out, publisher.ReviewURL(id)})
	})

	// Thumbnails are rendered client-side (the browser is the only place the
	// Excalidraw renderer exists) and cached here. A GET misses (404) until a
	// thumbnail rendered from the CURRENT scene version has been uploaded —
	// the grid regenerates on miss, so MCP writes self-heal on the next view.
	mux.HandleFunc("GET /api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		b, err := drawings.Thumbnail(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	mux.HandleFunc("PUT /api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		base, err := time.Parse(time.RFC3339Nano, r.Header.Get("X-Base-Updated-At"))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "X-Base-Updated-At is required (the scene updatedAt the thumbnail was rendered from)")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "thumbnail too large (4MB max)")
			return
		}
		d, err := drawings.SetThumbnail(r.PathValue("id"), data, base)
		if errors.Is(err, errDrawingNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("DELETE /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := drawings.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.broadcast("drawings-updated", drawings.List())
		// Cards must not keep pointing at a drawing that no longer exists.
		if todos.UnlinkDrawing(id) {
			hub.broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- docs wiki (data.db; the server is the single writer). Mutations
	// broadcast the fresh metadata list so other tabs' trees stay in sync; the
	// page body is fetched per page and is last-writer-wins with optimistic
	// concurrency, exactly like the design library's scenes.

	mux.HandleFunc("GET /api/docs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, docs.List())
	})

	mux.HandleFunc("POST /api/docs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, ParentID, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Create(in.Title, in.ParentID, in.Group)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("docs-updated", docs.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("GET /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := docs.Get(r.PathValue("id"))
		if err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		body, _ := docs.Content(d.ID)
		writeJSON(w, docWithBody{Doc: d, Body: body})
	})

	// Body saves. X-Base-Updated-At (the updatedAt the client last saw,
	// RFC3339Nano) makes the write conditional: a stale base gets 409 instead
	// of clobbering a save that happened elsewhere (another tab, an MCP client).
	mux.HandleFunc("PUT /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "body too large (4MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				writeJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := docs.SetContent(r.PathValue("id"), string(body), base)
		if errors.Is(err, errDocNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, errDocConflict) {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("docs-updated", docs.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("PATCH /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p docPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Update(r.PathValue("id"), p)
		if errors.Is(err, errDocNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, errDocCycle) {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("docs-updated", docs.List())
		writeJSON(w, d)
	})

	mux.HandleFunc("DELETE /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := docs.Delete(id); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.broadcast("docs-updated", docs.List())
		// Cards must not keep pointing at a doc that no longer exists.
		if todos.UnlinkDoc(id) {
			hub.broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Project groups: named sets of project names for the nav's global scope.
	// Mutations broadcast groups-updated like the other stores, so a second
	// tab's switcher (or one watching an MCP writer) stays in sync.
	mux.HandleFunc("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, groups.List())
	})

	mux.HandleFunc("PUT /api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Projects []string `json:"projects"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		g, err := groups.Upsert(r.PathValue("name"), in.Projects)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("groups-updated", groups.List())
		writeJSON(w, g)
	})

	mux.HandleFunc("DELETE /api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := groups.Delete(r.PathValue("name")); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.broadcast("groups-updated", groups.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// Project registry: user-owned projects, decoupled from the raw ~/.claude
	// scan. Each owns the Claude folders (session cwd-basenames) it stands for;
	// mutations broadcast projects-updated like the other stores. (The plain
	// GET /api/projects still returns the raw index names — datalist fodder.)
	mux.HandleFunc("GET /api/projects/registry", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, projects.List())
	})

	// Claude folders the index reports that no registry entry owns yet —
	// offered in the manager so they can be claimed or merged.
	mux.HandleFunc("GET /api/projects/unmapped", func(w http.ResponseWriter, r *http.Request) {
		owned := map[string]bool{}
		for _, p := range projects.List() {
			for _, f := range p.Folders {
				owned[f] = true
			}
		}
		out := []string{}
		for _, name := range ix.Projects() {
			if !owned[name] {
				out = append(out, name)
			}
		}
		sort.Strings(out)
		writeJSON(w, out)
	})

	mux.HandleFunc("PUT /api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Folders []string `json:"folders"`
			Hidden  bool     `json:"hidden"`
			Ord     int      `json:"ord"`
			Parent  string   `json:"parent"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		p, err := projects.Upsert(r.PathValue("name"), in.Folders, in.Hidden, in.Ord, in.Parent)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.broadcast("projects-updated", projects.List())
		writeJSON(w, p)
	})

	mux.HandleFunc("DELETE /api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := projects.Delete(r.PathValue("name")); err != nil {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.broadcast("projects-updated", projects.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// Rename a project AND cascade the new name across every label that carried
	// the old one — cards, pages, drawings and group members. The name is the
	// label those items store, so an in-place rename without this cascade would
	// orphan them. A user-data change: the client confirms before calling.
	mux.HandleFunc("POST /api/projects/{name}/rename", func(w http.ResponseWriter, r *http.Request) {
		old := r.PathValue("name")
		var in struct {
			To string `json:"to"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			writeJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		to := strings.TrimSpace(in.To)
		// Projects and groups share the rail's namespace — refuse a group's name.
		for _, g := range groups.List() {
			if g.Name == to {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("%q is a group name", to))
				return
			}
		}
		if err := projects.Rename(old, to); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errProjectNotFound) {
				code = http.StatusNotFound
			}
			writeJSONError(w, code, err.Error())
			return
		}
		hub.broadcast("projects-updated", projects.List())
		if todos.RenameRepo(old, to) > 0 {
			hub.broadcast("todos-updated", todos.List())
		}
		if states.RenameRepo(old, to) > 0 {
			hub.broadcast("board-states-updated", states.List())
		}
		if cycles.RenameRepo(old, to) > 0 {
			hub.broadcast("cycles-updated", cycles.List())
		}
		if docs.RenameGroup(old, to) > 0 {
			hub.broadcast("docs-updated", docs.List())
		}
		if drawings.RenameGroup(old, to) > 0 {
			hub.broadcast("drawings-updated", drawings.List())
		}
		if views.RenameRepo(old, to) > 0 {
			hub.broadcast("board-views-updated", views.List())
		}
		if groups.RenameMember(old, to) > 0 {
			hub.broadcast("groups-updated", groups.List())
		}
		writeJSON(w, map[string]string{"name": to})
	})

	// Per-project logo (image bytes in data.db). PUT takes the raw image with
	// its Content-Type; GET serves it (cache-busted by the ?v= the client adds
	// from logoVersion); DELETE clears it. PUT/DELETE broadcast so the rail's
	// logo version refreshes everywhere.
	mux.HandleFunc("PUT /api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "image/") {
			writeJSONError(w, http.StatusBadRequest, "an image body is required")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "logo too large (2MB max)")
			return
		}
		if len(data) == 0 {
			writeJSONError(w, http.StatusBadRequest, "empty logo body")
			return
		}
		if err := projects.SetLogo(r.PathValue("name"), data, ct, time.Now().UnixMilli()); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errProjectNotFound) {
				code = http.StatusNotFound
			}
			writeJSONError(w, code, err.Error())
			return
		}
		hub.broadcast("projects-updated", projects.List())
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		data, ct, err := projects.Logo(r.PathValue("name"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", ct)
		// Immutable, with a ?v= cache-buster on the URL: the browser keeps the
		// image until logoVersion (hence the URL) changes.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Write(data)
	})

	mux.HandleFunc("DELETE /api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		if err := projects.DeleteLogo(r.PathValue("name")); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errProjectNotFound) {
				code = http.StatusNotFound
			}
			writeJSONError(w, code, err.Error())
			return
		}
		hub.broadcast("projects-updated", projects.List())
		w.WriteHeader(http.StatusNoContent)
	})

	registerCodegraphAPI(mux, ix)
	registerGitAPI(mux, ix)

	mux.Handle("/mcp", newMCPHandler(drawings, todos, states, cycles, docs, groups, projects, ix, hub))

	// Per-day activity buckets for the last `weeks` weeks (default 26), bucketed
	// by the local calendar day of each session's start. Powers the heatmap; its
	// window is independent of the sessions list's day filter.
	mux.HandleFunc("GET /api/activity", func(w http.ResponseWriter, r *http.Request) {
		weeks, _ := strconv.Atoi(r.URL.Query().Get("weeks"))
		if weeks <= 0 {
			weeks = 26
		}
		if weeks > 53 {
			weeks = 53
		}
		now := time.Now()
		startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		cutoff := startToday.AddDate(0, 0, -(weeks*7 - 1)) // inclusive of today

		// The heatmap is a full-history view: archived sessions are still activity
		// that happened, so it ignores the status filter (status "" = all).
		buckets := map[string]*activityDay{}
		for _, s := range ix.Sessions(0, r.URL.Query().Get("project"), "") {
			d := s.StartedAt.Local()
			if d.Before(cutoff) {
				continue
			}
			key := d.Format("2006-01-02")
			b := buckets[key]
			if b == nil {
				b = &activityDay{Date: key}
				buckets[key] = b
			}
			b.Sessions++
			b.Tokens += s.TotalTokens + s.AgentTokens
			b.CostUSD += s.CostUSD + s.AgentCostUSD
		}

		days := make([]*activityDay, 0, len(buckets))
		for _, b := range buckets {
			days = append(days, b)
		}
		sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
		writeJSON(w, map[string]any{"weeks": weeks, "days": days})
	})

	// Rework radar: the sessions index pivoted by file. `min` (default 2) drops
	// files only one session ever touched — one edit isn't rework — and `limit`
	// (default 50) bounds the payload; the response's totalFiles says how many
	// passed min, so the UI never implies it showed everything.
	mux.HandleFunc("GET /api/churn", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		min := 2
		if v, err := strconv.Atoi(q.Get("min")); err == nil && v > 0 {
			min = v
		}
		limit := 50
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		writeJSON(w, ix.Churn(days, q.Get("project"), min, limit))
	})

	// Friction: the sessions you kept stopping. `limit` (default 20) bounds the
	// list; totalSessions says how many had friction at all.
	mux.HandleFunc("GET /api/friction", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 20
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 200 {
			limit = 200
		}
		writeJSON(w, ix.Friction(days, q.Get("project"), limit))
	})

	// Work sizing: the sessions that outgrew their context. `limit` (default 20)
	// bounds the list; totalSessions says how many compacted at all.
	mux.HandleFunc("GET /api/sizing", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 20
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 200 {
			limit = 200
		}
		writeJSON(w, ix.Sizing(days, q.Get("project"), limit))
	})

	// Cost per outcome, week by week. No limit: a window holds a handful of
	// weeks, and dropping any would break the trend the block exists to show.
	mux.HandleFunc("GET /api/ledger", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		writeJSON(w, ix.Ledger(days, r.URL.Query().Get("project")))
	})

	// Ship history: recorded make check / make release runs from the drop dir
	// (see ships.go). Distinct from /api/ledger, the cost-per-outcome insights.
	// `log=1` includes each run's captured log tail.
	mux.HandleFunc("GET /api/ships", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 100
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		writeJSON(w, ix.Ships(q.Get("project"), days, limit, q.Get("log") == "1"))
	})

	// Transcript search over the FTS5 index (see search.go). `limit` (default
	// 100) bounds the response; `matched` reports what the cap left out.
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 100
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		writeJSON(w, ix.Search(q.Get("q"), days, q.Get("project"), limit))
	})

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		var st statsResponse
		for _, s := range ix.Sessions(days, r.URL.Query().Get("project"), r.URL.Query().Get("status")) {
			st.Sessions++
			st.TotalTokens += s.TotalTokens + s.AgentTokens
			st.TotalCost += s.CostUSD + s.AgentCostUSD
			st.AgentSpawns += s.AgentCount
			if s.Running {
				st.Running++
			}
		}
		writeJSON(w, st)
	})

	// Claude Code hooks POST their event JSON here for instant live updates —
	// an accelerator over the 500ms file-watch, which remains the fallback when
	// no hooks are installed. Fire-and-forget: the response is ignored by the
	// hook. The PreToolUse/PostToolUse/SubagentStop flood goes through the
	// coalescer (each rescan is a full re-parse; see rescanCoalescer); the rare
	// per-turn Notification/Stop signals parse synchronously because their
	// broadcasts need the freshly parsed session.
	rescans := newRescanCoalescer(func(path string) {
		if s := ix.RescanSession(path); s != nil {
			hub.broadcast("session-updated", ix.withStatus(s, time.Now()))
		}
	})
	mux.HandleFunc("POST /api/hook", func(w http.ResponseWriter, r *http.Request) {
		var ev struct {
			TranscriptPath string `json:"transcript_path"`
			SessionID      string `json:"session_id"`
			HookEventName  string `json:"hook_event_name"`
			Message        string `json:"message"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&ev); err != nil || ev.TranscriptPath == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !withinRoot(ix.root, ev.TranscriptPath) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch ev.HookEventName {
		// Distinct signals the UI turns into OS notifications:
		// Notification = Claude wants attention (needs input / permission);
		// Stop = a turn ended (the UI debounces it into a "finished" notify).
		case "Notification", "Stop":
			if s := ix.RescanSession(ev.TranscriptPath); s != nil {
				hub.broadcast("session-updated", ix.withStatus(s, time.Now()))
				if ev.HookEventName == "Notification" {
					hub.broadcast("session-attention", map[string]any{
						"id": s.ID, "title": s.Title, "project": s.Project, "message": ev.Message,
					})
				} else {
					hub.broadcast("session-stopped", map[string]any{
						"id": s.ID, "title": s.Title, "project": s.Project,
					})
				}
			}
		default:
			rescans.kick(ev.TranscriptPath)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Connection", "keep-alive")

		ch := hub.subscribe()
		defer hub.unsubscribe(ch)

		// keep-alive comment every 25s so proxies/browsers don't drop us
		tick := time.NewTicker(25 * time.Second)
		defer tick.Stop()

		fl.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-ch:
				w.Write(msg)
				fl.Flush()
			case <-tick.C:
				w.Write([]byte(": ping\n\n"))
				fl.Flush()
			}
		}
	})
}
