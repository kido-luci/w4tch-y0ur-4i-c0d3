# W4tch y0ur 4I c0d3

A local, privacy-first dashboard for solo development with **Claude Code**. It
reads `~/.claude/projects/` to show what your sessions actually did — tokens,
cost, models, the agent graph, the milestones — and then puts the rest of the
work next to them: a board whose cards link to the sessions that moved them, a
drawing library, a docs wiki, a read-only git dashboard, and a ship log. One Go
binary, one page, everything scoped to the project you're looking at.

Everything runs on `127.0.0.1:4777` against local files. Session text is indexed
locally into `index.db` in the config dir so search can rank and fold diacritics
("loi" finds "lỗi"); tool inputs and results are deliberately kept out of that
index. **Nothing is sent anywhere** — with three **opt-in** exceptions, each one
off until you configure it:

- the milestone *summarize* button shells out to your own `claude` CLI (haiku,
  `--no-session-persistence`) to write one line per milestone group. Only the
  milestone labels (branch names, commit subjects, tag names) are sent, and the
  result is cached on disk so each session is summarized once.
- the design library's *share* button PUTs that one drawing's `.excalidraw`
  scene to a review backend you name, so teammates can view and comment. There
  is **no built-in backend**: `COWORK_API` and `DESIGN_INGEST_SECRET` must both
  be set or the button explains itself instead of sending anything.

All three are click-triggered: nothing goes out in the background, ever.
Design notes: `docs/spec.md`.

## Layout

Routes are real paths shaped `family/scope/tab`. Three families across the top,
one **project scope** rail down the side — pick a project (or a group, or a
parent with its whole subtree) and every view follows it. The scope lives in the
URL, so any view you are looking at is a link you can paste.

### claude — what your sessions did

- **Sessions** — filter by day and status; stat cards for sessions, tokens,
  est. cost and agent spawns; a live strip of running sessions; a table with
  per-session duration, models, tokens, lines changed and cost.
- **Session detail** — the **agent graph** (main thread plus every subagent it
  spawned), a **timeline** Gantt on a shared time axis, and a click-node
  **inspector** for any node's model, status, tokens, tools, cost and duration.
  Plus cost-ranked subagent cards and lines added/removed.
- **Milestones** — the session's semantic arc (plans, branches, commits, PRs,
  releases) mined from its own git commands and folded into branch-scoped
  groups; a button summarizes each group in one line via your local `claude`
  CLI (on-demand, cached).
- **Insights** — an activity heatmap, tokens over time by model family, and
  four cards that ask harder questions: **rework radar**, **friction**, **work
  sizing**, **cost per outcome**.
- **Search** — full-text over what you said and what Claude said back, ranked,
  diacritics folded.

### project — everything else about the work

- **Board** — backlog / doing / done cards, each linkable to the Claude
  sessions that worked it, so a card carries its own history.
- **Design** — an embedded Excalidraw library with groups, topic tags and
  cached thumbnails.
- **Docs** — a markdown wiki with an index rail and a per-page TOC.
- **Ships** — every `make check` and `make release` run recorded with project,
  version, exit code, duration and log tail.
- **Code graph** — a read-only view over each repo's `.codegraph` index:
  folder overview, drill-down, and an ELK-routed graph with subsystem colours.
- **Git** — strictly read-only. An overview row per repo (branch, clean/dirty,
  ahead/behind, latest commit), and a detail view with commits → diffs, working
  tree, branches, pull requests and CI. GitHub remotes are read via `gh`.

### everywhere

- **Live + notifications** — instant updates via Claude Code hooks (below),
  plus optional OS notifications when a session finishes or needs input.
- **MCP server** — create and update board cards, cycles, docs pages and
  drawings from inside a Claude Code session, so the agent files its own work.

## Build & run

```bash
make build            # builds the frontend, then the Go binary (embeds it)
./watch-your-ai-code  # serves http://127.0.0.1:4777
```

Open http://127.0.0.1:4777 in a browser (or Chrome → "Install as app" for a
standalone window).

## Run persistently (macOS launchd)

Edit `launchd/com.luci.watch-your-ai-code.plist` — set the `ProgramArguments`
path to your built binary — then:

```bash
cp launchd/com.luci.watch-your-ai-code.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.luci.watch-your-ai-code.plist
```

Logs: `/tmp/watch-your-ai-code.log`. After a rebuild:
`launchctl kickstart -k gui/$(id -u)/com.luci.watch-your-ai-code`.

The label, the filename and the `kickstart` argument are all the same string —
that is a launchd convention, not a coincidence. Rename it and all three have
to move together, or `kickstart` answers `Could not find service`.

## Live updates (optional)

By default the viewer refreshes from a ~500 ms file-watch — always works, no
setup. Installing Claude Code hooks makes updates **instant** (push instead
of poll); it's optional, the viewer works fine without it.

```bash
./watch-your-ai-code --print-hooks
```

Prints a `hooks` JSON block to stdout (guidance goes to stderr, so redirect
just the JSON with `> hooks.json`); the URL follows `--addr`, so pass the
same `--addr` you run the server with if you changed it. Merge the block into
`~/.claude/settings.json` yourself (into an existing `"hooks"` block if you
have one) — the tool only prints, it never writes to your config. It wires
`PreToolUse`, `PostToolUse`, `SubagentStop`, `Stop`, and `Notification` as
fire-and-forget `curl` calls to `/api/hook` that always exit 0, adding no
tokens and never stalling or breaking a session even when the viewer isn't
running.

## Notifications (optional)

Click the 🔔 bell in the topbar to enable OS notifications — your browser
asks for permission once. `127.0.0.1` is a secure context, so this works over
plain http.

You get a notification when a session **needs input** (from the
`Notification` hook) or **finishes** (from the `Stop` hook, after ~60 s of
quiet so an active back-and-forth doesn't spam you). Clicking a notification
opens that session.

It's 100% client-side — the browser does the notifying, so notifications add
no server-side process or network use. Requires the viewer tab to
stay open, and the hooks from "Live updates" above installed (the
`Notification` event powers the needs-input signal).

## Notes

- A session/agent counts as **running** when its transcript changed in the last 60 s.
- Token totals include cache read/write; costs use a built-in pricing table, so a
  small drift vs. Anthropic's billing is expected.
- Background agent spawns are logged as `async_launched` and never updated in the
  transcript; the server normalizes them to running/completed by file freshness.
- The Claude Code transcript format is undocumented and may change; this reads it
  best-effort.
- Lines changed is best-effort too: a `Write` that overwrites an existing file
  counts its whole new content as added.
