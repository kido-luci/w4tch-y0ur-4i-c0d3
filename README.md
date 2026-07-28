# W4tch y0ur 4I c0d3

A local, privacy-first viewer for **Claude Code** sessions: per-session history
(tokens, cost, models, duration), a live **agent graph** and **timeline** of
every subagent a session spawned, and a click-node inspector for digging into
any of them. It also tracks activity over time with a heatmap, breaks down
token/cost by model, shows lines changed per session, and can notify you when
a session needs input or finishes — watch your AI code as it works.

Reads `~/.claude/projects/` transcript **metadata** only (never prompt text), no
database, no network beyond `127.0.0.1:4777` — with three **opt-in** exceptions,
each one off until you configure it:

- the milestone *summarize* button shells out to your own `claude` CLI (haiku,
  `--no-session-persistence`) to write one line per milestone group. Only the
  milestone labels (branch names, commit subjects, tag names) are sent, and the
  result is cached on disk so each session is summarized once.
- the **service** tab proxies Cloudflare Analytics and Google Search Console for
  sites you own, so the browser never sees a token. Credentials live in
  `webstats.json` in the config dir, never in this repo; with no file there the
  endpoints answer 503 and the view renders setup hints.
- the design library's *share* button PUTs that one drawing's `.excalidraw`
  scene to a review backend you name, so teammates can view and comment. There
  is **no built-in backend**: `COWORK_API` and `DESIGN_INGEST_SECRET` must both
  be set or the button explains itself instead of sending anything.

Nothing else ever leaves the machine, and nothing happens unless you click.
Design notes: `docs/spec.md`.

## Features

- **Sessions list** — filter by day and project; stat cards for sessions, total
  tokens, est. cost, and agent spawns; a live strip of running sessions; and a
  table with per-session duration, models, tokens, lines changed, and cost.
- **Activity heatmap** — a GitHub-style contribution calendar of the last 26
  weeks, colored by sessions per day.
- **Model distribution** — a stacked bar of tokens split by model family
  (opus / sonnet / haiku / fable) with each family's cost; global on the list,
  per-session on the detail view.
- **Milestones** — the session's semantic arc (plans, branches, commits, PRs,
  releases) mined from its own git commands and folded into branch-scoped,
  collapsible groups; a button summarizes each group in one line via your
  local `claude` CLI (on-demand, cached).
- **Agent graph** — the main session plus every subagent it spawned, drawn as
  a live node graph.
- **Session timeline** — a compact Gantt of the main thread plus each subagent
  on a shared time axis.
- **Click-node inspector** — click any graph node or timeline row to open a
  side drawer with that node's model, status, token breakdown, tools, cost,
  and duration.
- **Subagent cost cards** — cost-ranked cards, one per subagent.
- **Lines changed** — added/removed lines per session (main thread from each
  edit's structuredPatch; subagent edits approximated from their tool inputs).
- **Live + notifications** — instant updates via Claude Code hooks (see
  below), plus optional OS notifications when a session finishes or needs
  input.

## Build & run

```bash
make build            # builds the frontend, then the Go binary (embeds it)
./watch-your-ai-code  # serves http://127.0.0.1:4777
```

Open http://127.0.0.1:4777 in a browser (or Chrome → "Install as app" for a
standalone window).

## Run persistently (macOS launchd)

Edit `launchd/watch-your-ai-code.plist` — set the `ProgramArguments` path to
your built binary — then:

```bash
cp launchd/watch-your-ai-code.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/watch-your-ai-code.plist
```

Logs: `/tmp/watch-your-ai-code.log`. After a rebuild:
`launchctl kickstart -k gui/$(id -u)/watch-your-ai-code`.

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
