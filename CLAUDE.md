# w4tch-y0ur-4i-c0d3 — notes for Claude

Single Go binary serving a Vite/TypeScript frontend on `127.0.0.1:4777` by
default (read-only viewer over `~/.claude/projects`). Build/run details:
`README.md`, `Makefile`. Design notes: `docs/spec.md`.

## Where the code lives, and which way the arrows point

Both halves are layered, and in both the rule is the same: **nothing imports a
layer above it.** If an import looks like it points upward, it is wrong.

**`frontend/` and `backend/` are siblings at the repo root.** Neither contains
the other. `backend/main.go` is pure wiring (flags, construction, the SPA
fallback) and the composition root — the only place that knows every package.

The one thing that crosses the boundary is the built bundle, not source:
`go:embed` cannot reach a parent directory, so Vite writes straight into
`backend/internal/web/dist` (`outDir` in `frontend/vite.config.ts`) and
`backend/main.go` embeds it from there. That is the whole reason the two halves
used to be nested. If you ever move either directory, that pair of paths is
what breaks, silently, at build time.

    main -> {httpapi, mcpserver} -> board -> index

- `index` — transcript scan/parse, the `index.db` cache, session types, the file
  watcher. Everything reads it; it reads nothing. Its schema covers `sessions`,
  the `messages` FTS table and `ships` together, under one generation stamp.
- `repos` — resolves a scope to on-disk repo roots, and owns `ResolveRoot`, the
  whitelist every git/GitHub drill-down validates `?repo` against.
- `git`, `github`, `codegraph` — the three read-only repo views, each with its
  own handlers. `search`, `ships` — query layers over `index.db`'s handle.
- `board` — the nine stores plus `data.db`'s schema and migrations, plus scope
  resolution. They share one database, so they share one package.
- `httpapi` / `mcpserver` — the two transports, siblings. Neither imports the
  other; `main` mounts both. `httpx`, `sse`, `cowork`, `summarize` are leaves.

Packages take **narrow interfaces, not the whole index** — `Snapshot()`,
`SessionRef(id)`, `Session(id)`. That is what lets git, ships and search live
outside `index` at all: a method must sit with its receiver's type, a function
needn't. `Index.Snapshot` hands out stored pointers, which is sound only because
a `*Session` in the map is never mutated in place (rescan replaces the entry).
Mutating a stored session would make every reader a data race.

**Frontend — `frontend/src/`, layered the same way:**

    api -> domain -> {scope, app} -> ui -> views -> main.ts

- `api/` — the typed client, one module per domain behind an `index.ts` barrel.
  `api/events.ts` holds the SSE transport and its module-level state; it must
  stay one module, or the Web Lock leader election breaks (see its comment).
- `domain/` — pure logic: format, filters, markdown, board queries.
- `scope/` — `location.ts` is the pure path grammar and **imports nothing**;
  `scope.ts` is the stateful half. `navigate` lives with the state, not the
  grammar, because its replace branch calls `syncScopeToURL`.
- `ui/` — reusable rendering. Note `sessionRail.ts` (the detail view's session
  switcher) and `scopeRail.ts` (the project rail) are different rails.
- `views/` — one module per route.

Two files are deliberately still large: `views/board.ts` (~1.5k lines) and
`style.css` (~6.2k). `renderBoardView` is one closure over 19 mutable variables
and that closure IS the per-mount state isolation, so splitting it means
redesigning state on the busiest screen, untested. Splitting `style.css`
reorders the cascade with no visual regression test. Both were left on purpose;
don't "fix" either in passing.

## Three jobs, three ways to run it — don't collapse them

Iterating, verifying, and delivering are separate. The trap is assuming the last
two are the same thing: **verifying what ships does NOT mean running it on your
everyday port.** That port is where you *deliver*.

**`make dev` — iterating.** Vite serves the frontend on `127.0.0.1:5173` with HMR
and proxies `/api` to a dev binary on `127.0.0.1:4778` that `air` rebuilds on
every `.go` save. A `.ts`/`.css` edit shows up on save; **`make build` is NOT
needed on this path** — it bypasses `go:embed` entirely. Needs `air` once:
`go install github.com/air-verse/air@latest`.

