# Changelog

All notable changes to this project are documented here. Versions follow
[semantic versioning](https://semver.org/).

## v2.6.1 — 2026-08-03

### Fixed
- **Checkout names in the code tab were ellipsised at the wrong end.** The label
  is the last two path segments, so several checkouts under one workspace share a
  long prefix and differ only at the end — trimming the tail rendered four
  distinct repos as four identical `luci-studio_workspace/luc…` rows. The name
  cell now trims the FRONT (`…rkspace/luci-studio_admin`), with the path in a
  `<bdi>` so the right-to-left trick cannot reorder it.

## v2.6.0 — 2026-08-03

### Added
- **A project can be bound to several local checkouts of its repo.** A second
  clone kept on another branch, a copy made to try something — each gets its own
  row in the code tab, with its own branch and dirty count, and its own detail.
  The manager takes one path per line; the first line is the one anything needing
  a single root uses (the slug, the code graph, design files).
  - They must all be the **same repo** — the server compares remotes and refuses
    the save otherwise, naming the path that disagreed. A project's slug, GitHub
    sections and visibility are single values derived from the binding, and two
    remotes under one project would make each of them a coin toss.
  - A git **worktree is not a separate checkout**: it canonicalises to the repo
    it belongs to, so listing one alongside its parent collapses to a single row.
  - Rows are labelled by the last two path segments rather than the Claude
    folder, which is the same string for every clone of one repo.
- Schema v15 adds `project_repos`; existing bindings are copied into it and
  `projects.repo_root` stays as the first root.

### Changed
- **`style.css` is split into fifteen parts.** It was 6446 lines, and the reason
  it stayed that way was real — reordering the cascade breaks what no test here
  renders a page to see. The split was made provably inert instead: the parts are
  `@import`ed in the exact order they appeared, and the built CSS is
  byte-identical to before, same content hash and same 115392 bytes.
  `css/order.test.ts` pins it — every part imported exactly once, in prefix
  order, and `style.css` holding nothing but imports.
- **The view layer can be tested at all now.** vitest ran in a node environment,
  so the suite covered only the pure modules — 5 files against 57 — and every
  module that renders was on the wrong side of that line. A dead link left by
  the `code` tab rename passed tsc and the whole suite and was found by opening
  a browser. jsdom is configured, and two tests come with it: one asserts every
  internal `/project` and `/claude` link in the source names a tab that still
  exists (it reproduces that bug when reverted), one renders the code view and
  pins that two clones of one repo get distinct links and distinct names.
- **`httpapi/api.go` is 1201 lines rather than 1684.** Drawings, docs and the
  read-only analytics moved into their own files. The extracted functions take
  parameters named for the locals they replaced, so the handler bodies moved
  verbatim — a rename sweep across sixty bodies is where this kind of refactor
  goes wrong.
- **Routing moved from `http.ServeMux` to go-chi.** All 79 routes and 33 path
  params were converted; every package that owns a slice of the API now takes a
  `chi.Router` instead of a `*http.ServeMux`. No route, method or status changed
  — the SPA fallback is wired to chi's `MethodNotAllowed` as well as `NotFound`,
  because chi answers a known path with an unregistered method from its own 405
  handler, which would have returned an empty body where `/api/*` has always
  answered JSON.
- **The `git` and `codegraph` tabs are now one tab, `code`.** The overview is
  unchanged — one status row per repo in the scope — and the repo detail gained
  a `graph` tab alongside commits, changes, branches, pull requests and issues &
  CI. The graph reads the repo from the route rather than from a picker of its
  own, which removes the second place a repo could be selected: the URL and the
  graph could previously disagree about which repo you were looking at.

### Removed
- **The `/project/<scope>/git` and `/project/<scope>/codegraph` routes.** They
  are not redirected — `code` replaced both, and the old spellings now parse as
  a scope name rather than a tab. A saved link to either lands on the default
  view.

### Fixed
- **`make release-dry` replaced the production binary.** It depends on
  `check-run`, whose build wrote `-o ../watch-your-ai-code` — the file the
  launchd agent runs — so the one target that "tags nothing, pushes nothing,
  publishes nothing" was swapping the running binary underneath a live process.
  `check-run` takes a `CHECK_BIN` path now, still defaulting to the everyday
  binary (that delivery is deliberate); `release-dry-run` points it at `.dev/`.

## v2.5.0 — 2026-08-02

### Added
- **A Windows build.** The release now cross-compiles `windows/amd64`
  alongside the two darwin and two linux targets, shipped as a `.zip`
  holding a `.exe` — a tarball is neither runnable nor openable over
  there. Nothing needed a CGO escape hatch: the SQLite driver has been
  pure Go since the index cache landed.

### Fixed
- **Four paths were built from `$HOME`, which Windows usually leaves
  unset.** `filepath.Join` then quietly returns a RELATIVE path, so the
  transcript root, the config fallback, the archived-session store and the
  ship-drop directory would all have pointed at whatever directory the
  server happened to be started from. They ask `os.UserHomeDir()` now,
  which reads `USERPROFILE` there and `$HOME` everywhere else.

## v2.4.1 — 2026-08-01

### Fixed
- **A new project was born public.** The `private` column defaults to
  public and the insert omitted it, so every project created — by the
  manager, or by seeding from a label — was showable for the up to five
  minutes before the visibility sync first reached it. With presentation
  mode on that is a private repo's cards on screen mid-demo, which is the
  one thing the mode exists to prevent. New rows are written private;
  what the sync derives afterwards still wins.

## v2.4.0 — 2026-08-01

### Changed
- **The project page and the Claude pages stop sharing a taxonomy.** They
  shared one scope, so a label picked on `/project` arrived at `/claude`
  meaning something else — a project name where a session lives in a
  folder — and the session views had to go through the project registry to
  translate. Now each family answers for itself and remembers its own
  scope: `/project` scopes by the projects you curate, `/claude` by the
  repos its sessions actually ran in (`/api/claude/scopes`, derived from
  the transcripts). The URL grammar is unchanged; only what the scope
  segment means is per-family.
- **A project declares which repo it IS.** The git, code-graph, GitHub and
  design-file tabs used to resolve a scope through the directories Claude
  had run in, which made the project page's answer depend on where the
  agent had wandered. Bind a repo in the project manager instead — the
  picker offers every repo this machine has seen, and any checkout's path
  works. What the server could confirm about that binding rides beside it:
  linked (a GitHub remote, so visibility is knowable), local (a repo with
  no remote), missing (the bound path is gone), or unbound. A git icon
  after the project's name in the rail shows it at a glance.
- **A project with no binding lists no repos.** That is the answer, not a
  regression: the fix is to bind it, not to guess for it. Existing
  projects are offered their first binding once, from what their folders
  resolve to, and never from a directory matched by name alone.
- **`usage` is its own tab.** The sessions route was two pages stacked:
  totals, a heatmap, a model mix and a tokens chart on top of the list you
  actually search. Sessions keeps what is per-session; usage takes
  everything that aggregates. The filter window is shared between them.

### Fixed
- **Presentation mode asked the wrong thing about visibility.** It
  subtracted the private projects' folders, so every folder no project
  owned stayed on screen mid-demo — nine of them here, raw directory names
  and all. Asking the registry instead meant a session showed only if some
  project had claimed its folder AND that project was bound to a public
  repo: two conditions for a question about one repo, which on this board
  left 11 sessions visible out of six public repos' worth. It is the
  REPO's own visibility now — public on GitHub or hidden, with no repo at
  all counting as hidden.
- **A reload could leave you on a scope presentation mode hides.** Only the
  toggle bounced off it, so a refresh (or a shared link) left the chip and
  the URL printing a private project's name while the views quietly showed
  everything public instead of that scope.
- **A worktree or a submodule bound the wrong path.** Repo identity came
  from trimming `/.git` off git's common dir, which is only the shape a
  plain checkout has: a linked worktree resolved to a path nothing else
  could match, and a submodule to its superproject's storage — not a
  working tree at all.
- **The sessions list refetched on every SSE tick.** A running session
  emits one per tool use, and each answered with three requests on top of
  the row it had just been handed. The list now fetches nothing on a tick;
  the usage tab coalesces its own refresh.

## v2.3.0 — 2026-07-31

### Added
- **Private projects + presentation mode.** Mark a project private in the
  rail's project manager, and one switch — the eye at the bottom of the
  rail — hides it everywhere at once while you demo or screenshot: the
  rail and scope panels, every scope-resolved view, the session-derived
  endpoints (sessions, insights, stats, search), the git overview, ships,
  and MCP (#30). The state lives server-side, so every open tab flips
  together off one SSE echo; flip it back and everything returns —
  nothing is deleted, only hidden. `private` is orthogonal to `hidden`
  (which keeps a project off the rail always).

## v2.2.0 — 2026-07-31

### Added
- **The agent opens and closes its own sprints.** The MCP server grows
  `create_cycle` and `update_cycle` (#28) — the missing half of the cycle
  loop: cards could already be planned into a cycle (`cycleId`/`estimate`
  on `create_todo`/`update_todo`), but the window itself had to be opened
  and closed in the UI. Dates take RFC3339 or a bare `YYYY-MM-DD` read in
  the server's zone — midnight for a start, 23:59:59 for an end, the same
  rule the cycles view applies. Closing stamps the moment server-side, and
  delete stays UI-only like every other store.

## v2.1.0 — 2026-07-31

### Changed
- **Paper Editorial Cool — the app wears a new design language.** The pixel
  language (cream ground, Silkscreen chrome, hard offset shadows, 0-radius
  ink boxes) is replaced wholesale by an editorial one: a cool slate palette,
  Newsreader for headings and column heads, Public Sans for prose and UI,
  IBM Plex Mono staying on data — all vendored latin + vietnamese. Structure
  is hairlines and soft shadows; chips are pills; board columns open up into
  serif heads over a heavy rule, purple while in progress and green when
  done; the docs view becomes a single sheet with a scroll-spied "on this
  page" rail; markdown tables take the property-map shape. Colour speaks in
  roles — blue for navigation and links, teal for ids and hashes, pink for
  actions, purple for in-review, green for done/live — and every hue that
  colours text was measured to 4.5:1 rather than copied from the mock. Dark
  is a derived inversion on the same slate axis. The wordmark reads "Watch
  Your AI Code".
- **The design tab split into Wireframe and UI** — sketches and hi-fi
  screens are different artifacts and now have different tabs (#24), and the
  UI tab lists a scope's `.fig`/`.pen` files and opens them in OpenPencil
  (#21).

### Fixed
- **The file watcher no longer holds a kqueue fd per watched file.** It
  polls with stamp sweeps instead, dropping the ~4.3k-fd baseline (#26); a
  periodic fd census logs what remains (#25).
- **Design files resolve from the scope's folders, not its rail label**
  (#23).
- **The session detail no longer refetches summaries on every repaint** —
  the request is keyed to the milestones' freshness, not to the render loop
  (#14).

### Internal
- Frontend and backend are sibling trees of narrow packages instead of one
  nesting in the other (#13–#17); CI runs the frontend test suite (#15); and
  `make release-dry` rehearses the whole release path without publishing
  (#18).

## v2.0.1 — 2026-07-28

### Fixed
- **The activity heatmap no longer sits stranded in a panel three times its
  width.** It draws at a fixed 387px — 26 weeks of cells plus the weekday
  gutter — and filled 32% of the full-width card it had to itself, the only
  graphic in the app that did. It cannot be stretched to fit: the SVG sizes by
  its `viewBox`, so scaling it up would enlarge the month labels and the
  less/more legend by the same factor, and widening it with more weeks is
  capped at 53 by `/api/activity`, which still leaves a third of the panel
  empty. So it now shares a row with **model distribution**, taking exactly the
  width it draws and handing the rest to the distribution bar, which stretches.
  Below 900px the two stack. The two at-a-glance summaries also read better
  together, above the detailed tokens-over-time chart rather than split by it.

## v2.0.0 — 2026-07-28

### Added
- **Board depth** — the board tracks work the way a tracker does, not the way
  a sticky note does.
  - **Custom workflow columns.** Add, rename, reorder and WIP-limit your own
    columns ("In review", "Blocked", "Shipped"), globally or per project.
    Every column carries a category — `todo`, `started` or `done` — and it is
    the *category*, not the name, that decides whether landing there freezes a
    card's cost snapshot or auto-links the running session.
  - **Card hierarchy.** Cards nest two levels (epic → story → subtask) with a
    kind (epic/story/task/bug). A parent shows a live rollup of its children's
    progress and points. Deleting a parent promotes its children rather than
    deleting them.
  - **Cycles, estimates and priority.** Plan cards into a named sprint, size
    them in story points, and rank them 0–4. A **cycles tab** creates and
    closes sprints and draws a **burndown** chart per cycle plus a
    committed-vs-landed line per sprint, all computed from a new append-only
    event log rather than stored totals. Cards with no estimate are counted
    and named rather than silently omitted from the chart.
  - **Table and timeline views**, plus a filter bar (text, kind, cycle,
    unestimated) you can save as a named view per scope. The table view drags
    rows to reorder or to nest one card under another.
  - **History you can read.** Every column move, estimate, cycle, priority and
    re-parent is folded into a card's journey stream in its panel, and the
    cycles tab carries a board-wide *recent activity* feed. Both render from
    the same event log the burndown replays.
  - **MCP**: `list_board_states` and `list_cycles`, and `create_todo` /
    `update_todo` take the new fields — a session can file a sized epic with
    children in one pass.

### Fixed
- **Three badges were invisible.** A card's estimate, its cycle and its parent
  link were drawn with the nested-hairline border and no fill behind it, which
  is a 1.18:1 frame and nothing else — in both themes. They now carry the same
  fill as every other tag, and the muted text on that fill was given its own
  value so it still clears 4.5:1 where the plain muted would have fallen to
  4.12:1 in light.
- **One definition of scope, server-side.** The rail's scope label can name a
  project, a group standing for several, or a parent with children nested under
  it — and the client has always expanded it into a set. Three stores added with
  the deep board (columns, cycles, saved views) compared the raw label to a
  stored project name instead, so anything created while the rail sat on a group
  became invisible the moment you narrowed to one of its members: a column would
  disappear, taking its cards' status with it. MCP's `list_todos` had the same
  fault and answered a group name with zero cards. Everything now resolves
  through one function, which walks the tree in both directions — an ancestor's
  configuration governs the cards below it, and a descendant's holds cards the
  view shows.
- **The scope rule now exists once.** The client recomputed it from
  `/api/groups` + `/api/projects` — a second implementation of the resolver, in
  another language, agreeing with the Go one only by inspection. It now reads a
  resolved index from `/api/scopes` and looks the answer up.
- **The burndown and velocity respect scope.** Both counted the whole board
  regardless, so a scoped cycles tab showed a stats row disagreeing with the
  activity feed beside it. The burndown also validates its cycle against the
  scope now, like the git tab's drill-downs, instead of charting one the same
  scope reports as absent.

### Changed
- **The pixel language is enforced instead of restated.** Every font size now
  comes from a named ramp and every padding, gap and margin from a spacing
  scale, replacing 231 and 454 hand-written literals that had drifted across
  eleven and twenty-one distinct values. Most of it is invisible by design —
  the tokens hold the values the rules already had — but spacing now lands on
  a rhythm rather than wherever it happened to fall, and the depth tokens the
  design system already defined are finally the thing components use.
- **Meta tags read as fills rather than frames.** A card's id, points, cycle
  and repo labels sat in a framed card inside a framed column: three levels of
  the same ink, which is exactly what the design system's nested-border note
  warns against. They now take a soft fill and drop their border, losing a
  contrast level without losing their shape. The git tab's branch labels and
  the search hit roles get the same treatment.
- Adding and renaming a workflow **column**, and saving, renaming or updating a
  **view**, all use inline forms instead of `prompt()` dialogs — and a saved
  view can now be renamed and overwritten, not only created and deleted.
- `data.db` migrates to schema v12. The three original columns are seeded as
  rows rather than migrated, so every existing card, REST body and MCP call
  keeps working unchanged; boards with no custom columns look and behave
  exactly as before.

### Removed
- **Service views.** The Cloudflare Analytics and Search Console dashboards
  moved to their own private repo, `w4tch-y0ur-s3rv1c3s`. They watched third-
  party services, which is a different job from watching your own coding
  sessions, and they shared this binary only by accident of history. Gone with
  them: the `service` nav family and its `/service/*` routes, the
  `/api/cloudflare/*`, `/api/gsc/*` and `/api/webstats/sites` endpoints, the
  `cfanalytics` and `gsc` packages, and `webstats.json` handling. An existing
  `webstats.json` is simply ignored here now; move it to the new app's config
  dir. Nothing else changes — no data is touched.

## v1.0.0 — 2026-07-27

First public release.

### Added
- **Claude views** — a sessions list (filter by day and project; stat cards
  for sessions, tokens, cost and agent spawns; a live strip of running
  sessions), a per-session detail view (agent graph, timeline, click-node
  inspector, milestones, token/cost breakdown, lines changed), an insights
  view (activity heatmap, model distribution, churn, friction, sizing,
  ledger) and full-text search across transcript metadata.
- **Project views** — a kanban **board** whose cards link to the Claude
  sessions that worked them, an Excalidraw **design** library, a markdown
  **docs** wiki, a **ships** log of check/release runs, a read-only **code
  graph** over each repo's `.codegraph` index, and a read-only **git**
  dashboard (status per repo; per-repo commits, diffs, working tree,
  branches, pull requests and CI via `gh`).
- **Project scope** — a rail that scopes every view to one project or group,
  backed by a durable project registry that owns the Claude folders it
  stands for. The scope lives in the URL path, so any view is linkable.
- **Live updates** — a ~500 ms file watch by default, or instant push via
  Claude Code hooks (`--print-hooks`), plus optional OS notifications when a
  session needs input or finishes.
- **Share a drawing for review** — an opt-in `share` action PUTs one drawing's
  scene to a review backend you name via `COWORK_API` + `DESIGN_INGEST_SECRET`.
  No backend ships with the app: unset either variable and the button explains
  itself rather than sending anything.
- **MCP server** — create and update board cards, docs pages and drawings
  from inside a Claude Code session.
