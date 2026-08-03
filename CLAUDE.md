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
- `repos` — resolves a scope to on-disk repo roots, two ways for two questions.
  `Bound` reads the project registry's repo bindings (what a project declares it
  IS) and owns `BoundRoot`, the whitelist every git/GitHub drill-down validates
  `?repo` against. `Repos` reads the session index instead — where work actually
  happened — which is what the manager's binding picker and the registry's
  opening offer need, and nothing else should.
- `git`, `github`, `codegraph` — the three read-only repo views, each with its
  own handlers. `search`, `ships` — query layers over `index.db`'s handle.
- `figfiles` — the `.fig`/`.pen` documents under each repo's `design/`, and the
  **one place the server launches something on your machine** (`open -a
  OpenPencil`). Everything else here only reads, so its whitelist is load-
  bearing rather than decorative: `Open` re-lists the scope and refuses any path
  that is not in the result, so `open` only ever sees a path the scope itself
  produced. It validates *before* checking the platform, which is what lets that
  test run on CI's Linux runner and not only on a Mac. Darwin-only by nature —
  the release cross-compiles four platforms, so every other one answers 501.
- `board` — the nine stores plus `data.db`'s schema and migrations, plus scope
  resolution. They share one database, so they share one package.
- `httpapi` / `mcpserver` — the two transports, siblings. Neither imports the
  other; `main` mounts both. Routing is **go-chi**: `main` builds one
  `chi.NewRouter()` and every package that owns a slice of the API takes a
  `chi.Router` — `httpapi.Register`, and through it `codegraph`, `figfiles`,
  `git`, `github`. Path params are `chi.URLParam(r, "name")`, not `r.PathValue`.

  One thing chi does that `http.ServeMux` did not, and it bit on the way in: a
  known path with an unregistered method is answered by chi's OWN 405 handler,
  which never reaches the SPA fallback. Under ServeMux the bare `"/"` pattern
  caught that case and returned the JSON error. `GET /api/projects/<name>` is
  exactly it. So `main` wires the fallback to **`MethodNotAllowed` as well as
  `NotFound`** — the invariant being protected is that `/api/*` answers JSON,
  never a web page and never a bare status.
- `httpx`, `sse`, `cowork`, `summarize` — leaves.

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

**A render function cannot find its own wrapper in the document.** The views
build a detached element and the caller appends it *after* the function
returns, so `document.querySelector` inside a render path matches nothing and
fails silently. `ui/milestones.ts` hid this for a long time: it fetched on
every render, and the fetch's `.then()` ran after the append and painted
everything. Gating that fetch exposed it — repaints stopped painting and the
summarize button stayed hidden. Hence `paintSummaries(wrapper, id)` (takes the
element, cannot no-op) versus `repaintSummaries(id)` (looks it up, for async
callbacks where doing nothing is correct). Pass the element on a render path.

**Fetching from inside a render is a leak, not a refresh.** The detail view
re-renders on every `session-updated` SSE event, and on a running session those
never stop — one summaries fetch per repaint, measured at 13 requests in 91
seconds and still climbing. Key a fetch to the data it depends on, not to the
repaint: `ui/summaryGate.ts` mirrors the server's freshness hash so the request
fires when the milestones change and not otherwise.

`httpapi` is split by resource — `drawings.go`, `docs.go`, `analytics.go` beside
`api.go` — and the extracted functions take parameters **named for the locals
they replaced**, which is what let the handler bodies move verbatim. Keep that
when you split the next group: a rename sweep across sixty bodies is where this
goes wrong, and the compiler cannot see a body that captured the wrong
same-typed variable. The projects/claude/repos/presentation routes are still
interleaved in `api.go` and were left for that reason, not overlooked.

**Views are testable now** — vitest runs under jsdom. It ran in a node
environment until `code` replaced the `git` tab and left `href="/project/git"`
behind in the repo detail: tsc cannot see a string in a template literal, no
test rendered that view, and the suite stayed green. `app/links.test.ts` now
asserts every internal link in the source names a tab that still exists, by
running it through `parseLocation` rather than a second copy of the tab sets.

`views/board.ts` (~1.5k lines) is deliberately still large: `renderBoardView` is
one closure over 19 mutable variables and that closure IS the per-mount state
isolation, so splitting it means redesigning state on the busiest screen,
untested. Left on purpose; don't "fix" it in passing.

