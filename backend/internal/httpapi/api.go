package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/codegraph"
	"watch-your-ai-code/internal/cowork"
	"watch-your-ai-code/internal/figfiles"
	"watch-your-ai-code/internal/git"
	"watch-your-ai-code/internal/github"
	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/index"
	"watch-your-ai-code/internal/repos"
	"watch-your-ai-code/internal/search"
	"watch-your-ai-code/internal/ships"
	"watch-your-ai-code/internal/sse"
	"watch-your-ai-code/internal/summarize"
)

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
	board.Doc
	Body string `json:"body"`
}

// Deps is everything the handlers are wired over, named at the call site.
//
// It replaced a thirteen-parameter list. Every type in it is distinct, so the
// compiler was already catching transposed arguments; what it could not catch
// was the edit cost — adding one store meant changing the signature, changing
// main's call, and lining up thirteen positional arguments across the two by
// eye. A keyed struct literal says which store is which, and a new dependency
// is one field rather than a signature change.
type Deps struct {
	Index    *index.Index
	Hub      *sse.Hub
	Sum      *summarize.Summarizer
	Todos    *board.TodoStore
	States   *board.StateStore
	Cycles   *board.CycleStore
	Events   *board.EventStore
	Views    *board.ViewStore
	Drawings *board.DrawingStore
	Docs     *board.DocStore
	Groups   *board.GroupStore
	Projects *board.ProjectStore
	Settings *board.SettingsStore
}

