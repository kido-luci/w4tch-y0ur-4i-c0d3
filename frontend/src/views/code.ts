// View 9 — code (route `/project/code`): a read-only status dashboard over the
// scope's repos. One compact clickable ROW per repo — name · branch · clean/dirty
// · ahead/behind · its latest commit — so a scope with many repos reads at a
// glance. The full commit history, diffs, branches, PRs, CI and the architecture
// graph live one click deeper, in the repo detail (`/project/code/<folder>`).
// wyac only reads: the server shells `git log` / `status` / `branch`, nothing
// that mutates a repo.

import { getGit } from "../api";
import type { GitRepo } from "../api";
import { chipAttrs, escapeHtml, formatRelativeTime } from "../domain/format";
import { showError } from "../app/live";
import { getScope } from "../scope";

/** Renders the code view into `container`; returns a cleanup callback. */
export function renderCodeView(container: HTMLElement): () => void {
  // The scope LABEL, not its Claude folders: the server resolves it through the
  // project registry's repo bindings now. A scope change re-renders the whole
  // view, so reading it once is fine.
  const scope = getScope();
  let dead = false;
  let repos: GitRepo[] = [];
  // Row filters. None on = show everything; several on = OR (a repo matching any
  // of them stays). Purely client-side — the rows are already in hand.
  const on = new Set<string>();

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="filter-row" id="git-chips"></div>
        <div class="git-meta" id="git-meta"></div>
      </header>
      <section class="git-list" id="git-list">
        <div class="empty-state">loading…</div>
      </section>
    </div>
  `;
  const listEl = container.querySelector<HTMLElement>("#git-list")!;
  const metaEl = container.querySelector<HTMLElement>("#git-meta")!;
  const chipsEl = container.querySelector<HTMLElement>("#git-chips")!;

  const FILTERS: { key: string; label: string; hit: (r: GitRepo) => boolean }[] = [
    { key: "dirty", label: "dirty", hit: (r) => r.staged + r.unstaged + r.untracked > 0 },
    { key: "clean", label: "clean", hit: (r) => r.isRepo && r.staged + r.unstaged + r.untracked === 0 },
    { key: "ahead", label: "ahead", hit: (r) => r.ahead > 0 },
    { key: "behind", label: "behind", hit: (r) => r.behind > 0 },
  ];

  function visible(): GitRepo[] {
    if (!on.size) return repos;
    return repos.filter((r) => FILTERS.some((f) => on.has(f.key) && f.hit(r)));
  }

  function renderChips(): void {
    chipsEl.innerHTML = FILTERS.map(
      (f) =>
        `<button type="button" ${chipAttrs(on.has(f.key))} data-f="${f.key}">${f.label}</button>`,
    ).join("");
  }

  chipsEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-f]");
    if (!btn) return;
    const k = btn.dataset["f"]!;
    if (on.has(k)) on.delete(k);
    else on.add(k);
    renderChips();
    renderList();
  });

  function stateBadge(r: GitRepo): string {
    if (!r.isRepo) return `<span class="git-badge git-badge--none">not a git repo</span>`;
    const dirty = r.staged + r.unstaged + r.untracked;
    return dirty === 0
      ? `<span class="git-badge git-badge--clean">clean</span>`
      : `<span class="git-badge git-badge--dirty" title="${r.staged} staged · ${r.unstaged} changed · ${r.untracked} untracked">${dirty} dirty</span>`;
  }

  function trackBits(r: GitRepo): string {
    if (!r.hasUpstream || (!r.ahead && !r.behind)) return "";
    return (
      (r.ahead ? `<span class="git-track git-track--ahead" title="ahead of upstream">↑${r.ahead}</span>` : "") +
      (r.behind ? `<span class="git-track git-track--behind" title="behind upstream">↓${r.behind}</span>` : "")
    );
  }

  // The latest commit, as one line of context — the deep list is in the detail view.
  function latestCommit(r: GitRepo): { body: string; when: string } {
    const c = (r.commits ?? [])[0];
    if (!c) {
      return { body: `<span class="git-row-commit git-row-commit--empty">${r.isRepo ? "no commits" : ""}</span>`, when: "" };
    }
    return {
      body: `<span class="git-row-commit"><code class="git-hash">${escapeHtml(c.hash)}</code><span class="git-subject" title="${escapeHtml(
        c.subject,
      )}">${escapeHtml(c.subject)}</span></span>`,
      when: c.when ? `<span class="git-when" title="${escapeHtml(c.when)}">${escapeHtml(formatRelativeTime(c.when))}</span>` : "",
    };
  }

  function rowHtml(r: GitRepo): string {
    const branch = r.branch
      ? `<span class="git-branch${r.detached ? " git-branch--detached" : ""}" title="${
          r.detached ? "detached HEAD" : "current branch"
        }">${escapeHtml(r.branch)}</span>`
      : "";
    const { body: commit, when } = latestCommit(r);
    // Every cell is emitted even when empty. The row is a grid, and a grid only
    // lines up if each row fills the same tracks — drop the absent ones and a
    // repo with no upstream shifts its commit under the next repo's branch,
    // which is the ragged column this layout exists to fix.
    const inner =
      `<span class="git-row-name" title="${escapeHtml(r.folder || r.root)}">${escapeHtml(r.folder || r.root)}</span>` +
      `<span class="git-cell">${branch}</span>` +
      `<span class="git-cell">${stateBadge(r)}</span>` +
      `<span class="git-cell git-cell--track">${trackBits(r)}</span>` +
      commit +
      `<span class="git-cell git-cell--when">${when}</span>`;
    // The whole row is the entry point into the detail view; a resolved-but-not-a
    // -repo root isn't clickable (there's nothing to drill into).
    return r.isRepo && r.folder
      ? `<a class="git-row" href="/project/code/${encodeURIComponent(r.folder)}" title="mở chi tiết — ${escapeHtml(r.root)}">${inner}</a>`
      : `<div class="git-row git-row--norepo" title="${escapeHtml(r.root)}">${inner}</div>`;
  }

  /** Re-renders the rows for the current filter, saying plainly when a filter is
      what's hiding repos (an empty list shouldn't read as "no repos"). */
  function renderList(): void {
    const shown = visible();
    const hidden = repos.length - shown.length;
    metaEl.textContent = hidden
      ? `${shown.length} / ${repos.length} repos — ${hidden} filtered out`
      : `${repos.length} repo${repos.length === 1 ? "" : "s"}`;
    // The count reads like "this is what the scope contains"; it isn't. The
    // server resolves repos from the cwds your sessions actually ran in
    // (cgRepos), so a configured project you've never opened Claude in never
    // appears — a scope with ten game projects can legitimately show one. The
    // empty state already says this; the count needs it more, because a
    // plausible number invites no questions.
    metaEl.title = "repos are resolved from the working trees your Claude sessions ran in, so a project with no session history won't be listed";
    listEl.innerHTML = shown.length
      ? shown.map(rowHtml).join("")
      : `<div class="empty-state">no repos match the filters — drop a chip to see them again.</div>`;
  }

  async function load(): Promise<void> {
    try {
      const res = await getGit(scope);
      if (dead) return;
      repos = res.repos ?? [];
      if (repos.length === 0) {
        metaEl.textContent = "";
        chipsEl.innerHTML = "";
        listEl.innerHTML = `<div class="empty-state">no repos in this scope — the git tab reads the working trees your Claude sessions ran in.</div>`;
        return;
      }
      renderChips();
      renderList();
    } catch (err) {
      if (dead) return;
      metaEl.textContent = "";
      showError(listEl, "failed to load git status", () => void load());
      console.error("git load failed", err);
    }
  }

  void load();
  return () => {
    dead = true;
  };
}