`style.css` was in that same paragraph until it wasn't. It is now `src/style.css`
— an index of `@import`s — plus fifteen numbered parts under `src/css/`. The
objection to splitting it was never taste, it was that reordering the cascade
breaks things nothing here renders a page to notice. What made the split safe was
proving it inert rather than promising it: the parts are imported in the exact
order they appeared, Vite inlines them in that order, and the built CSS came out
byte-identical — same content hash, same 115392 bytes. `css/order.test.ts` keeps
it that way. **The import order IS the cascade**; reordering those lines is a
silent restyle, and adding a rule to `style.css` itself puts it after every
import where it beats all of them.

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

**A throwaway port and config dir are two axes; the OUTPUT PATH is the third,
and `make` doesn't give it to you.** `make build` and `make check` both end in
`go build -o ../watch-your-ai-code` — the exact file the launchd agent runs. So
running either one IS a delivery to the everyday instance: it puts whichever
branch is checked out on 4777, with no merge, tag or release anywhere in sight.
The restart below is not a way to avoid that, it's the *other* consequence of
the same overwrite. Never reach for `make build`/`make check` to try a branch
out; shipping to the everyday instance is a decision to ask for, not a step
inside a verification.

Build somewhere else instead — the port and the config dir were never the part
at risk:

    npm --prefix frontend run build                  # writes backend/…/web/dist
    go build -C backend -o /tmp/wyac-verify/wyac .   # -o absolute: -C moved us
    /tmp/wyac-verify/wyac -addr 127.0.0.1:4779 -config-dir /tmp/wyac-verify/cfg

`npm run build` alone is harmless — `dist` is read at go-build time, so nothing
already running notices. It is the `-o` that decides whose binary you replaced.
This is not hypothetical: verifying an unmerged branch on 4779 began with a
`make build`, which left the everyday instance running that branch and forced a
`kickstart` to keep it answering at all.

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

**`main` is protected.** A PR is required, the `frontend + backend` check must
pass, the branch must be up to date (strict), and force-push and deletion are
blocked. Approvals are set to zero on purpose — this is a solo repo and
requiring a review would deadlock it. Admins can bypass, so protection is a
guard rail, not a wall. Strict is not ceremony: merging a fix for a flaky test
immediately put an open PR into `BEHIND`, which forced it to re-run CI on the
fixed base instead of merging on a stale green.

**`make release-dry` — the release path, minus publishing.** Guards, full
check, all four cross-compiles, then the one thing a real release never does:
it unpacks the tarball built for THIS host and runs it, checking the served
bundle matches the one on disk, that the API and a deep SPA path answer, and
that an unrouted `/api` path still 404s. Compiling is not evidence the
artifact works. It takes no argument — VERSION defaults to the newest
CHANGELOG entry — and tags nothing, pushes nothing, publishes nothing.

Use it before every release. The cross-compile is shared with `make release`
(`release-build`) rather than copied, so the two cannot drift.

Two build-file traps, both of which produced a broken file that still *looked*
right:

- **`#` starts a comment in a Makefile even inside quotes.** `grep '^## '` in a
  variable assignment gets truncated mid-string and make reports something
  unrelated ("unterminated call to function `shell'"). Escape it: `'^\#\# '`.
- **Environment assignments cannot prefix a subshell.** `GOOS=... (cd backend
  && go build ...)` is a shell syntax error, not a slower way to do the same
  thing. Use `go build -C backend` instead, and `bash -n` a recipe before
  trusting it — nothing else here would have caught it before a release. This exists
because the release path is the one path nothing else covers: CI runs on
pull_request and never cross-compiles, and `make check` stops at a native
build. A `GOOS=... (cd backend && ...)` line — an outright shell syntax error
— lived in the cross-compile and would have surfaced only mid-release.

There is no safe way to rehearse the real thing instead: `make release` ends
in `git push origin <tag>`, `release.yml` fires on `v*`, and GitHub hands
"Latest" to whatever published last — so a throwaway tag publishes a release
AND demotes the real one on a public repo.

**CI is NOT the same gate as `make check`.** `ci.yml` runs on `pull_request`
only (never on push to main) and does npm ci, `npm run build` (tsc + vite), the
vitest suite, gofmt, go vet, go test, go build — all Go steps inside `backend/`.
It does **not** do the served-vs-disk embed check, and it never cross-compiles,
so it cannot see the release path at all. `make check` covers the embed gate and
`make release-dry` covers the rest; a green PR is still weaker evidence than
both. The vitest step was added late: every PR before it merged green without a
single frontend test having run.

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