The dev binary reads the real `~/.claude/projects` (read-only, and an empty index
shows nothing), but keeps its **own** board + design library under `.dev/config`
via `-config-dir` (its own `data.db` + `index.db`). That isolation is the point:
your everyday instance is writing the real `data.db`, and two binaries sharing
one data store is how the old todos.json lost fields silently. Never point the
dev proxy at the everyday port.

**Stopping it: kill `air`, not the binary.** Ctrl+C on `make dev` stops
everything (the Makefile traps and kills the process group). `pkill -f wyac-dev`
does not: it kills the server, but air keeps watching, so the next `.go` save
silently brings the server back. Measured — the binary stays down until a save,
then returns under a new pid. From a script, kill `air` first:

    pkill -f 'air -- -addr'; pkill -f 'make dev'; pkill -f wyac-dev

The tell that you left one stack running and started a second is in air's output:

    listen tcp 127.0.0.1:4778: bind: address already in use
    Process Exit with Code: 1

That is the newer stack losing the race for 4778 — not a broken build. The older
one keeps serving, so requests still answer while your rebuilds go nowhere.

**`make build` + a throwaway port — verifying what ships.** `backend/main.go`
embeds the built frontend via `//go:embed all:internal/web/dist`, so on this
path a `frontend/src` change stays invisible until BOTH are rebuilt, in that
order:

    make build   # npm run build (writes into backend/) THEN go build

Rebuilding only the Go binary — or editing `frontend/src` without `npm run build` —
leaves stale assets embedded and served. `make dev` never exercises this path at
all, which is why a release is verified here and not there.

Run the built binary anywhere you like — nothing about verifying needs the
everyday port:

    ./watch-your-ai-code -addr 127.0.0.1:4779 -config-dir /tmp/wyac-verify

That is the same binary you're about to ship, with its own throwaway board, so
it can't touch the real one. It reads the real transcripts either way (read-only).
Need the real board to test against? That's a *data* question, not a port
question: copy `data.db` (with its `-wal`/`-shm` siblings) into the throwaway
config dir.

**Check the served bundle against the one on disk, not just against last time:**

    curl -s http://127.0.0.1:4779/ | grep -oE 'assets/index-[A-Za-z0-9_-]+\.js'
    grep -oE 'assets/index-[A-Za-z0-9_-]+\.js' backend/internal/web/dist/index.html

They must match. A binary can be **new and stale at once**: this happened once —
the binary answered on a brand-new endpoint (so the Go half was current) while
serving the *previous* release's assets, with the built bundle on disk timestamped
17s after the binary that supposedly embedded it. The cause was never found: a
port conflict was ruled out (a losing process logs its bind error and the winner
keeps serving — checked), and `make build` under a running `make dev` wouldn't
reproduce it. So this check stays, because it is what caught it. "New endpoint
answers" is not evidence the frontend came along.

**`make check` + `make release` — the gates.** `make check` runs npm ci +
vite/tsc build, then gofmt/vet/test/build inside `backend/`, and the served-vs-disk embed
check above. `make release VERSION=vX.Y.Z` = fail-fast guards (VERSION set, clean
tree, CHANGELOG entry) → full check → cross-compiles 4 platforms → tag push →
`gh release create`. That cross-compile makes **CGO a red line**: it's why the
SQLite index uses pure-Go `modernc.org/sqlite`, and a CGO dependency would take
the whole release flow down with it.

**Publishing has TWO paths, and the second one fires on its own.** Reading the
Makefile alone tells you `make release` publishes. `.github/workflows/release.yml`
also does, on `push: tags: ['v*']` — so **pushing any `v*` tag publishes a
release**, with no `make release` involved. It checks out the tagged commit,
builds the frontend and the same 4 platform binaries, then:

- release for that tag exists (i.e. `make release` just made it) → `gh release
  upload --clobber`. That is the normal path, and it means every release is
  built twice, once locally and once in CI. Harmless, and `make release` is now
  largely redundant with CI.
