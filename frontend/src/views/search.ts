// View 6 — search (route `/claude/search`): find something you said, or Claude said
// back, across every session. Its own surface rather than a block on
// `/claude/insights`, because it serves you mid-task — you're looking for a decision
// you half-remember, not reviewing last week.
//
// The server greps at query time (see search.go); nothing is indexed, so this
// is as live as the transcripts on disk.

import { search } from "../api";
import type { SearchHit, SearchResult } from "../api";
import { escapeHtml, formatRelativeTime, truncate } from "../format";
import { getScopeParam, labelForFolder } from "../scope";

const DAY_OPTS: { v: number; label: string }[] = [
  { v: 7, label: "7d" },
  { v: 30, label: "30d" },
  { v: 90, label: "90d" },
  { v: 0, label: "all" },
];

// Typing fires a full-corpus grep, so wait for a pause. The scan itself runs in
// ~150ms; this is about not starting six of them while a word is being typed.
const DEBOUNCE_MS = 250;
const LIMIT = 100;

/** Renders the search view into `container`; returns a cleanup callback. */
export function renderSearchView(container: HTMLElement): () => void {
  // The nav's global project scope is the only project filter (a scope change
  // re-renders the whole view, so reading it once is enough).
  const scopeParam = getScopeParam();
  let days = 0; // searching is a hunt through history — start with all of it
  let timer: number | undefined;
  let seq = 0; // drops results from a query the user has already typed past

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          <div class="filter-row" id="s-days"></div>
        </div>
      </header>
      <section class="card search-card">
        <input class="search-input" id="s-q" type="search" autocomplete="off" spellcheck="false"
               placeholder="find something you said, or Claude said back…" />
        <div id="s-results"><div class="empty-state">type to search every transcript — nothing is indexed, each query re-reads them.</div></div>
      </section>
    </div>
  `;

  const daysEl = container.querySelector<HTMLElement>("#s-days")!;
  const qEl = container.querySelector<HTMLInputElement>("#s-q")!;
  const resultsEl = container.querySelector<HTMLElement>("#s-results")!;

  function renderDays(): void {
    daysEl.innerHTML = DAY_OPTS.map(
      (o) =>
        `<button type="button" class="filter-chip${o.v === days ? " filter-chip-on" : ""}" ` +
        `data-days="${o.v}">${escapeHtml(o.label)}</button>`,
    ).join("");
  }
  renderDays();

  async function run(): Promise<void> {
    const q = qEl.value.trim();
    if (q === "") {
      resultsEl.innerHTML = `<div class="empty-state">type to search every transcript — nothing is indexed, each query re-reads them.</div>`;
      return;
    }
    const mine = ++seq;
    try {
      const res = await search(q, days, scopeParam, LIMIT);
      if (mine !== seq) return; // a newer query already went out
      resultsEl.innerHTML = resultsHtml(res, q);
    } catch (err) {
      if (mine !== seq) return;
      resultsEl.innerHTML = `<div class="empty-state">search failed</div>`;
      console.error("search failed", err);
    }
  }

  function schedule(): void {
    window.clearTimeout(timer);
    timer = window.setTimeout(() => void run(), DEBOUNCE_MS);
  }

  qEl.addEventListener("input", schedule);
  qEl.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      window.clearTimeout(timer);
      void run();
    }
  });
  daysEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-days]");
    if (!btn) return;
    days = Number(btn.dataset["days"]);
    renderDays();
    void run();
  });

  qEl.focus();
  return () => window.clearTimeout(timer);
}

function resultsHtml(res: SearchResult, q: string): string {
  if (res.hits.length === 0) {
    return `<div class="empty-state">nothing matched — ${res.files} transcripts read in ${res.tookMs}ms.</div>`;
  }
  const shown = res.truncated
    ? `showing ${res.hits.length} of ${res.matched} matches`
    : `${res.matched} match${res.matched === 1 ? "" : "es"}`;
  return `
    <div class="search-meta">${shown} · ${res.files} transcripts read in ${res.tookMs}ms</div>
    <div class="search-hits">${res.hits.map((h) => hitHtml(h, q)).join("")}</div>
  `;
}

function hitHtml(h: SearchHit, q: string): string {
  return `
    <a class="search-hit" href="/claude/session/${encodeURIComponent(h.sessionId)}">
      <div class="search-hit-head">
        <span class="search-hit-role search-hit-role--${h.role === "user" ? "user" : "ai"}">${escapeHtml(h.role)}</span>
        <span class="search-hit-title">${escapeHtml(truncate(h.title || "untitled session", 46))}</span>
        <span class="search-hit-meta">${escapeHtml(labelForFolder(h.project))} · ${escapeHtml(formatRelativeTime(h.ts))}</span>
      </div>
      <div class="search-hit-snippet">${highlight(h.snippet, q)}</div>
    </a>`;
}

// Escapes first, then marks the match — the snippet is transcript text and must
// never reach the DOM as markup.
function highlight(snippet: string, q: string): string {
  const safe = escapeHtml(snippet);
  const needle = escapeHtml(q);
  if (needle === "") return safe;
  const i = safe.toLowerCase().indexOf(needle.toLowerCase());
  if (i < 0) return safe;
  return (
    safe.slice(0, i) + `<mark class="search-mark">` + safe.slice(i, i + needle.length) + `</mark>` + safe.slice(i + needle.length)
  );
}
