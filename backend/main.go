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

	// syncProjectRepos derives, in one pass over the registry, the two things a
	// project cannot state about itself: which repo it is bound to, and whether
	// that repo is public. Both come out of the same resolution, and both are
	// STORED on the row rather than recomputed per request — resolving stats the
	// filesystem once per candidate session cwd, which is fine on a five-minute
	// tick and not fine on the session endpoints' hot path.
	//
	// Visibility keeps its safe default: every resolved repo public → public; a
	// private repo, no GitHub remote, no resolvable repo at all, or a failed gh
	// call → private — public screenshots must only ever show work that is
	// already public. The gh answer is cached per slug (github.IsPrivate), so a
	// tick is cheap.
	//
	// The link records how far the binding could be PROVEN: a root the name
	// fallback guessed stays `guessed` and carries no slug, so nothing
	// downstream can mistake it for ownership.
	rr := repos.New(ix)
	syncProjectRepos := func() bool {
		changed := false
		for _, p := range projectStore.List() {
			private := true
			root, slug, kind, count := "", "", board.LinkNone, 0
			if len(p.Folders) > 0 { // Repos("") would mean ALL folders, not none
				if roots := rr.Repos(strings.Join(p.Folders, ",")); len(roots) > 0 {
					allPublic := true
					for _, r := range roots {
						// A guessed root is a directory that merely shares the
						// folder's name — calling a project public on that
						// basis would let presentation mode show work off a
						// repo nobody proved is this project's.
						if r.Guessed {
							allPublic = false
							break
						}
						priv, ok := github.IsPrivate(r.Root)
						if !ok || priv {
							allPublic = false
							break
						}
					}
					private = !allPublic
					first := roots[0]
					// The repo, not the directory the session happened to run
					// in: a linked worktree has its own path, so without this a
					// folder working in one binds to a root nothing else can
					// ever match.
					root, count = git.CanonicalRoot(first.Root), len(roots)
					if first.Guessed {
						kind = board.LinkGuessed
					} else if s, ok := github.Slug(first.Root); ok {
						slug, kind = s, board.LinkLinked
					} else {
						kind = board.LinkLocal
					}
				}
			}
			if projectStore.SetPrivate(p.Name, private) {
				changed = true
			}
			if projectStore.SetRepoLink(p.Name, root, slug, kind, count) {
				changed = true
			}
		}
		return changed
	}

	// seedRepoProjects grows the registry from the code instead of waiting for
	// someone to type a folder name: a Claude folder no project owns, whose
	// sessions resolve to a real repo, becomes a project named after that REPO
	// (the checkout's directory is often not the repo's name). Folders that
	// resolve to nothing — a directory since deleted, work outside any repo —
	// are left alone, because inventing a project for them would be the
	// unchecked guessing this replaces; so is a `guessed` root, which only
	// matched a directory by name.
	//
	// Add-only, the contract SeedProjects already has for labels: the sessions
	// are the evidence, so a row deleted while its folder keeps producing
	// sessions comes back on the next tick.
	seedRepoProjects := func() bool {
		owned := map[string]bool{}
		for _, p := range projectStore.List() {
			for _, f := range p.Folders {
				owned[f] = true
			}
		}
		added := false
		for _, folder := range ix.Projects() {
			if owned[folder] {
				continue
			}
			rs := rr.Repos(folder)
			if len(rs) == 0 || rs[0].Guessed {
				continue
			}
			root := git.CanonicalRoot(rs[0].Root)
			if projectStore.SeedRepo(git.RepoName(root), folder) {
				added = true
			}
		}
		return added
	}

	// Seeding first, so a row created this pass gets its repo link and its
	// visibility in the same tick rather than five minutes later.
	syncRegistry := func() bool {
		changed := seedRepoProjects()
		if syncProjectRepos() {
			changed = true
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
	// Inside it, PresentationGuard subtracts private projects from the
	// session-endpoint family while presentation mode is on.
	handler := httpapi.PresentationGuard(settingsStore, projectStore, ix.Projects, mux)
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