func Register(mux *http.ServeMux, d Deps) {
	// Bound to the names the handlers below already use. The struct is the
	// wiring contract at the boundary, not an indirection to thread through
	// sixty handler bodies.
	ix, hub, su := d.Index, d.Hub, d.Sum
	todos, states, cycles, events := d.Todos, d.States, d.Cycles, d.Events
	views, drawings, docs := d.Views, d.Drawings, d.Docs
	groups, projects := d.Groups, d.Projects
	settings := d.Settings

	// Repo resolution, ship records and transcript search read the index but are
	// not part of it — each takes the narrow slice of it that it needs, so none
	// of them (nor the handlers below, nor MCP) depends on the whole index.
	rr := repos.New(ix)
	shipStore := ships.New(ix.DB(), ix)
	searchIdx := search.New(ix.DB(), ix)

	// privateNames is the presentation-mode subtraction: the private projects'
	// names while the toggle is on, empty otherwise — so every caller can apply
	// it unconditionally.
	privateNames := func() map[string]bool {
		if settings == nil || !settings.PresentationHidden() {
			return nil
		}
		names, _ := projects.PrivateSets()
		return names
	}

	// Every scope question below goes through here, so a group label expands the
	// same way the rail expands it — see scope.go. Presentation mode subtracts
	// the private projects right here, so every scope-filtered endpoint hides
	// them without knowing the toggle exists.
	scopeOf := func(r *http.Request) board.ScopeSet {
		s := board.ResolveScope(strings.TrimSpace(r.URL.Query().Get("repo")), groups, projects)
		return s.WithExclude(privateNames())
	}

	mux.HandleFunc("GET /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		httpx.WriteJSON(w, ix.Sessions(days, r.URL.Query().Get("project"), r.URL.Query().Get("status")))
	})

	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(r.PathValue("id"))
		if s == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		httpx.WriteJSON(w, s)
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
		httpx.WriteJSON(w, map[string]any{"summaries": sums, "fresh": fresh})
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
			httpx.WriteJSONError(w, http.StatusBadRequest, "no milestones to summarize")
			return
		}
		sums, err := su.Summarize(r.Context(), s.ID, s.MilestoneGroups, s.Milestones)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		httpx.WriteJSON(w, map[string]any{"summaries": sums, "fresh": true})
	})

	// The resolved scope index: every label the rail can show, mapped to the
	// project names whose CARDS it covers.
	//
	// It exists so the rule lives in exactly one place. The client used to
	// recompute it from /api/groups + /api/projects — a second implementation of
	// board.ResolveScope, in another language, agreeing only by inspection. Two copies
	// of one rule drift the moment someone edits one, and the last time this rule
	// was wrong a workflow column vanished from a member project.
	mux.HandleFunc("GET /api/scopes", func(w http.ResponseWriter, r *http.Request) {
		// Presentation mode subtracts private projects from every label's
		// coverage. The client filters its lists by these sets, so trimming
		// them here hides private cards/pages/drawings in every label-based
		// view without the client learning a second rule.
		private := privateNames()
		out := map[string][]string{}
		add := func(label string) {
			if label == "" {
				return
			}
			in := board.ResolveScope(label, groups, projects)
			names := make([]string, 0, len(in.Cards))
			for n := range in.Cards {
				if private[n] {
					continue
				}
				names = append(names, n)
			}
			sort.Strings(names) // stable payload, so a refetch diffs cleanly
			out[label] = names
		}
		for _, p := range projects.List() {
			add(p.Name)
		}
		for _, g := range groups.List() {
			add(g.Name)
		}
		httpx.WriteJSON(w, out)
	})

	mux.HandleFunc("GET /api/projects", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, ix.Projects())
	})

	// --- todo board (local todos.json; the server is the single writer). Every
	// mutation broadcasts the fresh column-ordered list so other tabs stay in sync.

	mux.HandleFunc("GET /api/todos", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, todos.List())
	})

	mux.HandleFunc("POST /api/todos", func(w http.ResponseWriter, r *http.Request) {
		var in board.TodoCreate
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		todo, err := todos.CreateFull(in)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("todos-updated", todos.List())
		httpx.WriteJSON(w, todo)
	})

	mux.HandleFunc("PATCH /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.TodoPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		// Linked drawings must exist — a typo'd id would render as a dead chip.
		if p.LinkedDrawingIDs != nil {
			if bad := board.UnknownDrawingID(drawings, *p.LinkedDrawingIDs); bad != "" {
				httpx.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown drawing id %q", bad))
				return
			}
		}
		// Linked docs must exist too, for the same reason.
		if p.LinkedDocIDs != nil {
			if bad := board.UnknownDocID(docs, *p.LinkedDocIDs); bad != "" {
				httpx.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("unknown doc id %q", bad))
				return
			}
		}
		todo, err := todos.Update(r.PathValue("id"), p)
		if errors.Is(err, board.ErrTodoNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if p.Status != nil {
			todo = board.RefreezeTodo(todos, ix, todo, *p.Status)
		}
		hub.Broadcast("todos-updated", todos.List())
		httpx.WriteJSON(w, todo)
	})

	mux.HandleFunc("DELETE /api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := todos.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		// The card is gone, so its history can name nothing — the one place
		// the append-only log is allowed to shrink.
		events.PurgeTodo(id)
		hub.Broadcast("todos-updated", todos.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// One card's activity feed, oldest first.
	mux.HandleFunc("GET /api/todos/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		evs, err := events.ForTodo(r.PathValue("id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, evs)
	})

	// The board-wide feed, newest first. `?limit=` caps at 500.
	mux.HandleFunc("GET /api/board/events", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		evs, err := events.Recent(n)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, evs)
	})

	// --- workflow columns (states.go). `?repo=` narrows to what one scope
	// sees: the shared columns plus that project's own.

	mux.HandleFunc("GET /api/board/states", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, states.ListForScope(scopeOf(r)))
	})

	mux.HandleFunc("POST /api/board/states", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name     string `json:"name"`
			Category string `json:"category"`
			Repo     string `json:"repo"`
			WIPLimit int    `json:"wipLimit"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		st, err := states.Create(in.Name, in.Category, in.Repo, in.WIPLimit)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("board-states-updated", states.List())
		httpx.WriteJSON(w, st)
	})

	mux.HandleFunc("PATCH /api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.StatePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		st, err := states.Update(r.PathValue("id"), p)
		if errors.Is(err, board.ErrStateNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		// A recategorised column changes what counts as done, so the board's
		// order and its snapshots both need re-reading.
		hub.Broadcast("board-states-updated", states.List())
		hub.Broadcast("todos-updated", todos.List())
		httpx.WriteJSON(w, st)
	})

	// Deleting a column that still holds cards would strand them in a status
	// nothing renders, so the count is checked here — the side that can see
	// the cards — rather than inside the state store.
	mux.HandleFunc("DELETE /api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		// Builtin first: "this column can never be deleted" is the real reason,
		// and reporting "move the cards out" instead sends the user off to do
		// work that changes nothing.
		if board.BuiltinStates[id] {
			httpx.WriteJSONError(w, http.StatusBadRequest,
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
			httpx.WriteJSONError(w, http.StatusConflict,
				fmt.Sprintf("%d card(s) are still in this column — move them first", n))
			return
		}
		if err := states.Delete(id); err != nil {
			if errors.Is(err, board.ErrStateNotFound) {
				httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("board-states-updated", states.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// --- cycles (cycles.go): the sprints cards are planned into, plus the two
	// reports that make the bookkeeping pay for itself.

	mux.HandleFunc("GET /api/cycles", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, cycles.ListForScope(scopeOf(r)))
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
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		c, err := cycles.Create(in.Name, in.Repo, in.Goal, in.StartsAt, in.EndsAt)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("cycles-updated", cycles.List())
		httpx.WriteJSON(w, c)
	})

	mux.HandleFunc("PATCH /api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.CyclePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		c, err := cycles.Update(r.PathValue("id"), p)
		if errors.Is(err, board.ErrCycleNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("cycles-updated", cycles.List())
		httpx.WriteJSON(w, c)
	})

	// Deleting a cycle keeps its cards — they fall back out of any cycle, the
	// way a deleted drawing is unlinked rather than taking the card with it.
	mux.HandleFunc("DELETE /api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := cycles.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if todos.UnlinkCycle(id) {
			hub.Broadcast("todos-updated", todos.List())
		}
		hub.Broadcast("cycles-updated", cycles.List())
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/cycles/{id}/burndown", func(w http.ResponseWriter, r *http.Request) {
		c, ok := cycles.Get(r.PathValue("id"))
		in := scopeOf(r)
		// A drill-down validates its target against the resolved scope, the way
		// the git tab's endpoints validate ?repo: without it this charted a
		// cycle that GET /api/cycles at the same scope says does not exist, so a
		// shared URL rendered a report for something the page cannot list.
		if !ok || !in.CoversOwner(c.Repo) {
			httpx.WriteJSONError(w, http.StatusNotFound, "cycle not found")
			return
		}
		bd, err := board.ComputeBurndown(c, todos, events, time.Now(), in)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, bd)
	})

	mux.HandleFunc("GET /api/cycles/velocity", func(w http.ResponseWriter, r *http.Request) {
		in := scopeOf(r)
		httpx.WriteJSON(w, board.Velocity(cycles.ListForScope(in), todos, in))
	})

	// --- saved views (boardviews.go): a named filter plus the shape it draws.

	mux.HandleFunc("GET /api/board/views", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, views.ListForScope(scopeOf(r)))
	})

	mux.HandleFunc("POST /api/board/views", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name  string          `json:"name"`
			Repo  string          `json:"repo"`
			Kind  string          `json:"kind"`
			Query json.RawMessage `json:"query"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		v, err := views.Create(in.Name, in.Repo, in.Kind, in.Query)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("board-views-updated", views.List())
		httpx.WriteJSON(w, v)
	})

	mux.HandleFunc("PATCH /api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.ViewPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		v, err := views.Update(r.PathValue("id"), p)
		if errors.Is(err, board.ErrViewNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("board-views-updated", views.List())
		httpx.WriteJSON(w, v)
	})

	mux.HandleFunc("DELETE /api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := views.Delete(r.PathValue("id")); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("board-views-updated", views.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// --- design library (local drawings.json + drawings/*.excalidraw; the
	// server is the single writer). Mutations broadcast the fresh metadata list
	// so other tabs' libraries stay in sync; scene content itself is not
	// broadcast — concurrent editors are last-writer-wins.

	// Built here rather than in the composition root: publishing is
	// self-contained (env-configured, no shared state) and only the design
	// routes use it — keep the surface local to them.
	publisher := cowork.NewPublisherFromEnv()

	mux.HandleFunc("GET /api/drawings", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, drawings.List())
	})

	mux.HandleFunc("POST /api/drawings", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := drawings.Create(in.Name, in.Group)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("GET /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := drawings.Content(r.PathValue("id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
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
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "scene too large (20MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				httpx.WriteJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := drawings.SetContent(r.PathValue("id"), content, base)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDrawingConflict) {
			httpx.WriteJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
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
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
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
			hub.Broadcast("drawings-updated", drawings.List())
		}
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("POST /api/drawings/{id}/duplicate", func(w http.ResponseWriter, r *http.Request) {
		d, err := drawings.Duplicate(r.PathValue("id"))
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	// Publish is an explicit user action: push the current scene to the review
	// backend, then stamp PublishedAt with the version that was sent (the
	// ThumbUpdatedAt freshness idiom — edits after publish show as stale).
	mux.HandleFunc("POST /api/drawings/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		d, err := drawings.Get(id)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		content, err := drawings.Content(id)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err := publisher.Publish(id, d.Name, content); err != nil {
			if errors.Is(err, cowork.ErrPublishNotConfigured) {
				httpx.WriteJSONError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			// The backend, not this app, is unreachable/unhappy — 502 keeps
			// that distinction visible in the UI's error message.
			httpx.WriteJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		out, err := drawings.MarkPublished(id, d.UpdatedAt)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, struct {
			board.Drawing
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
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	mux.HandleFunc("PUT /api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		base, err := time.Parse(time.RFC3339Nano, r.Header.Get("X-Base-Updated-At"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "X-Base-Updated-At is required (the scene updatedAt the thumbnail was rendered from)")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "thumbnail too large (4MB max)")
			return
		}
		d, err := drawings.SetThumbnail(r.PathValue("id"), data, base)
		if errors.Is(err, board.ErrDrawingNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("DELETE /api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := drawings.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("drawings-updated", drawings.List())
		// Cards must not keep pointing at a drawing that no longer exists.
		if todos.UnlinkDrawing(id) {
			hub.Broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// --- docs wiki (data.db; the server is the single writer). Mutations
	// broadcast the fresh metadata list so other tabs' trees stay in sync; the
	// page body is fetched per page and is last-writer-wins with optimistic
	// concurrency, exactly like the design library's scenes.

	mux.HandleFunc("GET /api/docs", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, docs.List())
	})

	mux.HandleFunc("POST /api/docs", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title, ParentID, Group string }
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Create(in.Title, in.ParentID, in.Group)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("GET /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := docs.Get(r.PathValue("id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		body, _ := docs.Content(d.ID)
		httpx.WriteJSON(w, docWithBody{Doc: d, Body: body})
	})

	// Body saves. X-Base-Updated-At (the updatedAt the client last saw,
	// RFC3339Nano) makes the write conditional: a stale base gets 409 instead
	// of clobbering a save that happened elsewhere (another tab, an MCP client).
	mux.HandleFunc("PUT /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "body too large (4MB max)")
			return
		}
		var base time.Time
		if h := r.Header.Get("X-Base-Updated-At"); h != "" {
			if base, err = time.Parse(time.RFC3339Nano, h); err != nil {
				httpx.WriteJSONError(w, http.StatusBadRequest, "bad X-Base-Updated-At (want RFC3339)")
				return
			}
		}
		d, err := docs.SetContent(r.PathValue("id"), string(body), base)
		if errors.Is(err, board.ErrDocNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDocConflict) {
			httpx.WriteJSONError(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("PATCH /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.DocPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Update(r.PathValue("id"), p)
		if errors.Is(err, board.ErrDocNotFound) {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, board.ErrDocCycle) {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		httpx.WriteJSON(w, d)
	})

	mux.HandleFunc("DELETE /api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if err := docs.Delete(id); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("docs-updated", docs.List())
		// Cards must not keep pointing at a doc that no longer exists.
		if todos.UnlinkDoc(id) {
			hub.Broadcast("todos-updated", todos.List())
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Project groups: named sets of project names for the nav's global scope.
	// Mutations broadcast groups-updated like the other stores, so a second
	// tab's switcher (or one watching an MCP writer) stays in sync.
	mux.HandleFunc("GET /api/groups", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, groups.List())
	})

	mux.HandleFunc("PUT /api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Projects []string `json:"projects"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		g, err := groups.Upsert(r.PathValue("name"), in.Projects)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("groups-updated", groups.List())
		httpx.WriteJSON(w, g)
	})

	mux.HandleFunc("DELETE /api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := groups.Delete(r.PathValue("name")); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("groups-updated", groups.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// Project registry: user-owned projects, decoupled from the raw ~/.claude
	// scan. Each owns the Claude folders (session cwd-basenames) it stands for;
	// mutations broadcast projects-updated like the other stores. (The plain
	// GET /api/projects still returns the raw index names — datalist fodder.)
	mux.HandleFunc("GET /api/projects/registry", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, projects.List())
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
		httpx.WriteJSON(w, out)
	})

	mux.HandleFunc("PUT /api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Folders []string `json:"folders"`
			Hidden  bool     `json:"hidden"`
			Ord     int      `json:"ord"`
			Parent  string   `json:"parent"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		p, err := projects.Upsert(r.PathValue("name"), in.Folders, in.Hidden, in.Ord, in.Parent)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("projects-updated", projects.List())
		httpx.WriteJSON(w, p)
	})

	mux.HandleFunc("DELETE /api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := projects.Delete(r.PathValue("name")); err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		hub.Broadcast("projects-updated", projects.List())
		w.WriteHeader(http.StatusNoContent)
	})

	// Presentation mode: the one switch that hides private projects app-wide
	// (rail, scope resolution, session endpoints, MCP) while you demo or
	// screenshot. Server-side state so every tab and consumer flips together;
	// the broadcast is what makes open tabs re-render.
	mux.HandleFunc("GET /api/presentation", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, map[string]bool{"hidden": settings != nil && settings.PresentationHidden()})
	})
	mux.HandleFunc("PUT /api/presentation", func(w http.ResponseWriter, r *http.Request) {
		if settings == nil {
			httpx.WriteJSONError(w, http.StatusServiceUnavailable, "settings store unavailable")
			return
		}
		var in struct {
			Hidden bool `json:"hidden"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		if err := settings.SetPresentationHidden(in.Hidden); err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		hub.Broadcast("presentation-updated", map[string]bool{"hidden": in.Hidden})
		httpx.WriteJSON(w, map[string]bool{"hidden": in.Hidden})
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
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		to := strings.TrimSpace(in.To)
		// Projects and groups share the rail's namespace — refuse a group's name.
		for _, g := range groups.List() {
			if g.Name == to {
				httpx.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("%q is a group name", to))
				return
			}
		}
		if err := projects.Rename(old, to); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, board.ErrProjectNotFound) {
				code = http.StatusNotFound
			}
			httpx.WriteJSONError(w, code, err.Error())
			return
		}
		hub.Broadcast("projects-updated", projects.List())
		if todos.RenameRepo(old, to) > 0 {
			hub.Broadcast("todos-updated", todos.List())
		}
		if states.RenameRepo(old, to) > 0 {
			hub.Broadcast("board-states-updated", states.List())
		}
		if cycles.RenameRepo(old, to) > 0 {
			hub.Broadcast("cycles-updated", cycles.List())
		}
		if docs.RenameGroup(old, to) > 0 {
			hub.Broadcast("docs-updated", docs.List())
		}
		if drawings.RenameGroup(old, to) > 0 {
			hub.Broadcast("drawings-updated", drawings.List())
		}
		if views.RenameRepo(old, to) > 0 {
			hub.Broadcast("board-views-updated", views.List())
		}
		if groups.RenameMember(old, to) > 0 {
			hub.Broadcast("groups-updated", groups.List())
		}
		httpx.WriteJSON(w, map[string]string{"name": to})
	})

	// Per-project logo (image bytes in data.db). PUT takes the raw image with
	// its Content-Type; GET serves it (cache-busted by the ?v= the client adds
	// from logoVersion); DELETE clears it. PUT/DELETE broadcast so the rail's
	// logo version refreshes everywhere.
	mux.HandleFunc("PUT /api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if !strings.HasPrefix(ct, "image/") {
			httpx.WriteJSONError(w, http.StatusBadRequest, "an image body is required")
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusRequestEntityTooLarge, "logo too large (2MB max)")
			return
		}
		if len(data) == 0 {
			httpx.WriteJSONError(w, http.StatusBadRequest, "empty logo body")
			return
		}
		if err := projects.SetLogo(r.PathValue("name"), data, ct, time.Now().UnixMilli()); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, board.ErrProjectNotFound) {
				code = http.StatusNotFound
			}
			httpx.WriteJSONError(w, code, err.Error())
			return
		}
		hub.Broadcast("projects-updated", projects.List())
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
			if errors.Is(err, board.ErrProjectNotFound) {
				code = http.StatusNotFound
			}
			httpx.WriteJSONError(w, code, err.Error())
			return
		}
		hub.Broadcast("projects-updated", projects.List())
		w.WriteHeader(http.StatusNoContent)
	})

	codegraph.Register(mux, rr)
	figfiles.Register(mux, rr)
	git.Register(mux, rr)
	github.Register(mux, rr)

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
		httpx.WriteJSON(w, map[string]any{"weeks": weeks, "days": days})
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
		httpx.WriteJSON(w, ix.Churn(days, q.Get("project"), min, limit))
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
		httpx.WriteJSON(w, ix.Friction(days, q.Get("project"), limit))
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
		httpx.WriteJSON(w, ix.Sizing(days, q.Get("project"), limit))
	})

	// Cost per outcome, week by week. No limit: a window holds a handful of
	// weeks, and dropping any would break the trend the block exists to show.
	mux.HandleFunc("GET /api/ledger", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		httpx.WriteJSON(w, ix.Ledger(days, r.URL.Query().Get("project")))
	})

	// Ship history: recorded make check / make release runs from the drop dir
	// (see internal/ships). Distinct from /api/ledger, the cost-per-outcome insights.
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
		out := shipStore.List(q.Get("project"), days, limit, q.Get("log") == "1")
		// Ship records carry Makefile-reported project names — a different
		// namespace from Claude folders, so PresentationGuard skips this path
		// and the private-NAME subtraction happens here instead.
		if private := privateNames(); len(private) > 0 {
			kept := make([]ships.ShipRecord, 0, len(out.Ships))
			for _, s := range out.Ships {
				if !private[s.Project] {
					kept = append(kept, s)
				}
			}
			out.Total -= len(out.Ships) - len(kept)
			out.Ships = kept
		}
		httpx.WriteJSON(w, out)
	})

	// Transcript search over the FTS5 index (see internal/search). `limit` (default
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
		httpx.WriteJSON(w, searchIdx.Search(q.Get("q"), days, q.Get("project"), limit))
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
		httpx.WriteJSON(w, st)
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
			hub.Broadcast("session-updated", ix.WithStatus(s, time.Now()))
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
		if !httpx.WithinRoot(ix.Root(), ev.TranscriptPath) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch ev.HookEventName {
		// Distinct signals the UI turns into OS notifications:
		// Notification = Claude wants attention (needs input / permission);
		// Stop = a turn ended (the UI debounces it into a "finished" notify).
		case "Notification", "Stop":
			if s := ix.RescanSession(ev.TranscriptPath); s != nil {
				hub.Broadcast("session-updated", ix.WithStatus(s, time.Now()))
				if ev.HookEventName == "Notification" {
					hub.Broadcast("session-attention", map[string]any{
						"id": s.ID, "title": s.Title, "project": s.Project, "message": ev.Message,
					})
				} else {
					hub.Broadcast("session-stopped", map[string]any{
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

		ch := hub.Subscribe()
		defer hub.Unsubscribe(ch)

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

// PresentationGuard hides private projects from the session-derived endpoint
// family while presentation mode is on. Those endpoints share one convention —
// a `project` query param carrying comma-separated Claude folders, empty
// meaning all of them — and the guard rewrites that param in place: an empty
// param becomes every folder the index knows minus the private ones, and a
// non-empty one loses its private folders. The handlers (sessions, insights,
// stats, search, git, code graph) never learn the toggle exists.
//
// /api/ships is the one exception: its `project` values are Makefile-reported
// names, a different namespace, so its handler subtracts by project NAME
// itself. Writes, non-/api paths and the SSE stream pass through untouched.
//
// `folders` supplies every folder the index knows (ix.Projects at the call
// site) — a func, not a slice, because the index rescans behind this handler.
func PresentationGuard(settings *board.SettingsStore, projects *board.ProjectStore, folders func() []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if settings == nil || !settings.PresentationHidden() ||
			r.Method != http.MethodGet ||
			!strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/api/ships" {
			next.ServeHTTP(w, r)
			return
		}
		_, private := projects.PrivateSets()
		if len(private) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		var keep []string
		if cur := q.Get("project"); cur != "" {
			for _, f := range index.SplitProjects(cur) {
				if !private[f] {
					keep = append(keep, f)
				}
			}
		} else {
			for _, f := range folders() {
				if !private[f] {
					keep = append(keep, f)
				}
			}
		}
		if len(keep) == 0 {
			// An empty param means "all folders" — the opposite of what an
			// emptied filter should mean — so send a token no folder can be.
			keep = []string{"\x00none"}
		}
		q.Set("project", strings.Join(keep, ","))
		r2 := r.Clone(r.Context())
		r2.URL.RawQuery = q.Encode()
		next.ServeHTTP(w, r2)
	})
}