- it does not exist → `gh release create --generate-notes` with the tarballs.

The second branch is the trap, because a bare `git push origin <tag>` reaches it.
Two consequences, both of which have bitten:

- **The body is auto-generated** — a lone "Full Changelog" link, not the
  CHANGELOG entry. Trying to `gh release create` afterwards fails 422
  (`tag_name already exists`); use `gh release edit <tag> --notes-file` instead.
- **GitHub marks the newly published release "Latest"**, so tagging an OLD
  version demotes the real one on a public repo. Tagging `v1.0.0` retroactively
  did exactly that to `v2.0.1`. Fix with `gh release edit <current> --latest`,
  and check `gh api repos/<owner>/<repo>/releases/latest`.

So a tag is never "just a tag" here. If you want one without publishing, there
is no such thing while this trigger exists — say so rather than promising it.

**CI is NOT the same gate as `make check`.** `ci.yml` runs on `pull_request`
only (never on push to main) and does npm ci, `npm run build` (tsc + vite),
gofmt, go vet, go test, go build. It does **not** run the frontend vitest suite,
and it does **not** do the served-vs-disk embed check. `make check` does both, so
a green PR is weaker evidence than a green `make check` — keep running the local
one before you release.

**The everyday instance — delivering.** Under the optional launchd agent
(`launchd/com.luci.watch-your-ai-code.plist`, `KeepAlive=true`), restart it
AFTER `make build`:

    launchctl kickstart -k gui/$(id -u)/com.luci.watch-your-ai-code

That argument is the plist's `Label`, which is also its filename. It used to be
documented here as the bare `watch-your-ai-code`, which no longer matched the
installed agent — every `kickstart` answered `Could not find service ... in
domain for user gui: 501`, and the tempting next move is to `kill` the pid,
which is the one thing the paragraph below says not to do. If it ever fails
again, read the label back rather than guessing:

    launchctl list | grep watch-your-ai-code

Don't `kill` the PID and relaunch by hand — KeepAlive respawns the old binary within
seconds and grabs the port (`bind: address already in use`), and you keep serving
stale assets. Confirm the served bundle changed:

    curl -s http://127.0.0.1:4777/ | grep -oE 'assets/index-[A-Za-z0-9_-]+\.js'

The hash only moves when the built bundle changed — a Go-only fix legitimately
leaves it identical, so check the behaviour too, not just the hash.

## Routing — real paths, and the scope lives in one of the segments

Routes are real paths (History API), not `#/`: **`family/scope/tab[/detail]`** —
`/project/<scope>/git/<repo>`, `/claude/<scope>/session/<id>` (`/` canonicalises
to `/claude/<scope>/sessions`). `backend/main.go` holds the SPA fallback that lets a deep
path reload instead of 404. On the client the grammar and the state are separate
modules: `scope/location.ts` owns `parseLocation` / `buildPath` and imports
nothing, `scope/scope.ts` owns `navigate` / `setScope` / `syncScopeToURL`.
`scope/routing.test.ts` pins both rules below — it is mutation-checked, so if you
change routing and it stays green, suspect the change, not the tests.

The scope SEGMENT is the source of truth; localStorage is only the fallback that
remembers your last one and seeds a bare path. Internal links deliberately stay
scope-LESS (`href="/project/git"`) and `syncScopeToURL` splices the active scope
in during `render()` — which is why adding a link never means threading the scope
through it, and why a link's `href` and the address bar legitimately differ.

Two rules that exist because breaking them shipped bugs:

- **A scope change must DROP the detail segment.** Carry it over and you land on a
  detail belonging to the scope you just left: a repo that isn't in that scope —
  a dead "not in this scope" screen you can't reach by typing, only by switching
  project while viewing one. It bit docs pages, drawings and board cards too, not
  just git. Only a *replace* keeps the detail, because that path is the boot
  default or a rename of the scope you're already in.
