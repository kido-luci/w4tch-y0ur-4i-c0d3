// View 9b — code repo detail (route `/project/code/<folder>`): the deep read-only look at
// one repo the scope resolved to. Reached from a row on `/project/code`. Sections load
// lazily on first open and cache for the visit; the local ones (commits →
// per-commit diff, working-tree changes, branches) are instant, the GitHub ones
// (pull requests, issues & CI) go over `gh` so they show a loading state. wyac
// only reads — nothing here commits, pushes, merges, or checks anything out.
//
// The `graph` tab is the exception to the caching above: every other tab is an
// HTML string this view builds and stashes in bodyCache, while the graph is a
// whole view module with its own cytoscape instance and cleanup. It is mounted
// fresh on open and disposed on the way out, so it never leaks a canvas behind a
// tab you have left.

import {
  getGit,
  getGitActivity,
  getGitBranches,
  getGitCommit,
  getGitCommits,
  getGitDiff,
  getGitPRs,
} from "../api";
import type { GitBranch, GitCommit, GitCommitDetail, GitFileChange, GitPR, GitRepo } from "../api";
import { checkoutKey, chipAttrs, escapeHtml, formatRelativeTime } from "../domain/format";
import { getScope } from "../scope";
import { renderCodegraphView } from "./codegraph";

type TabKey = "commits" | "changes" | "branches" | "prs" | "activity" | "graph";
const TABS: { key: TabKey; label: string }[] = [
  { key: "commits", label: "commits" },
  { key: "changes", label: "changes" },
  { key: "branches", label: "branches" },
  { key: "prs", label: "pull requests" },
  { key: "activity", label: "issues & CI" },
  { key: "graph", label: "graph" },
];

// The snapshot payload carries this many commits, so a shorter first batch means
// the whole history is already on screen. Must match gitLogLimit in git.go.
const FIRST_BATCH = 20;
// How many more each "load more" pulls (the server clamps at 100).
const PAGE = 30;

// --- shared render helpers ----------------------------------------------------

function fileList(files: GitFileChange[] | null): string {
  const fs = files ?? [];
  if (!fs.length) return "";
  return `<ul class="git-files">${fs
    .map((f) => {
      const stat =
        f.add < 0 || f.del < 0
          ? `<span class="git-fstat git-fstat--bin">bin</span>`
          : `<span class="git-fstat"><span class="git-add">+${f.add}</span> <span class="git-del">−${f.del}</span></span>`;
      return `<li class="git-file"><span class="git-fpath">${escapeHtml(f.path)}</span>${stat}</li>`;
    })
    .join("")}</ul>`;
}

function diffBlock(diff: string, truncated: boolean): string {
  if (!diff) return `<div class="git-commits-empty">no content changes (e.g. merge/binary).</div>`;
  const body = diff
    .split("\n")
    .map((ln) => {
      let cls = "git-dl";
      if (ln.startsWith("+++") || ln.startsWith("---") || ln.startsWith("diff ") || ln.startsWith("index "))
        cls = "git-dl git-dl--meta";
      else if (ln.startsWith("@@")) cls = "git-dl git-dl--hunk";
      else if (ln.startsWith("+")) cls = "git-dl git-dl--add";
      else if (ln.startsWith("-")) cls = "git-dl git-dl--del";
      return `<span class="${cls}">${escapeHtml(ln) || " "}</span>`;
    })
    .join("");
  return `<pre class="git-diff">${body}</pre>${
    truncated ? `<div class="git-diff-trunc">… diff too large, truncated.</div>` : ""
  }`;
}

