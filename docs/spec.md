# W4tch y0ur 4I c0d3 — Spec

**Status:** draft (2026-07-14)
**Supersedes:** `claude-session-analytics.md` (admin-route version — kept as the
future multi-machine option; the transcript-format findings there still apply).
This spec will move into the tool's own repo once it exists.

## Goal

A **local-only** web app on this Mac that reads `~/.claude/projects/` directly.
(One deliberate egress exists: the design library's explicit *share* action
publishes a single drawing to the owner's review backend — see the README's
network-exceptions note. Everything else stays on loopback.) It shows:

1. **Session history** — every Claude Code session: title, project, duration,
   models, tokens, est. cost, subagent count.
2. **Agent graph** — per session, the tree of subagents (like Claude Code
   desktop's live graph, but browsable after the fact): agent type, model,
   tokens, duration, tool stats per node.
3. **Live view** — sessions currently running update in near-real-time
   (file watch + SSE), including their agent graph as it grows.

No backend/VPS involvement, no server DB, no sync, no secrets. Data never
leaves the machine. (Since v0.43.0 there IS an embedded SQLite file — the
local index cache below — but the pillars it was guarding stand: no server
process, no network egress, nothing shared.)

## Architecture

**One Go binary** (standalone repo `watch-your-ai-code`):

```
┌─────────────────────────────────────────────┐
│ watch-your-ai-code (Go, listens 127.0.0.1:4777) │
│                                                │
│  scanner ──→ in-memory index ──→ JSON API      │
│     ▲                              + SSE       │
│  fsnotify (incremental re-parse)               │
│                                                │
│  //go:embed frontend/dist  (Vite vanilla TS)   │
└─────────────────────────────────────────────┘
```

- **Server**: Go stdlib `net/http` (+ chi only if routing outgrows stdlib).
  Binds **127.0.0.1 only** — never 0.0.0.0.
- **Scanner**: concurrent startup scan of `~/.claude/projects/**/*.jsonl`
  (currently ~1,870 files / 1.2 GB; substring pre-filter before JSON decode).
  Target cold start < 10 s; per-file results
  keyed by (path, mtime, size) so re-scans only touch changed files.
- **Index cache** (v0.43.0 — the "later optimization" arrived): embedded
  SQLite (`modernc.org/sqlite`, pure Go — CGO would break the local
  cross-compile release) at `<config-dir>/index.db`, WAL mode. Holds each
  session's parsed blob (gob, stamp-keyed) for warm boots, and an FTS5 table
  of message text for search. Disposable by design: the JSONL transcripts
  stay the source of truth, and any generation change (new binary, schema
  bump, different root) wipes and rebuilds — never migrates.
- **Watcher**: `fsnotify` on the projects tree; a changed session file is
  re-parsed and the diff pushed to subscribed clients over **SSE**. A session
  is "running" if its file changed in the last ~60 s.
- **Frontend**: Vite + vanilla TS + hand-rolled SVG (same stack family as the
  games; no framework, no chart lib). Built `dist/` embedded via `//go:embed`
  → the shipped artifact is literally one file.

## Data extraction (verified against real transcripts, 2026-07-14)

Metadata, plus — since v0.43.0 — the user/assistant text blocks of main
transcripts, which feed the local FTS5 search index. (Until v0.43.0 the rule
here was "message text is never read into the index"; it was retired
deliberately for diacritics-folding search — "loi" finds "lỗi". The text
lives only in `<config-dir>/index.db` on the same machine and never leaves
it.) Tool inputs and results stay out of the index: they carry whole files
and command output, and would bury conversation search under them.

**Per session** (from `<proj>/<sessionId>.jsonl`, skipping `subagents/`):
- Identity: `sessionId`, `slug`, latest `type:"custom-title"` line → title
  (fallback slug), `gitBranch`, project from `cwd` basename (collapse
  `.claude/worktrees/<name>` to parent repo).
- Time: first/last line `timestamp` → started/ended/duration; `compact_boundary`
  system lines → compaction count; `pr-link` lines → PR URL.
- Usage: per-line `message.model` + `message.usage`, deduped by
  `(message.id, requestId)` (fallback `uuid`), skip `<synthetic>`. Cost from
  a built-in PRICING table (ported to Go).

**Per agent run** (from `toolUseResult` rollup lines in the parent transcript —
NOT by re-parsing the 1,300+ `agent-*.jsonl` files):
- `agentId`, `agentType`, `resolvedModel`, `totalDurationMs`, `totalTokens`,
  full `usage` buckets, `toolStats` (reads/searches/bashes/edits, lines +/-),
  `status`.
- Label: the spawning `tool_use` block's `input.description`
  (via `sourceToolAssistantUUID`); spawn timestamp from that line.
- Tree: `parent_agent_id` = agentId of the file the spawn appears in (rollups
  found inside an `agent-*.jsonl` are nested spawns); NULL = main session.
  Subagent files ARE scanned, but only for these rollup/spawn lines.

## API (all local, no auth)

- `GET /api/sessions?days=N&project=X` — session summaries, newest first.
- `GET /api/sessions/{id}` — one session + its agent runs (flat list with
  `parentAgentId`; the client builds the tree).
- `GET /api/projects` — distinct projects for the filter dropdown.
- `GET /api/stats?days=N` — totals for the header cards (sessions, tokens,
  cost, agent spawns).
- `GET /api/events` — SSE: `session-updated` / `session-started` /
  `session-idle` events carrying the refreshed session summary (+ agent runs
  when the detail view is subscribed).

## UI (3 views, one page app)

Design read: *dev dashboard for one developer, terminal-adjacent, data-dense.*
Dark theme matching the screenshot vibe (near-black canvas, dotted grid, card
nodes); JetBrains Mono for numbers. Model color palette copied from admin
analytics: opus `#7c5cff` · sonnet `#2dd4bf` · haiku `#f59e0b` · fable
`#ec4899`, gray fallback.

1. **Sessions** (default): filter chips (Today/7d/30d/All) + project dropdown;
   4 stat cards; table — Title · Project · Started · Duration · Models
   (colored badges) · Msgs · Agents · Tokens · Cost. Live sessions get a
   pulsing "running" dot and float to the top. Row click → detail.
2. **Session detail**: header card (title, project, branch, PR link, duration,
   compactions, totals) + **agent graph** + agent-runs table (type, model,
   label, tokens, duration, tool stats).
   - **Graph**: hand-rolled SVG tree, left→right; root = main-session node
     (model split, total tokens, turns), one column per nesting depth; nodes
     as rounded cards (agent type, model badge, label, tokens, duration);
     elbow connectors; running agents pulse. Pan by scroll; no zoom in v1.
3. **Live strip** (top of Sessions view, only when something is running):
   compact cards of in-flight sessions updating via SSE.

## Install & run

- Build: `go build` (frontend `npm run build` first; `make` wraps both).
- Run persistent: launchd plist `watch-your-ai-code` (RunAtLoad +
  KeepAlive) pointing at the binary.
- Use: bookmark `http://localhost:4777` (or Chrome "Install as app" for a
  dock icon/own window).

## Non-goals (v1)

- Multi-machine aggregation / remote access (that's the shelved admin spec).
- Editing/deleting transcripts; the tool is strictly read-only over `~/.claude`.
- Cost reconciliation with the admin daily pipeline (both use the same PRICING
  table; small drift vs Anthropic billing is accepted there too).
- Packaging as a real desktop app (Tauri wrap is a possible later phase).
