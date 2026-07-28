# Changelog

All notable changes to this project are documented here. Versions follow
[semantic versioning](https://semver.org/).

## Unreleased

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