**Skipping that restart doesn't leave you on stale assets — it leaves you with a
server that accepts connections and never answers.** `make check` and `make
build` write `go build -o ../watch-your-ai-code`, which is the exact path the
agent is running, so a build replaces the live binary underneath an 18-hour-old
process. Its accept loop survives on code already paged in; serving a request
needs pages that no longer match the file, and the request hangs forever. What
that looks like:

    launchctl list | grep watch-your-ai-code   # running, with a pid
    lsof -nP -iTCP -sTCP:LISTEN -p <pid>       # LISTEN on 127.0.0.1:4777
    curl -sv -m 5 http://127.0.0.1:4777/       # "Connected" … then nothing
    ps -o stat,pcpu -p <pid>                   # S, 0.0%

Every signal says the server is up, which is what makes it expensive: the
tempting reading is "the process died" and the tempting fix is to `kill` it —
the one move the paragraph above rules out. It is not down, it is a build
artifact. `kickstart -k` is the whole fix. So the restart above is not hygiene
for the frontend's sake; it is what stops a `make check` from taking your
everyday instance down until you notice.

## Tests — two things that pass locally and fail on CI

**A send on a shared unbuffered channel goes to whichever goroutine parked
first, and parking order is not start order.** `TestRescanCoalescer` had two
callbacks waiting on one `release` channel and assumed the send would reach the
first one started. It always did on this machine — 300 runs pinned to one CPU
stayed green — and on a contended runner the other one won, finished, exited,
and left the first blocked forever: a ten-minute timeout with no output. Give
each concurrent actor its own channel so there is one waiter per channel and no
ambiguity to lose. This had shipped green through three PRs before it fired.

**Local green is not evidence for a scheduling race.** When a test hangs on CI
and passes here, reproduce it by forcing the interleaving by hand (a `sleep`
that makes the other goroutine park first) rather than re-running and hoping.
That turns "flaky" into a fact in about a minute.

## Routing — real paths, and the scope lives in one of the segments

Routes are real paths (History API), not `#/`: **`family/scope/tab[/detail]`** —
`/project/<scope>/code/<repo>`, `/claude/<scope>/session/<id>` (`/` canonicalises
to `/claude/<scope>/sessions`). `backend/main.go` holds the SPA fallback that lets a deep
path reload instead of 404. On the client the grammar and the state are separate
modules: `scope/location.ts` owns `parseLocation` / `buildPath` and imports
nothing, `scope/scope.ts` owns `navigate` / `setScope` / `syncScopeToURL`.
`scope/routing.test.ts` pins both rules below — it is mutation-checked, so if you
change routing and it stays green, suspect the change, not the tests.

The scope SEGMENT is the source of truth; localStorage is only the fallback that
remembers your last one and seeds a bare path. Internal links deliberately stay
scope-LESS (`href="/project/code"`) and `syncScopeToURL` splices the active scope
in during `render()` — which is why adding a link never means threading the scope
through it, and why a link's `href` and the address bar legitimately differ.

**The two families share the grammar and nothing else.** The segment sits in the
same place, but on `/project` it names a registry project (or group) and on
`/claude` it names a repo the sessions ran in — two taxonomies, so two remembered
scopes (`wyac-scope`, `wyac-scope-claude`). `setScope` therefore takes the family
it applies to, and rewrites the URL only when you are standing in that family:
the project rail mounts on EVERY route and applies its default at boot, so
without that check it spliced a project name into a claude path, where it names
nothing and the session list came back empty. For the same reason the rail reads
`getProjectScope()`, never `getScope()`.

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

## The code tab — read-only, and the graph is one of its tabs

Two views over the scope's repos. Both are strictly **READ-ONLY**: the server
shells `git log` / `status` / `branch` / `show` / `for-each-ref`, and `gh` for
GitHub. Nothing here commits, pushes, merges, or checks anything out — don't add
that without asking; it's a deliberate property, not an oversight.

- **Overview** `/project/<scope>/code` — one compact status ROW per repo: name ·
  branch · clean/dirty · ahead/behind · its latest commit. It's a dashboard you
  scan. If you catch yourself adding a long list inside a row, it belongs in the
  detail instead — an earlier version shipped 20 commits per card and had to undo
  it, because six repos became six walls of log.
