# Changelog

All notable changes to this project are documented here. Versions follow
[semantic versioning](https://semver.org/).

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
