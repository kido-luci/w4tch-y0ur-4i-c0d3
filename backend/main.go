package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/fdgauge"
	"watch-your-ai-code/internal/git"
	"watch-your-ai-code/internal/github"
	"watch-your-ai-code/internal/httpapi"
	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/index"
	"watch-your-ai-code/internal/mcpserver"
	"watch-your-ai-code/internal/repos"
	"watch-your-ai-code/internal/ships"
	"watch-your-ai-code/internal/sse"
	"watch-your-ai-code/internal/summarize"
)

//go:embed all:internal/web/dist
var distFS embed.FS

func main() {
	defaultRoot := filepath.Join(os.Getenv("HOME"), ".claude", "projects")
	root := flag.String("root", defaultRoot, "Claude Code projects directory")
	addr := flag.String("addr", "127.0.0.1:4777", "listen address (keep it on loopback)")
	configDir := flag.String("config-dir", "", "board + design library directory (default: the OS config dir)")
	shipsDir := flag.String("ships-dir", "", "ship-record drop directory (default: ~/.wyac/ships)")
	printHooks := flag.Bool("print-hooks", false, "print Claude Code hook config for instant live updates, then exit")
	flag.Parse()

	if *printHooks {
		emitHookConfig(*addr)
		return
	}

	cfgDir := *configDir
	if cfgDir == "" {
		cfgDir = defaultConfigDir()
	}

	ix := index.New(*root)
	if db, err := index.OpenDB(cfgDir, *root); err != nil {
		// No cache is a slower boot and an empty search, never a dead viewer.
		log.Printf("index cache disabled: %v", err)
	} else {
		ix.UseCache(db)
	}
	ix.RefreshArchived()
	start := time.Now()
	updated, err := ix.Rescan()
	if err != nil {
		log.Fatalf("initial scan: %v", err)
	}
	log.Printf("indexed %d sessions in %s", len(updated), time.Since(start).Round(time.Millisecond))

	// Ship records (make check / make release drops — see internal/ships). Scanned
	// after the index DB is open, watched alongside the transcripts.
	sd := *shipsDir
	if sd == "" {
		sd = ships.DefaultDir()
	}
	shipStore := ships.New(ix.DB(), ix)
	if rs := shipStore.Scan(sd); len(rs) > 0 {
		log.Printf("ships: %d records ingested", len(rs))
	}

	// Both watchers poll: each tick rides the stores' own change detection
	// (session stamps, the ships known-files set), so it re-parses only what
	// moved and doubles as the reconcile — new projects, deletions and
	// anything missed while asleep are caught within a tick, so there is no
	// separate safety-net rescan loop.
	hub := sse.New()
	index.Watch(ix, 2*time.Second, func(s *index.Session) { hub.Broadcast("session-updated", s) })
	if err := ships.Watch(shipStore, sd, 2*time.Second, func(r *ships.ShipRecord) { hub.Broadcast("ship-recorded", r) }); err != nil {
		log.Printf("ships watch disabled: %v", err)
	}

	// When fds run out the failures look unrelated (empty design library,
	// accept errors) and a restart destroys the evidence — the census in the
	// log is the post-mortem, and it names the kind of fd doing the leaking.
	fdgauge.Every(10 * time.Minute)

	// Archiving happens in the app, not via transcript writes, so poll the
	// session store on its own cadence to keep active/archived status fresh.
	go func() {
		for range time.Tick(45 * time.Second) {
			ix.RefreshArchived()
		}
	}()

	// data.db: the durable half — board + design library. A failure here is
	// fatal on purpose: refusing to start beats starting over the user's data.
	dataDB, err := board.OpenDB(cfgDir)
	if err != nil {
		log.Fatalf("data.db: %v", err)
	}
	board.ImportOnce(dataDB, cfgDir)
	go board.Backup(dataDB, cfgDir)

	todoStore := board.NewTodoStore(dataDB)
	stateStore := board.NewStateStore(dataDB)
	cycleStore := board.NewCycleStore(dataDB)
	eventStore := board.NewEventStore(dataDB)
	viewStore := board.NewViewStore(dataDB)
	// The board's columns and its history are injected rather than constructed
	// inside TodoStore: each store keeps its own serving copy of one table, and
	// a second instance would be a second writer over it.
	todoStore.UseStates(stateStore)
	todoStore.UseEvents(eventStore)
	drawingStore := board.NewDrawingStore(dataDB)
	docStore := board.NewDocStore(dataDB)
	groupStore := board.NewGroupStore(dataDB)
	projectStore := board.NewProjectStore(dataDB)
	settingsStore := board.NewSettingsStore(dataDB)
	// Seed the project registry from the content taxonomy (add-only, keeps
	// names): every label the board / docs / design actually carry. The Claude
	// session scan is NOT a source — which folders sessions ran in doesn't
	// invent projects; nothing is renamed or rewritten.
	if n := board.SeedProjects(projectStore, groupStore, todoStore, docStore, drawingStore); n > 0 {
		log.Printf("projects: seeded %d registry entries", n)
	}

	mux := http.NewServeMux()
	httpapi.Register(mux, httpapi.Deps{
		Index:    ix,
		Hub:      hub,
		Sum:      summarize.New(),
		Todos:    todoStore,
		States:   stateStore,
		Cycles:   cycleStore,
		Events:   eventStore,
		Views:    viewStore,
		Drawings: drawingStore,
		Docs:     docStore,
		Groups:   groupStore,
		Projects: projectStore,
		Settings: settingsStore,
	})
	mux.Handle("/mcp", mcpserver.Handler(drawingStore, todoStore, stateStore, cycleStore, docStore, groupStore, projectStore, settingsStore, shipStore, ix, hub))

	// syncRegistry keeps the two things a project cannot state about itself in
	// step with the binding the user gave it: what the filesystem says about
	// that repo, and whether it is public. Both are STORED on the row rather
	// than recomputed per request, so nothing on a request path shells out to
	// git or waits on gh.
	//
	// The binding itself is never inferred here — the project page is its own
	// taxonomy and a repo nobody named is a guess. The one exception is the
	// FIRST pass over a row that has never been resolved (LinkUnset): it may
	// adopt the repo the project's Claude folders resolve to, as an opening
	// offer. Clearing a binding stamps LinkNone, so the offer is never made
	// twice; see ProjectStore.AdoptRepoRoot.
	//
	// Visibility keeps its safe default: bound to a public GitHub repo → public;
	// anything else — private repo, no remote, no binding, a path that has gone
	// missing, or a failed gh call → private, because public screenshots must
	// only ever show work that is already public. The gh answer is cached per
	// slug (github.IsPrivate), so a tick is cheap.
	rr := repos.New(ix)

	syncRegistry := func() bool {
		changed := false
		for _, p := range projectStore.List() {
			if p.LinkKind == board.LinkUnset && p.RepoRoot == "" && len(p.Folders) > 0 {
				// Repos("") would mean ALL folders, not none — hence the guard.
				// A name-matched root is not evidence of anything, so it is
				// never adopted — it is offered in the manager's picker instead,
				// labelled as such, where accepting it makes the binding yours.
				if rs := rr.Repos(strings.Join(p.Folders, ",")); len(rs) > 0 && !rs[0].Guessed {
					if projectStore.AdoptRepoRoot(p.Name, git.CanonicalRoot(rs[0].Root)) {
						p.RepoRoot = git.CanonicalRoot(rs[0].Root)
						changed = true
					}
				}
			}

			private := true
			if p.RepoRoot != "" {
				slug, kind := "", board.LinkLocal
				switch {
				case !git.IsRepo(p.RepoRoot):
					// Bound to a path that is gone or no longer a checkout.
					// Saying "missing" rather than dropping the binding is the
					// point: a moved repo must not look like one nobody bound.
					kind = board.LinkMissing
				default:
					if s, ok := github.Slug(p.RepoRoot); ok {
						slug, kind = s, board.LinkLinked
						if priv, ok := github.IsPrivate(p.RepoRoot); ok && !priv {
							private = false
						}
					}
				}
				if projectStore.SetRepoDerived(p.Name, slug, kind) {
					changed = true
				}
			}
			// An unbound row is deliberately NOT stamped: LinkNone means the
			// user cleared the binding, and writing it here would erase the
			// never-resolved state on the first tick — foreclosing the opening
			// offer for every project whose repo only appears later.
			if projectStore.SetPrivate(p.Name, private) {
				changed = true
			}
		}
		return changed
	}

	// Adopt freshly-labelled content into the registry (a card/page/drawing
	// given a label that has no project row yet gets one), so nothing sits
	// under an unselectable scope for long. Add-only; broadcast only on change.
	// The same tick re-derives each project's repo link and GitHub visibility,
	// and boot runs one sync immediately so presentation mode doesn't spend its
	// first minutes wrong.
	go func() {
		if syncRegistry() {
			hub.Broadcast("projects-updated", projectStore.List())
		}
		for range time.Tick(5 * time.Minute) {
			changed := board.SeedProjects(projectStore, groupStore, todoStore, docStore, drawingStore) > 0
			if syncRegistry() {
				changed = true
			}
			if changed {
				hub.Broadcast("projects-updated", projectStore.List())
			}
		}
	}()

	dist, err := fs.Sub(distFS, "internal/web/dist")
	if err != nil {
		log.Fatal(err)
	}
	files := http.FileServerFS(dist)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback: the client routes on real paths (History API), so a clean
		// route like /project/<scope>/git is not a file. Serve the embedded file
		// when it exists; otherwise hand back index.html and let the client router
		// take over. A missing /assets/ file stays a 404 — masking it with the
		// HTML shell would feed a script/style tag a 200 of HTML.
		//
		// /api and /mcp are 404'd here for the same reason, one level up: only
		// REGISTERED patterns are more specific than "/", so an unrouted API
		// path lands in this handler and would answer 200 with a web page to a
		// caller expecting JSON. That went unnoticed while every /api path had
		// a handler; removing the service proxies turned three live endpoints
		// into HTML-200s and made it visible.
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(dist, name); err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/mcp" {
				httpx.WriteJSONError(w, http.StatusNotFound, "no such endpoint")
				return
			}
			if strings.HasPrefix(r.URL.Path, "/assets/") || strings.HasPrefix(r.URL.Path, "/excalidraw-assets/") {
				http.NotFound(w, r)
				return
			}
			r.URL.Path = "/" // the SPA shell; the client reads the real path itself
		}
		// Vite assets are content-hashed (immutable); the shell must always
		// revalidate or rebuilds serve a stale UI from browser cache.
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}))

	log.Printf("watch-your-ai-code on http://%s (root: %s, config: %s)", *addr, *root, cfgDir)
	// hostGuard wraps EVERYTHING (API, MCP, static): loopback alone doesn't
	// stop DNS rebinding or blind cross-origin POSTs — see internal/httpx.
	// Inside it, PresentationGuard narrows the session-endpoint family to the
	// public projects' folders while presentation mode is on.
	handler := httpapi.PresentationGuard(settingsStore, projectStore, mux)
	log.Fatal(http.ListenAndServe(*addr, httpx.HostGuard(*addr, handler)))
}

