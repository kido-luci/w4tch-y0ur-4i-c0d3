package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
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

// claudeScope is one entry in the session views' own scope list: a repo the
// transcripts show work happening in, with the Claude folders that resolve to
// it. Folders that resolve to no repo appear as themselves.
type claudeScope struct {
	Key      string   `json:"key"`  // the URL scope segment — never contains "/"
	Name     string   `json:"name"` // what to show
	Slug     string   `json:"slug,omitempty"`
	Root     string   `json:"root,omitempty"`
	Folders  []string `json:"folders"` // what the session endpoints filter on
	Sessions int      `json:"sessions"`
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
	// PublicFolders answers which Claude folders may be shown while
	// presentation mode is on — the same closure PresentationGuard is built
	// with, so the switcher and the content it offers cannot disagree.
	PublicFolders func() map[string]bool
}

func Register(router chi.Router, d Deps) {
	// Bound to the names the handlers below already use. The struct is the
	// wiring contract at the boundary, not an indirection to thread through
	// sixty handler bodies.
	ix, hub, su := d.Index, d.Hub, d.Sum
	todos, states, cycles, events := d.Todos, d.States, d.Cycles, d.Events
	views, drawings, docs := d.Views, d.Drawings, d.Docs
	groups, projects := d.Groups, d.Projects
	settings, publicFolders := d.Settings, d.PublicFolders

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
		return projects.PrivateNames()
	}

	// Every scope question below goes through here, so a group label expands the
	// same way the rail expands it — see scope.go. Presentation mode subtracts
	// the private projects right here, so every scope-filtered endpoint hides
	// them without knowing the toggle exists.
	scopeOf := func(r *http.Request) board.ScopeSet {
		s := board.ResolveScope(strings.TrimSpace(r.URL.Query().Get("repo")), groups, projects)
		return s.WithExclude(privateNames())
	}

	// The git tab, the code graph and the GitHub sections resolve a scope to the
	// repos its projects are BOUND to — the project page's own taxonomy —
	// instead of to wherever that scope's sessions happened to run. It goes
	// through the same scope rule as every board endpoint above, presentation
	// exclusion included: the git tab is as much a screen-share surface as the
	// session list. Wired here rather than in main because this is where the
	// resolver those three packages are handed is built.
	rr.UseBindings(func(scope string) []repos.Binding {
		in := board.ResolveScope(strings.TrimSpace(scope), groups, projects).WithExclude(privateNames())
		out := []repos.Binding{}
		for _, p := range projects.List() {
			if !in.Covers(p.Name) {
				continue
			}
			// One Binding per CHECKOUT, not per project: BoundRoot whitelists by
			// full path, so several clones of one repo simply make the allowed
			// set larger, and the overview lists each checkout as its own row.
			for _, root := range p.RepoRoots {
				out = append(out, repos.Binding{Root: root, Project: p.Name})
			}
		}
		return out
	})

	router.Get("/api/sessions", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		httpx.WriteJSON(w, ix.Sessions(days, r.URL.Query().Get("project"), r.URL.Query().Get("status")))
	})

	router.Get("/api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(chi.URLParam(r, "id"))
		if s == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		httpx.WriteJSON(w, s)
	})

	// Cached milestone-group summaries. `fresh` is false when the session has
	// grown past the cached hash (or nothing is cached yet) — the UI then shows
	// the summarize button; stale summaries still render, prefix-aligned.
	router.Get("/api/sessions/{id}/summaries", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(chi.URLParam(r, "id"))
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
	router.Post("/api/sessions/{id}/summarize", func(w http.ResponseWriter, r *http.Request) {
		s := ix.Session(chi.URLParam(r, "id"))
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
	router.Get("/api/scopes", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/projects", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, ix.Projects())
	})

	// --- todo board (local todos.json; the server is the single writer). Every
	// mutation broadcasts the fresh column-ordered list so other tabs stay in sync.

	router.Get("/api/todos", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, todos.List())
	})

	router.Post("/api/todos", func(w http.ResponseWriter, r *http.Request) {
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

	router.Patch("/api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		todo, err := todos.Update(chi.URLParam(r, "id"), p)
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

	router.Delete("/api/todos/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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
	router.Get("/api/todos/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		evs, err := events.ForTodo(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		httpx.WriteJSON(w, evs)
	})

	// The board-wide feed, newest first. `?limit=` caps at 500.
	router.Get("/api/board/events", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/board/states", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, states.ListForScope(scopeOf(r)))
	})

	router.Post("/api/board/states", func(w http.ResponseWriter, r *http.Request) {
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

	router.Patch("/api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.StatePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		st, err := states.Update(chi.URLParam(r, "id"), p)
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
	router.Delete("/api/board/states/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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

	router.Get("/api/cycles", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, cycles.ListForScope(scopeOf(r)))
	})

	router.Post("/api/cycles", func(w http.ResponseWriter, r *http.Request) {
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

	router.Patch("/api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.CyclePatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		c, err := cycles.Update(chi.URLParam(r, "id"), p)
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
	router.Delete("/api/cycles/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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

	router.Get("/api/cycles/{id}/burndown", func(w http.ResponseWriter, r *http.Request) {
		c, ok := cycles.Get(chi.URLParam(r, "id"))
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

	router.Get("/api/cycles/velocity", func(w http.ResponseWriter, r *http.Request) {
		in := scopeOf(r)
		httpx.WriteJSON(w, board.Velocity(cycles.ListForScope(in), todos, in))
	})

	// --- saved views (boardviews.go): a named filter plus the shape it draws.

	router.Get("/api/board/views", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, views.ListForScope(scopeOf(r)))
	})

	router.Post("/api/board/views", func(w http.ResponseWriter, r *http.Request) {
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

	router.Patch("/api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.ViewPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		v, err := views.Update(chi.URLParam(r, "id"), p)
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

	router.Delete("/api/board/views/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := views.Delete(chi.URLParam(r, "id")); err != nil {
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

	router.Get("/api/drawings", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, drawings.List())
	})

	router.Post("/api/drawings", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		content, err := drawings.Content(chi.URLParam(r, "id"))
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
	router.Put("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		d, err := drawings.SetContent(chi.URLParam(r, "id"), content, base)
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

	router.Patch("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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

	router.Post("/api/drawings/{id}/duplicate", func(w http.ResponseWriter, r *http.Request) {
		d, err := drawings.Duplicate(chi.URLParam(r, "id"))
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
	router.Post("/api/drawings/{id}/publish", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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
	router.Get("/api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
		b, err := drawings.Thumbnail(chi.URLParam(r, "id"))
		if err != nil {
			httpx.WriteJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})

	router.Put("/api/drawings/{id}/thumbnail", func(w http.ResponseWriter, r *http.Request) {
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
		d, err := drawings.SetThumbnail(chi.URLParam(r, "id"), data, base)
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

	router.Delete("/api/drawings/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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

	router.Get("/api/docs", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, docs.List())
	})

	router.Post("/api/docs", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := docs.Get(chi.URLParam(r, "id"))
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
	router.Put("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
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
		d, err := docs.SetContent(chi.URLParam(r, "id"), string(body), base)
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

	router.Patch("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p board.DocPatch
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		d, err := docs.Update(chi.URLParam(r, "id"), p)
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

	router.Delete("/api/docs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
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
	router.Get("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, groups.List())
	})

	router.Put("/api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Projects []string `json:"projects"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		g, err := groups.Upsert(chi.URLParam(r, "name"), in.Projects)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		hub.Broadcast("groups-updated", groups.List())
		httpx.WriteJSON(w, g)
	})

	router.Delete("/api/groups/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := groups.Delete(chi.URLParam(r, "name")); err != nil {
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
	router.Get("/api/projects/registry", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, projects.List())
	})

	// Claude folders the index reports that no registry entry owns yet —
	// offered in the manager so they can be claimed or merged.
	router.Get("/api/projects/unmapped", func(w http.ResponseWriter, r *http.Request) {
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

	router.Put("/api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Folders []string `json:"folders"`
			Hidden  bool     `json:"hidden"`
			Ord     int      `json:"ord"`
			Parent  string   `json:"parent"`
			// RepoRoots is the project's binding: every local checkout of the one
			// repo it IS. A pointer so that omitting the field leaves the binding
			// alone — every other caller of this endpoint (rename, reorder, a
			// folder edit) would otherwise send an empty list and silently unbind
			// the project. Sent whole, so removing a path is the same request
			// shape as adding one.
			RepoRoots *[]string `json:"repoRoots"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, "bad JSON body")
			return
		}
		p, err := projects.Upsert(chi.URLParam(r, "name"), in.Folders, in.Hidden, in.Ord, in.Parent)
		if err != nil {
			httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if in.RepoRoots != nil {
			// Only real checkouts may be bound: the git and code-graph endpoints
			// will operate on whatever is stored here, so this is the gate that
			// keeps them pointed at repos instead of at any path a request cared
			// to name. Stored canonical, so a worktree binds to the repo it
			// belongs to — which is also what makes the sameness check below
			// meaningful, since two worktrees of one repo canonicalise equal.
			roots := make([]string, 0, len(*in.RepoRoots))
			for _, raw := range *in.RepoRoots {
				root := strings.TrimSpace(raw)
				if root == "" {
					continue
				}
				if !git.IsRepo(root) {
					httpx.WriteJSONError(w, http.StatusBadRequest,
						fmt.Sprintf("%q is not a git repository", root))
					return
				}
				roots = append(roots, git.CanonicalRoot(root))
			}
			// Several roots mean several CHECKOUTS OF ONE REPO, never several
			// repos: the project's slug, its GitHub sections and its visibility
			// are single values derived from the binding, and two different
			// remotes under one project would make each of them a coin toss.
			// Compared by remote URL, so a clone and its worktree pass and two
			// unrelated repos do not. A root with no remote at all can only join
			// other rootless ones — nothing else could confirm they match.
			if len(roots) > 1 {
				first, _ := git.RemoteURL(roots[0], "origin")
				for _, other := range roots[1:] {
					if u, _ := git.RemoteURL(other, "origin"); u != first {
						httpx.WriteJSONError(w, http.StatusBadRequest,
							"every path must be a checkout of the same repo — "+other+" has a different remote")
						return
					}
				}
			}
			stored, err := projects.SetRepoRoots(p.Name, roots)
			if err != nil {
				httpx.WriteJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			// Reply with what was STORED, not with what arrived: two worktrees of
			// one repo canonicalise to the same root and collapse to one entry,
			// and echoing the request would advertise a duplicate that is not
			// there.
			root := ""
			if len(stored) > 0 {
				root = stored[0]
			}
			p.RepoRoot, p.RepoRoots = root, stored
			if root != "" {
				// Read the remote here rather than leaving the row unresolved
				// until the next five-minute tick: that window would render as
				// "no GitHub remote", which is a claim, not a wait. Visibility
				// stays with the sync — it is the gh round-trip, and a save
				// should not block on the network.
				slug, kind := "", board.LinkLocal
				if s, ok := github.Slug(root); ok {
					slug, kind = s, board.LinkLinked
				}
				projects.SetRepoDerived(p.Name, slug, kind)
				p.RepoSlug, p.LinkKind = slug, kind
			} else {
				// Upsert built this reply before the binding was cleared, so
				// without this the response still advertises the old link.
				p.RepoSlug, p.LinkKind = "", board.LinkNone
			}
		}
		hub.Broadcast("projects-updated", projects.List())
		httpx.WriteJSON(w, p)
	})

	// The CLAUDE family's own taxonomy — the counterpart to /api/scopes, which
	// serves the project family. Every Claude folder is grouped by the repo its
	// sessions actually ran in, so the session views scope by repo without
	// asking the project registry anything: /project curates names, /claude
	// reports what happened, and neither is the other's source of truth.
	//
	// A folder whose sessions resolve to no repo — a directory since deleted,
	// work outside any checkout — stands as its own scope under its raw name
	// rather than being dropped or guessed into someone else's repo. So does a
	// folder whose only match was by name (Guessed): grouping on that would
	// merge two projects' sessions on the strength of a coincidence.
	//
	// `key` is what rides the URL's scope segment, so it never contains "/" —
	// the repo's own name, or the folder's. On the rare collision (two hosts,
	// one repo name) the owner is prefixed, which keeps it stable rather than
	// letting whichever loaded first win.
	claudeScopes := func() []claudeScope {
		// Presentation mode filters this list by the same allowlist the session
		// endpoints apply, or the switcher would offer scopes whose content the
		// guard then withholds — every pick landing on an empty list. It is the
		// registry's notion of public (a folder owned by a public project), so
		// while the mode is on this list is narrower than what /claude can
		// actually show; the two agreeing beats each being right alone.
		var public map[string]bool
		if settings != nil && settings.PresentationHidden() && publicFolders != nil {
			public = publicFolders()
		}
		keep := func(folder string) bool { return public == nil || public[folder] }

		counts := map[string]int{}
		for _, s := range ix.Snapshot() {
			counts[s.Project]++
		}
		byRoot := map[string]*claudeScope{}
		out := []claudeScope{}
		for _, folder := range ix.Projects() {
			if !keep(folder) {
				continue
			}
			rs := rr.Repos(folder)
			if len(rs) == 0 || rs[0].Guessed {
				out = append(out, claudeScope{
					Key: folder, Name: folder, Folders: []string{folder}, Sessions: counts[folder],
				})
				continue
			}
			root := git.CanonicalRoot(rs[0].Root)
			if cur, ok := byRoot[root]; ok {
				cur.Folders = append(cur.Folders, folder)
				cur.Sessions += counts[folder]
				continue
			}
			cs := &claudeScope{
				Key: git.RepoName(root), Name: git.RepoName(root), Root: root,
				Folders: []string{folder}, Sessions: counts[folder],
			}
			if s, ok := github.Slug(root); ok {
				cs.Slug = s
			}
			byRoot[root] = cs
		}
		for _, cs := range byRoot {
			out = append(out, *cs)
		}
		// Disambiguate a repeated key with its owner, both sides, so neither
		// entry wins by load order.
		seen := map[string]int{}
		for i := range out {
			seen[out[i].Key]++
		}
		for i := range out {
			if seen[out[i].Key] > 1 && out[i].Slug != "" {
				out[i].Key = strings.ReplaceAll(out[i].Slug, "/", "-")
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Sessions != out[j].Sessions {
				return out[i].Sessions > out[j].Sessions // busiest first
			}
			return out[i].Name < out[j].Name
		})
		return out
	}
	router.Get("/api/claude/scopes", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, claudeScopes())
	})

	// The repos this machine knows about, for the manager's binding picker:
	// every root the sessions resolve to, canonical and deduped, with the name
	// the repo calls itself. It is a SUGGESTION list — /project stores whatever
	// binding you choose, and a repo that has never seen a session can still be
	// bound by path — so reading the session index here creates no dependency
	// the project page has to keep.
	router.Get("/api/repos", func(w http.ResponseWriter, r *http.Request) {
		type known struct {
			Root    string `json:"root"`
			Name    string `json:"name"`
			Slug    string `json:"slug,omitempty"`
			BoundBy string `json:"boundBy,omitempty"` // project already on this repo
			ByName  bool   `json:"byName,omitempty"`  // only found by matching the folder's name
		}
		bound := map[string]string{}
		for _, p := range projects.List() {
			if p.RepoRoot != "" {
				bound[p.RepoRoot] = p.Name
			}
		}
		seen, out := map[string]bool{}, []known{}
		add := func(rp repos.Repo) {
			root := git.CanonicalRoot(rp.Root)
			if seen[root] {
				return
			}
			seen[root] = true
			k := known{Root: root, Name: git.RepoName(root), BoundBy: bound[root], ByName: rp.Guessed}
			if s, ok := github.Slug(root); ok {
				k.Slug = s
			}
			out = append(out, k)
		}
		for _, rp := range rr.Repos("") {
			add(rp)
		}
		// Repos("") cannot reach the name-matching fallback (it only runs for a
		// scoped call), so the repos that most need binding — the ones with no
		// session cwd of their own — would be missing from the very list that
		// exists to bind them. Ask per folder to reach them, and mark them: a
		// suggestion the user picks becomes an explicit binding, which is the
		// difference between offering a name match and acting on one.
		for _, folder := range ix.Projects() {
			for _, rp := range rr.Repos(folder) {
				add(rp)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		httpx.WriteJSON(w, out)
	})

	router.Delete("/api/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := projects.Delete(chi.URLParam(r, "name")); err != nil {
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
	router.Get("/api/presentation", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, map[string]bool{"hidden": settings != nil && settings.PresentationHidden()})
	})
	router.Put("/api/presentation", func(w http.ResponseWriter, r *http.Request) {
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
	router.Post("/api/projects/{name}/rename", func(w http.ResponseWriter, r *http.Request) {
		old := chi.URLParam(r, "name")
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
	router.Put("/api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
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
		if err := projects.SetLogo(chi.URLParam(r, "name"), data, ct, time.Now().UnixMilli()); err != nil {
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

	router.Get("/api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		data, ct, err := projects.Logo(chi.URLParam(r, "name"))
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

	router.Delete("/api/projects/{name}/logo", func(w http.ResponseWriter, r *http.Request) {
		if err := projects.DeleteLogo(chi.URLParam(r, "name")); err != nil {
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

	codegraph.Register(router, rr)
	figfiles.Register(router, rr)
	git.Register(router, rr)
	github.Register(router, rr)

	// Per-day activity buckets for the last `weeks` weeks (default 26), bucketed
	// by the local calendar day of each session's start. Powers the heatmap; its
	// window is independent of the sessions list's day filter.
	router.Get("/api/activity", func(w http.ResponseWriter, r *http.Request) {
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
	router.Get("/api/churn", func(w http.ResponseWriter, r *http.Request) {
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
	router.Get("/api/friction", func(w http.ResponseWriter, r *http.Request) {
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
	router.Get("/api/sizing", func(w http.ResponseWriter, r *http.Request) {
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
	router.Get("/api/ledger", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		httpx.WriteJSON(w, ix.Ledger(days, r.URL.Query().Get("project")))
	})

	// Ship history: recorded make check / make release runs from the drop dir
	// (see internal/ships). Distinct from /api/ledger, the cost-per-outcome insights.
	// `log=1` includes each run's captured log tail.
	router.Get("/api/ships", func(w http.ResponseWriter, r *http.Request) {
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
	router.Get("/api/search", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/stats", func(w http.ResponseWriter, r *http.Request) {
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
	router.Post("/api/hook", func(w http.ResponseWriter, r *http.Request) {
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

	router.Get("/api/events", func(w http.ResponseWriter, r *http.Request) {
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

// PresentationGuard narrows the session-derived endpoint family to the public
// projects while presentation mode is on. Those endpoints share one convention —
// a `project` query param carrying comma-separated Claude folders, empty
// meaning all of them — and the guard rewrites that param in place: an empty
// param becomes every PUBLIC project's folders, and a non-empty one keeps only
// the folders among them. The handlers (sessions, insights, stats, search, git,
// code graph) never learn the toggle exists.
//
// It is an allowlist rather than a subtraction of the private folders, because
// the folders no project owns are neither: subtracting left every unclaimed
// folder — raw name, session titles and all — on screen mid-demo. Which is also
// why there is no "no private projects, pass through" shortcut here.
//
// What counts as public is the REPO's own visibility, not the registry's view
// of it (see the closure in main). Keying it on the registry meant a session
// only showed if some project had claimed its folder AND that project was
// bound to a public repo — two conditions for a question about one repo, and
// on a real board it hid work that had been public on GitHub all along.
//
// /api/ships is the one exception: its `project` values are Makefile-reported
// names, a different namespace, so its handler subtracts by project NAME
// itself. Writes, non-/api paths and the SSE stream pass through untouched.
func PresentationGuard(settings *board.SettingsStore, publicFolders func() map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if settings == nil || !settings.PresentationHidden() ||
			r.Method != http.MethodGet ||
			!strings.HasPrefix(r.URL.Path, "/api/") ||
			r.URL.Path == "/api/ships" {
			next.ServeHTTP(w, r)
			return
		}
		public := publicFolders()
		q := r.URL.Query()
		var keep []string
		if cur := q.Get("project"); cur != "" {
			for _, f := range index.SplitProjects(cur) {
				if public[f] {
					keep = append(keep, f)
				}
			}
		} else {
			for f := range public {
				keep = append(keep, f)
			}
			sort.Strings(keep) // map order is random; keep the param stable
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
