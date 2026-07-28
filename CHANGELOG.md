# Changelog

All notable changes to this project are documented here. Versions follow
[semantic versioning](https://semver.org/).

## Unreleased

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
    them in story points, and rank them 0–4. A **burndown** chart per cycle
    and a **velocity** table across cycles, both computed from a new
    append-only event log rather than stored totals.
  - **Table and timeline views**, plus a filter bar (text, kind, cycle,
    unestimated) you can save as a named view per scope. The table view drags
    rows to reorder or to nest one card under another.
  - **MCP**: `list_board_states` and `list_cycles`, and `create_todo` /
    `update_todo` take the new fields — a session can file a sized epic with
    children in one pass.

### Changed
- `data.db` migrates to schema v12. The three original columns are seeded as
  rows rather than migrated, so every existing card, REST body and MCP call
  keeps working unchanged; boards with no custom columns look and behave
  exactly as before.

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
- **Service views** — read-only Cloudflare Analytics and Google Search Console
  dashboards for sites you own, proxied server-side so the browser never sees a
  token. Credentials come from `webstats.json` in the config dir; with none
  configured the endpoints answer 503 and the view shows setup hints.
- **Share a drawing for review** — an opt-in `share` action PUTs one drawing's
  scene to a review backend you name via `COWORK_API` + `DESIGN_INGEST_SECRET`.
  No backend ships with the app: unset either variable and the button explains
  itself rather than sending anything.
- **MCP server** — create and update board cards, docs pages and drawings
  from inside a Claude Code session.