// defaultConfigDir is where the board and design library (data.db) live unless
// -config-dir says otherwise: the OS config dir, falling back to $HOME/.config.
// Pointing a second instance at its own directory is the whole reason the flag
// exists — two binaries sharing one data store is how the old todos.json lost
// fields silently, so the dev server (see `make dev`) keeps its own.
func defaultConfigDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(base, "watch-your-ai-code")
}

// emitHookConfig prints a Claude Code hooks block (JSON to stdout, guidance to
// stderr) that POSTs each tool/agent/stop event to the running server for
// instant live updates. The command is fire-and-forget with a short timeout and
// always exits 0, so it never stalls or breaks a Claude Code session — even
// when the viewer isn't running (connection refused returns immediately). An
// empty matcher matches every tool.
func emitHookConfig(addr string) {
	cmd := fmt.Sprintf("curl -sm2 -X POST http://%s/api/hook -d @- >/dev/null 2>&1 || true", addr)
	perTool := []map[string]any{{
		"matcher": "",
		"hooks":   []map[string]string{{"type": "command", "command": cmd}},
	}}
	onEvent := []map[string]any{{
		"hooks": []map[string]string{{"type": "command", "command": cmd}},
	}}
	cfg := map[string]any{
		"hooks": map[string]any{
			"PreToolUse":   perTool,
			"PostToolUse":  perTool,
			"SubagentStop": onEvent,
			"Stop":         onEvent,
			"Notification": onEvent, // needs-input / permission → attention notify
		},
	}
	fmt.Fprintln(os.Stderr, "# Merge this into ~/.claude/settings.json (into an existing \"hooks\" block if you have one).")
	fmt.Fprintln(os.Stderr, "# With it installed, live updates are instant; without it the viewer still works via 500ms file-watch.")
	fmt.Fprintln(os.Stderr, "# The command is fire-and-forget and adds no tokens to your Claude Code sessions.")
	// SetEscapeHTML(false) keeps the shell redirections (>, &) readable rather
	// than emitting > / & escapes into the pasted config.
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(cfg)
}