/** Renders the code repo-detail view; returns a cleanup callback. */
export function renderCodeRepoView(container: HTMLElement, folder: string): () => void {
  // The scope LABEL, not its Claude folders: the git and code-graph endpoints
  // resolve a scope through the project registry's repo bindings now.
  const scope = getScope();
  let dead = false;
  let repo: GitRepo | null = null;
  let active: TabKey = "commits";
  // Each network tab's rendered body, cached for the visit so re-opening a tab
  // doesn't re-fire its gh/git request and flash "loading…" every time (which
  // the "cache for the visit" note above promised but a never-read Set didn't
  // deliver). Failures aren't cached — they retry — and the branch/PR filter
  // chips refresh their entry when they re-render, so a restore stays in sync.
  const bodyCache = new Map<TabKey, string>();
  // The graph tab's teardown, held while it is mounted. It is a view module, not
  // an HTML string, so bodyCache cannot hold it and dropping the element is not
  // enough to stop it.
  let graphCleanup: (() => void) | null = null;
  const commitCache = new Map<string, GitCommitDetail>();
  let expanded: string | null = null; // open commit hash in the commits tab
  // The commits tab's own paged list: seeded from the snapshot, grown by "load
  // more". exhausted flips when a page comes back short — that's the end of history.
  let commits: GitCommit[] = [];
  let exhausted = false;
  let loadingMore = false;
  let commitsFailed = false; // a filtered-commit reload errored (vs. genuinely empty)

  // --- filters -----------------------------------------------------------------
  // Branches and PRs filter in the browser (their lists arrive whole). Commits
  // filter on the SERVER, because that list is paged — see reloadCommits.

  let branches: GitBranch[] = [];
  let bHideMerged = false;
  let bStale = false;
  const bKind = new Set<string>(); // local | remote; empty = both
  const STALE_DAYS = 90;

  let prs: GitPR[] = [];
  const prOn = new Set<string>(); // open | merged | closed | draft; empty = all

  let cNoMerges = false;
  let cQuery = "";
  let cAuthor = "";
  let authors: string[] = [];
  let authorsAsked = false;
  let searchTimer: number | undefined;
  const commitFilterOn = (): boolean => cNoMerges || cQuery !== "" || cAuthor !== "";
  const commitFilter = () => ({ nomerges: cNoMerges, q: cQuery, author: cAuthor });

  /** A row of toggle chips, in the app's existing filter-chip idiom. */
  function chipRow(attr: string, items: { key: string; label: string; on: boolean }[]): string {
    return `<div class="filter-row git-filters">${items
      .map(
        (i) =>
          `<button type="button" ${chipAttrs(i.on)} ${attr}="${i.key}">${i.label}</button>`,
      )
      .join("")}</div>`;
  }

  /** "N filtered out" — an empty list must never read as "there is nothing here". */
  function hiddenNote(shown: number, total: number): string {
    return shown === total ? "" : `<div class="git-filter-note">${total - shown} filtered out</div>`;
  }

  function branchVisible(b: GitBranch): boolean {
    if (bHideMerged && b.merged) return false;
    if (bKind.size && !bKind.has(b.isRemote ? "remote" : "local")) return false;
    if (bStale) {
      const t = b.when ? Date.parse(b.when) : NaN;
      if (!Number.isFinite(t) || Date.now() - t < STALE_DAYS * 86400000) return false;
    }
    return true;
  }

  const prKind = (p: GitPR): string => (p.draft && p.state === "OPEN" ? "draft" : p.state.toLowerCase());

  container.innerHTML = `
    <div class="page git-detail">
      <a class="git-back" href="/project/code">← all repos</a>
      <div id="git-detail-body"><div class="empty-state">loading…</div></div>
    </div>
  `;
  const bodyEl = container.querySelector<HTMLElement>("#git-detail-body")!;

  function headerHtml(r: GitRepo): string {
    const dirty = r.staged + r.unstaged + r.untracked;
    const badge =
      dirty === 0
        ? `<span class="git-badge git-badge--clean">clean</span>`
        : `<span class="git-badge git-badge--dirty">${dirty} dirty</span>`;
    const track = r.hasUpstream
      ? !r.ahead && !r.behind
        ? `<span class="git-track git-track--even">up to date</span>`
        : `${r.ahead ? `<span class="git-track git-track--ahead">↑${r.ahead}</span>` : ""}${
            r.behind ? `<span class="git-track git-track--behind">↓${r.behind}</span>` : ""
          }`
      : "";
    return `
      <header class="git-detail-head">
        <div class="git-detail-title">
          <span class="git-repo-name">${escapeHtml(r.folder || r.root)}</span>
          <span class="git-branch${r.detached ? " git-branch--detached" : ""}">${escapeHtml(r.branch)}</span>
          ${badge}${track}
        </div>
        <div class="git-detail-path">${escapeHtml(r.root)}</div>
      </header>`;
  }

  function tabsHtml(): string {
    return `<nav class="git-tabs">${TABS.map(
      (t) =>
        `<button type="button" class="git-tab${t.key === active ? " git-tab--on" : ""}" data-tab="${t.key}">${t.label}</button>`,
    ).join("")}</nav>`;
  }

  // --- commits tab: the overview's commit list, each expandable to its diff ---

  /** The load-more footer: a button while there's more, a count once we're done. */
  function moreHtml(): string {
    if (exhausted) {
      return `<div class="git-more-wrap"><span class="git-more-done">end of history — ${commits.length} commits</span></div>`;
    }
    return `<div class="git-more-wrap"><button type="button" class="git-more" data-more="1"${
      loadingMore ? " disabled" : ""
    }>${loadingMore ? "loading…" : `load more (${commits.length} commits)`}</button></div>`;
  }

  function authorOptions(): string {
    return (
      `<option value="">all authors</option>` +
      authors
        .map((a) => `<option value="${escapeHtml(a)}"${a === cAuthor ? " selected" : ""}>${escapeHtml(a)}</option>`)
        .join("")
    );
  }

  /** The commits tab's chrome. Rendered ONCE per tab open and then left alone —
      rebuilding it on every list refresh would yank focus out of the search box
      mid-typing. Only #c-list is re-rendered. */
  function commitChromeHtml(): string {
    return `<div class="git-cfilter">
      ${chipRow("data-cf", [{ key: "nomerges", label: "hide merges", on: cNoMerges }])}
      <select class="project-select" id="c-author">${authorOptions()}</select>
      <input class="search-input" id="c-search" type="search" placeholder="search commits…" value="${escapeHtml(cQuery)}" autocomplete="off">
    </div><div id="c-list"></div>`;
  }

  function commitsListHtml(): string {
    if (!commits.length) {
      // A filter reload empties the list and repaints before its request lands;
      // don't read that in-flight window as "no match". A failed reload gets its
      // own message rather than a misleading empty-result one.
      if (loadingMore) return `<div class="git-commits-empty">loading…</div>`;
      if (commitsFailed) return `<div class="git-commits-empty">failed to load commits.</div>`;
      return commitFilterOn()
        ? `<div class="git-commits-empty">no commits match the filters.</div>`
        : `<div class="git-commits-empty">no commits.</div>`;
    }
    return `<ul class="git-commits git-commits--rich">${commits
      .map((c) => {
        const open = expanded === c.hash;
        const detail = open ? commitDetailHtml(c.hash) : "";
        return `
        <li class="git-commit-item${open ? " git-commit-item--open" : ""}">
          <div class="git-commit git-commit--click" data-hash="${escapeHtml(c.hash)}">
            <code class="git-hash">${escapeHtml(c.hash)}</code>
            <span class="git-subject" title="${escapeHtml(c.subject)}">${escapeHtml(c.subject)}</span>
            <span class="git-author">${escapeHtml(c.author)}</span>
            <span class="git-when"${c.when ? ` title="${escapeHtml(c.when)}"` : ""}>${
              c.when ? escapeHtml(formatRelativeTime(c.when)) : ""
            }</span>
          </div>
          ${detail}
        </li>`;
      })
      .join("")}</ul>${moreHtml()}`;
  }

  function commitDetailHtml(hash: string): string {
    const d = commitCache.get(hash);
    if (!d) return `<div class="git-commit-detail"><div class="git-commits-empty">loading diff…</div></div>`;
    const body = d.body ? `<div class="git-commit-body">${escapeHtml(d.body)}</div>` : "";
    return `<div class="git-commit-detail">${body}${fileList(d.files)}${diffBlock(d.diff, d.truncated)}</div>`;
  }

  // --- changes / branches / prs / activity bodies -----------------------------

  async function renderChanges(host: HTMLElement): Promise<void> {
    try {
      const d = await getGitDiff(repo!.root, scope);
      if (dead) return;
      const untracked =
        d.untracked && d.untracked.length
          ? `<div class="git-sub">untracked</div><ul class="git-files">${d.untracked
              .map((p) => `<li class="git-file"><span class="git-fpath">${escapeHtml(p)}</span></li>`)
              .join("")}</ul>`
          : "";
      const changed = (d.files ?? []).length ? `<div class="git-sub">changed</div>${fileList(d.files)}` : "";
      const empty = !changed && !untracked && !d.diff;
      setTabBody(
        host,
        "changes",
        empty
          ? `<div class="git-commits-empty">working tree clean — no changes.</div>`
          : `${changed}${untracked}${d.diff ? diffBlock(d.diff, d.truncated) : ""}`,
      );
    } catch {
      if (!dead) host.innerHTML = `<div class="empty-state">failed to load changes.</div>`;
    }
  }

  function branchRow(b: GitBranch): string {
    const tags = [
      b.isCurrent ? `<span class="git-btag git-btag--cur">current</span>` : "",
      b.isRemote ? `<span class="git-btag git-btag--remote">remote</span>` : "",
      b.merged ? `<span class="git-btag git-btag--merged">merged</span>` : "",
    ].join("");
    const track =
      b.ahead || b.behind
        ? `${b.ahead ? `<span class="git-track git-track--ahead">↑${b.ahead}</span>` : ""}${
            b.behind ? `<span class="git-track git-track--behind">↓${b.behind}</span>` : ""
          }`
        : "";
    return `
      <li class="git-branch-row">
        <span class="git-bname">${escapeHtml(b.name)}</span>
        ${tags}${track}
        <span class="git-bsubject" title="${escapeHtml(b.subject)}">${escapeHtml(b.subject)}</span>
        <span class="git-when"${b.when ? ` title="${escapeHtml(b.when)}"` : ""}>${
          b.when ? escapeHtml(formatRelativeTime(b.when)) : ""
        }</span>
      </li>`;
  }

  function branchesBody(): string {
    if (!branches.length) return `<div class="git-commits-empty">no branches.</div>`;
    const chips = chipRow("data-bf", [
      { key: "merged", label: "hide merged", on: bHideMerged },
      { key: "local", label: "local", on: bKind.has("local") },
      { key: "remote", label: "remote", on: bKind.has("remote") },
      { key: "stale", label: `stale >${STALE_DAYS}d`, on: bStale },
    ]);
    const shown = branches.filter(branchVisible);
    const body = shown.length
      ? `<ul class="git-branch-list">${shown.map(branchRow).join("")}</ul>`
      : `<div class="git-commits-empty">no branches match the filters.</div>`;
    return `${chips}${hiddenNote(shown.length, branches.length)}${body}`;
  }

  async function renderBranches(host: HTMLElement): Promise<void> {
    try {
      const res = await getGitBranches(repo!.root, scope);
      if (dead) return;
      branches = res.branches ?? [];
      setTabBody(host, "branches", branchesBody());
    } catch {
      if (!dead) host.innerHTML = `<div class="empty-state">failed to load branches.</div>`;
    }
  }

  function prRow(p: GitPR): string {
    // state/checks/review are server-side GitHub enums, but escape the text and
    // title uses anyway (defense-in-depth, matching every other field here) —
    // the class-name uses stay raw, safe on the same enum values.
    const state = p.draft && p.state === "OPEN" ? "draft" : p.state.toLowerCase();
    const stateBadge = `<span class="git-pr-state git-pr-state--${state}">${escapeHtml(state)}</span>`;
    const checks = p.checks
      ? `<span class="git-check git-check--${p.checks}" title="checks: ${escapeHtml(p.checks)}"></span>`
      : "";
    const review = p.review
      ? `<span class="git-review git-review--${p.review}">${escapeHtml(p.review.replace(/_/g, " "))}</span>`
      : "";
    return `
      <li class="git-pr-row">
        <span class="git-pr-num">#${p.number}</span>
        ${stateBadge}${checks}
        <a class="git-pr-title" href="${escapeHtml(p.url)}" target="_blank" rel="noopener" title="${escapeHtml(p.title)}">${escapeHtml(p.title)}</a>
        ${review}
        <span class="git-pr-branch">${escapeHtml(p.branch)}</span>
        <span class="git-author">${escapeHtml(p.author)}</span>
        <span class="git-when" title="${escapeHtml(p.updatedAt)}">${escapeHtml(formatRelativeTime(p.updatedAt))}</span>
      </li>`;
  }

  function prsBody(): string {
    if (!prs.length) return `<div class="git-commits-empty">no pull requests yet.</div>`;
    const chips = chipRow("data-pf", [
      { key: "open", label: "open", on: prOn.has("open") },
      { key: "draft", label: "draft", on: prOn.has("draft") },
      { key: "merged", label: "merged", on: prOn.has("merged") },
      { key: "closed", label: "closed", on: prOn.has("closed") },
    ]);
    const shown = prOn.size ? prs.filter((p) => prOn.has(prKind(p))) : prs;
    const body = shown.length
      ? `<ul class="git-pr-list">${shown.map(prRow).join("")}</ul>`
      : `<div class="git-commits-empty">no PRs in the selected state.</div>`;
    return `${chips}${hiddenNote(shown.length, prs.length)}${body}`;
  }

  async function renderPRs(host: HTMLElement): Promise<void> {
    try {
      const res = await getGitPRs(repo!.root, scope);
      if (dead) return;
      if (!res.supported) {
        setTabBody(host, "prs", `<div class="git-commits-empty">this repo has no GitHub remote — no PRs.</div>`);
        return;
      }
      prs = res.prs ?? [];
      setTabBody(host, "prs", prsBody());
    } catch {
      if (!dead) host.innerHTML = `<div class="empty-state">failed to load PRs (gh not responding).</div>`;
    }
  }

  async function renderActivity(host: HTMLElement): Promise<void> {
    try {
      const res = await getGitActivity(repo!.root, scope);
      if (dead) return;
      if (!res.supported) {
        setTabBody(host, "activity", `<div class="git-commits-empty">this repo has no GitHub remote.</div>`);
        return;
      }
      const issues = res.issues ?? [];
      const runs = res.runs ?? [];
      const issuesHtml = issues.length
        ? `<ul class="git-issue-list">${issues
            .map(
              (is) => `
        <li class="git-issue-row">
          <span class="git-pr-num">#${is.number}</span>
          <a class="git-pr-title" href="${escapeHtml(is.url)}" target="_blank" rel="noopener">${escapeHtml(is.title)}</a>
          <span class="git-author">${escapeHtml(is.author)}</span>
          <span class="git-when" title="${escapeHtml(is.updatedAt)}">${escapeHtml(formatRelativeTime(is.updatedAt))}</span>
        </li>`,
            )
            .join("")}</ul>`
        : `<div class="git-commits-empty">no open issues.</div>`;
      const runsHtml = runs.length
        ? `<ul class="git-run-list">${runs
            .map((r) => {
              const st = r.conclusion || r.status || "";
              return `
        <li class="git-run-row">
          <span class="git-check git-check--${st === "success" ? "success" : st === "failure" || st === "cancelled" || st === "timed_out" ? "failure" : "pending"}"></span>
          <a class="git-pr-title" href="${escapeHtml(r.url)}" target="_blank" rel="noopener" title="${escapeHtml(r.title)}">${escapeHtml(r.title)}</a>
          <span class="git-pr-branch">${escapeHtml(r.branch)}</span>
          <span class="git-run-wf">${escapeHtml(r.workflow)}</span>
          <span class="git-when" title="${escapeHtml(r.createdAt)}">${escapeHtml(formatRelativeTime(r.createdAt))}</span>
        </li>`;
            })
            .join("")}</ul>`
        : `<div class="git-commits-empty">no CI runs yet.</div>`;
      setTabBody(
        host,
        "activity",
        `<div class="git-sub">open issues</div>${issuesHtml}<div class="git-sub">recent CI runs</div>${runsHtml}`,
      );
    } catch {
      if (!dead) host.innerHTML = `<div class="empty-state">failed to load issues / CI.</div>`;
    }
  }

  // --- tab plumbing -----------------------------------------------------------

  /** Tears the graph down if it is mounted. Idempotent: every path that replaces
   *  the tab body calls it, and calling it twice must be free. */
  function disposeGraph(): void {
    graphCleanup?.();
    graphCleanup = null;
  }

  function paint(): void {
    if (!repo) return;
    disposeGraph();
    bodyEl.innerHTML = `${headerHtml(repo)}${tabsHtml()}<section class="git-tab-body" id="git-tab-body"></section>`;
    renderActiveTab();
  }

  function renderActiveTab(): void {
    const host = bodyEl.querySelector<HTMLElement>("#git-tab-body");
    if (!host || !repo) return;
    disposeGraph();
    if (active === "graph") {
      // Mounted fresh every open rather than cached: the graph reads its own
      // index and owns a cytoscape instance, so a stale copy would be both wrong
      // and expensive to keep alive behind an unopened tab.
      host.innerHTML = "";
      graphCleanup = renderCodegraphView(host, repo.root);
      return;
    }
    if (active === "commits") {
      renderCommitsTab(host);
      void ensureAuthors();
      return;
    }
    // Already loaded this visit → restore its body, no refetch. Delegated
    // handlers on bodyEl keep working on the restored HTML.
    const cached = bodyCache.get(active);
    if (cached !== undefined) {
      host.innerHTML = cached;
      return;
    }
    // First open: show loading, then fetch + fill (each render caches on success).
    host.innerHTML = `<div class="empty-state">loading…</div>`;
    if (active === "changes") void renderChanges(host);
    else if (active === "branches") void renderBranches(host);
    else if (active === "prs") void renderPRs(host);
    else if (active === "activity") void renderActivity(host);
  }

  /** Set a network tab's body and cache it for the visit (see bodyCache). Keyed
   *  by the literal tab, so a fetch that resolves after a tab switch caches its
   *  own tab's content rather than the one now showing. */
  function setTabBody(host: HTMLElement, tab: TabKey, html: string): void {
    host.innerHTML = html;
    bodyCache.set(tab, html);
  }

  /** Paints the commits tab: chrome once, list every time. */
  function renderCommitsTab(host: HTMLElement): void {
    const list = host.querySelector<HTMLElement>("#c-list");
    if (!list) {
      host.innerHTML = commitChromeHtml();
    } else {
      // Re-sync the controls in place; never rebuild the live search input.
      host
        .querySelector<HTMLButtonElement>('[data-cf="nomerges"]')
        ?.classList.toggle("filter-chip-on", cNoMerges);
      const sel = host.querySelector<HTMLSelectElement>("#c-author");
      if (sel && sel.options.length !== authors.length + 1) {
        sel.innerHTML = authorOptions();
        sel.value = cAuthor;
      }
    }
    host.querySelector<HTMLElement>("#c-list")!.innerHTML = commitsListHtml();
  }

  /** Fill the author picker once per visit. Costs one 1-commit request; without
      it the picker would stay empty until a filter happened to be applied. */
  async function ensureAuthors(): Promise<void> {
    if (authorsAsked || !repo) return;
    authorsAsked = true;
    try {
      const res = await getGitCommits(repo.root, 0, 1, scope);
      if (dead || !res.authors?.length) return;
      authors = res.authors;
      if (active === "commits") renderActiveTab();
    } catch {
      /* the picker just stays empty */
    }
  }

  /** Re-run the commit list for the current filter. With no filter we fall back to
      the snapshot's instant 20; with one, page 1 comes from the server so the
      filter applies to the WHOLE history, not just the rows already loaded. */
  async function reloadCommits(): Promise<void> {
    if (!repo) return;
    expanded = null;
    commitsFailed = false;
    if (!commitFilterOn()) {
      commits = repo.commits ?? [];
      exhausted = commits.length < FIRST_BATCH;
      if (active === "commits") renderActiveTab();
      return;
    }
    commits = [];
    exhausted = false;
    loadingMore = true;
    if (active === "commits") renderActiveTab();
    try {
      const res = await getGitCommits(repo.root, 0, PAGE, scope, commitFilter());
      if (dead) return;
      commits = res.commits ?? [];
      if (res.authors?.length) authors = res.authors;
      exhausted = commits.length < PAGE;
    } catch {
      if (!dead) {
        exhausted = true;
        commitsFailed = true;
      }
    } finally {
      loadingMore = false;
      if (!dead && active === "commits") renderActiveTab();
    }
  }

  /** Pull the next page of history and append it. A short page means we've hit
      the end, so the button gives way to the final count. */
  async function loadMore(): Promise<void> {
    if (loadingMore || exhausted || !repo) return;
    loadingMore = true;
    if (active === "commits") renderActiveTab();
    try {
      const res = await getGitCommits(repo.root, commits.length, PAGE, scope, commitFilter());
      if (dead) return;
      const page = res.commits ?? [];
      commits = commits.concat(page);
      if (page.length < PAGE) exhausted = true;
    } catch {
      if (dead) return;
      exhausted = true; // don't strand the user clicking a button that keeps failing
    } finally {
      loadingMore = false;
      if (!dead && active === "commits") renderActiveTab();
    }
  }

  async function toggleCommit(hash: string): Promise<void> {
    expanded = expanded === hash ? null : hash;
    if (active === "commits") renderActiveTab();
    if (expanded === hash && !commitCache.has(hash)) {
      try {
        const d = await getGitCommit(repo!.root, hash, scope);
        if (dead) return;
        commitCache.set(hash, d);
        if (expanded === hash && active === "commits") renderActiveTab();
      } catch {
        if (!dead && active === "commits") renderActiveTab();
      }
    }
  }

  bodyEl.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    const tab = target.closest<HTMLButtonElement>("[data-tab]");
    if (tab?.dataset["tab"]) {
      active = tab.dataset["tab"] as TabKey;
      paint();
      return;
    }
    if (target.closest("[data-more]")) {
      void loadMore();
      return;
    }
    // --- filter chips ---
    const bf = target.closest<HTMLButtonElement>("[data-bf]")?.dataset["bf"];
    if (bf) {
      if (bf === "merged") bHideMerged = !bHideMerged;
      else if (bf === "stale") bStale = !bStale;
      else if (bKind.has(bf)) bKind.delete(bf);
      else bKind.add(bf);
      const host = bodyEl.querySelector<HTMLElement>("#git-tab-body");
      if (host) setTabBody(host, "branches", branchesBody());
      return;
    }
    const pf = target.closest<HTMLButtonElement>("[data-pf]")?.dataset["pf"];
    if (pf) {
      if (prOn.has(pf)) prOn.delete(pf);
      else prOn.add(pf);
      const host = bodyEl.querySelector<HTMLElement>("#git-tab-body");
      if (host) setTabBody(host, "prs", prsBody());
      return;
    }
    if (target.closest<HTMLButtonElement>('[data-cf="nomerges"]')) {
      cNoMerges = !cNoMerges;
      void reloadCommits();
      return;
    }
    if (target.closest("a")) return; // PR/issue/run links open normally
    const commit = target.closest<HTMLElement>(".git-commit--click");
    if (commit?.dataset["hash"]) void toggleCommit(commit.dataset["hash"]);
  });

  // The author picker and the search box: a change re-runs the server-side query.
  // Typing is debounced so a fast typist doesn't fire a request per keystroke.
  bodyEl.addEventListener("change", (e) => {
    const sel = (e.target as HTMLElement).closest<HTMLSelectElement>("#c-author");
    if (!sel) return;
    cAuthor = sel.value;
    void reloadCommits();
  });
  bodyEl.addEventListener("input", (e) => {
    const inp = (e.target as HTMLElement).closest<HTMLInputElement>("#c-search");
    if (!inp) return;
    const v = inp.value.trim();
    window.clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => {
      if (dead || v === cQuery) return;
      cQuery = v;
      void reloadCommits();
    }, 300);
  });

  async function load(): Promise<void> {
    try {
      const res = await getGit(scope);
      if (dead) return;
      // Matched on the SAME key the list built the link from, not on folder:
      // two checkouts of one repo share a folder and would both answer to it.
      repo = (res.repos ?? []).find((r) => checkoutKey(r.root, r.folder) === folder) ?? null;
      if (!repo) {
        bodyEl.innerHTML = `<div class="empty-state">repo “${escapeHtml(folder)}” is not in the current scope. <a href="/project/code">← back</a></div>`;
        return;
      }
      // Seed the paged list from the snapshot; a short first batch is already the
      // whole history, so no "load more" is offered.
      commits = repo.commits ?? [];
      exhausted = commits.length < FIRST_BATCH;
      paint();
    } catch {
      if (!dead) bodyEl.innerHTML = `<div class="empty-state">failed to load repo.</div>`;
    }
  }

  void load();
  return () => {
    dead = true;
    window.clearTimeout(searchTimer); // a pending debounce must not outlive the view
    disposeGraph(); // nor may the graph's cytoscape instance
  };
}