- **Detail** `/project/<scope>/code/<folder>` — tabs, each lazy-loaded on first
  open: commits (click one → its diff), changes (working tree), branches, pull
  requests, issues & CI, graph. (These are real paths, not `#/` — see the History-API
  routing and the Go SPA fallback in `backend/main.go`; a deep path that 404s means that
  fallback broke.)

**Repo resolution is shared with the code graph** — the git handlers and
`internal/codegraph` both go through `internal/repos`, so they can never disagree
about which repos a scope has. That shared answer is what made merging them into
one tab honest rather than cosmetic: the graph reads the same root the git
sections do, taken from the `<folder>` segment, so this page no longer has a
second repo picker of its own to drift from the URL. They resolve a scope
LABEL (`?scope=`, the same label the board endpoints take) to the repos its
projects are BOUND to in the registry — not to the directories its sessions ran
in, which is what these tabs used to do and what made the project page's answer
depend on where Claude had wandered. A project with no binding lists no repos,
on purpose: the fix is to bind it in the manager, not to guess for it.

**A project may be bound to SEVERAL checkouts of one repo** — a second clone on
another branch, a copy made to try something. They live in `project_repos`
(schema v15); `projects.repo_root` survives as the FIRST of them, rewritten
whenever the list changes, so everything that needs only "a" root — slug
derivation, figfiles, the graph — keeps reading the column. That projection is
sound only because the API refuses roots whose remotes differ: one project is one
repo, and its slug, GitHub sections and visibility are single values.

Two consequences worth knowing before you debug them. A git **worktree is not a
second checkout**: `CanonicalRoot` resolves it to the repo it belongs to, so
listing one beside its parent collapses to a single row. And the route segment is
`checkoutKey` (the last two path segments), not the Claude folder — every clone
of a repo shares that folder name, so a link keyed on it would resolve to
whichever row happened to come first. The list and the detail both compute the
key from `root` with the same function, which is what stops them disagreeing.

Every drill-down endpoint (`/api/git/{commit,diff,branches,commits,prs,activity}`)
validates `?repo` against that bound set (`BoundRoot`); an arbitrary path is a
404. That check needed no change for multiple checkouts — it always matched a
full path against a list, so more roots simply make the list longer. That guard is *why* these endpoints are safe to expose — keep it on anything
new you add here. It is also why binding accepts only a real checkout
(`git.IsRepo`): the binding IS the whitelist now, so the gate sits where the path
enters, in `PUT /api/projects/{name}`.

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

## A project's NAME is its identity in three stores that don't know each other

The registry row is only one of them. The same string is also the owner key on
every board card in `data.db`, the `project` column of the ships table in
`index.db`, and an entry in a group's member list. Nothing enforces agreement
between them, and a name that matches nothing does not error — it renders.

`POST /api/projects/{name}/rename` covers most of it: the row, its children's
`parent`, todos, board states, cycles, docs and drawings groups, views, and
group membership. What it cannot reach is **ships**, because those do not live
in the database it is renaming. Each record is a JSON file in
`~/.wyac/ships` whose `project` field the Makefile wrote at ship time, and
`Store.Scan` only reads that file back. Rename a project and its whole ship
history stays behind under the old name, invisible under every scope — it
survives only in the all-projects view, which is exactly where nobody looks.
Measured twice in one session: 99 records, then 83.

**Editing those files in place does nothing.** `Scan` keys the table by
FILENAME and skips any name it already holds, so a changed `project` field is
never re-read. Renaming the file is what makes it re-ingest, and the row for the
vanished old filename is deleted in the same pass — no `index.db` rebuild, no
downtime, the watcher picks it up in seconds.

The filename is `<epoch>-<pid>-<project>-<kind>.json`, and **substring
replacement on it is wrong** whenever one project name contains another:
`luci_web_blog-backend` holds `luci_web_blog` whole, so a naive replace turns 20
backend records into `luci-studio-workspace-backend`. Match the exact suffix
`-<project>-<kind>.json`, built from the `project` and `kind` you read out of
that same file. Back the files up first; they are the only copy.

The group list has the same shape of bug from the other side: it stores project
NAMES, while the thing people reach for is the FOLDER name — a different
namespace. `luci-studio` carried `luci_web_blog-frontend` for a long time, which
was never a project. The rail drew it as a plain text row with no icon, no link
and no warning. That is what a dangling member looks like, and it is the only
symptom you get.