- **Every navigate path must canonicalise, not only the ones that re-render.**
  `navigate(_, replace)` fires no popstate on purpose (it would double-render), and
  that also means `render()` — and with it `syncScopeToURL` — never runs. A
  scope-less replace then leaves the URL without its scope until some unrelated
  render happens to fix it; the docs view auto-opening its first page did exactly
  that. A link copied in that window resolves against the READER's remembered
  scope, not yours. So the replace branch canonicalises itself.

## The git tab — read-only, and it shares the code graph's repo resolution

Two views over the scope's repos. Both are strictly **READ-ONLY**: the server
shells `git log` / `status` / `branch` / `show` / `for-each-ref`, and `gh` for
GitHub. Nothing here commits, pushes, merges, or checks anything out — don't add
that without asking; it's a deliberate property, not an oversight.

- **Overview** `/project/<scope>/git` — one compact status ROW per repo: name ·
  branch · clean/dirty · ahead/behind · its latest commit. It's a dashboard you
  scan. If you catch yourself adding a long list inside a row, it belongs in the
  detail instead — an earlier version shipped 20 commits per card and had to undo
  it, because six repos became six walls of log.
- **Detail** `/project/<scope>/git/<folder>` — tabs, each lazy-loaded on first
  open: commits (click one → its diff), changes (working tree), branches, pull
  requests, issues & CI. (These are real paths, not `#/` — see the History-API
  routing and the Go SPA fallback in `backend/main.go`; a deep path that 404s means that
  fallback broke.)

**Repo resolution is shared with the code graph** — both go through
`internal/repos`, so both tabs always list the same repos, resolved through each
folder's most recent session cwd. Every drill-down endpoint
(`/api/git/{commit,diff,branches,commits,prs,activity}`) validates `?repo`
against that resolved set; an arbitrary path is a 404. That guard is *why* these
endpoints are safe to expose — keep it on anything new you add here.

**Filters** ride all four lists in the shared `.filter-chip` idiom: overview
(dirty/clean/ahead/behind) · branches (hide merged/local/remote/stale >90d) · pull
requests (by state) · commits (hide merges/author/search). No chip on = nothing
filtered, chips within a group are OR'd, and every filtered list prints how many
rows it hid — a short list must never read as an empty repo.

Two things about them that look like needless complexity but aren't:

- **The commits filters run on the SERVER** (`git log --no-merges` / `--author=` /
  `--grep=`), unlike the other three, which filter in the browser. That list is
  **paged**, so filtering only the loaded rows would report "no results" whenever
  the match simply sits further back. With no filter active the tab still shows the
  snapshot's instant 20 and makes no request at all. Search passes
  `--fixed-strings`, so `feat(web):` is a literal and not a regex, and each value
  travels as ONE `--flag=value` token so a term starting with `-` stays a search
  term instead of becoming another flag.
- **The commits filter chrome renders ONCE**; only its list re-renders underneath.
  Rebuild the whole tab body per keystroke and the search box loses focus mid-word.

Traps, each of which cost a debugging cycle:

- **`runGit`, not `gitCmd`** — `gitCmd` is already a regex const in
  `internal/index/parse.go`. `runGit` is unexported; `internal/github` gets the
  one thing it needs via `git.RemoteURL`, deliberately narrow so "read-only"
  stays a property of the surface rather than a convention.
- **`gh` is found by ABSOLUTE path** (`internal/github`): launchd's PATH has no
  `/opt/homebrew/bin`, so a bare `exec.Command("gh", …)` works under `make dev`
  and silently returns nothing under launchd. Auth itself is fine headless.
- **Merge commits need `--first-parent`** or `git show` hands back an empty diff
  while the file list is non-empty.
- `refs/remotes/origin/HEAD` shortens to `origin`, so skip it by the FULL refname
  — matching on the short name lets a phantom `origin` branch through.
- Commits **page**: the snapshot carries 20, `/api/git/commits?skip&limit` fetches
  more (one page capped at 100).
- **Only GitHub is supported** for the remote sections (`gh`). A repo on any other
  host shows an empty state *by design*.
