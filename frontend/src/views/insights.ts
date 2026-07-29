// View 5 — insights (route `/claude/insights`): cross-session quality signals, one
// card per concern — "rework radar" (churn by file, /api/churn), "friction"
// (the sessions you kept stopping, /api/friction), "work sizing" (the ones that
// outgrew their context, /api/sizing) and "cost per outcome" (/api/ledger).
// Further blocks go in as sibling <section class="card"> elements inside
// #insights-sections — append there, don't restructure the page.
//
// Every block reads the one filter row in the topbar; a change refetches all.

import { getChurn, getFriction, getLedger, getSizing } from "../api";
import type {
  ChurnFile,
  ChurnResult,
  ChurnSessionRef,
  FrictionResult,
  FrictionSession,
  LedgerResult,
  LedgerWeek,
  SizingResult,
  SizingSession,
  ToolCount,
} from "../api";
import {
  chipAttrs,
  escapeHtml,
  formatCost,
  formatDuration,
  formatRelativeTime,
  formatTokens,
  linesBadgeHtml,
  truncate,
} from "../domain/format";
import { showError } from "../app/live";
import { getScopeParam, labelForFolder, scopeChipHtml } from "../scope";

interface InsightsFilters {
  days: number;
}

const DEFAULT_FILTERS: InsightsFilters = { days: 30 };
const FILTERS_KEY = "wyac.insights.filters";

const DAY_OPTS: { v: number; label: string }[] = [
  { v: 7, label: "7d" },
  { v: 30, label: "30d" },
  { v: 90, label: "90d" },
  { v: 0, label: "all" },
];

// Server defaults for /api/churn — a file only one session ever touched isn't
// rework, and the payload is capped so a busy history stays a page, not a dump.
const CHURN_MIN = 2;
const CHURN_LIMIT = 50;
const FRICTION_LIMIT = 20;
const SIZING_LIMIT = 20;

function loadFilters(): InsightsFilters {
  try {
    const raw = localStorage.getItem(FILTERS_KEY);
    if (raw) {
      const parsed = JSON.parse(raw) as Partial<InsightsFilters>;
      const cleaned: InsightsFilters = { days: parsed.days ?? DEFAULT_FILTERS.days };
      saveFilters(cleaned); // drops a stale persisted `project` key instead of carrying it forward
      return cleaned;
    }
  } catch {
    /* corrupt or unavailable storage — fall back to defaults */
  }
  return { ...DEFAULT_FILTERS };
}

function saveFilters(f: InsightsFilters): void {
  try {
    localStorage.setItem(FILTERS_KEY, JSON.stringify(f));
  } catch {
    /* storage unavailable — filters just won't persist */
  }
}

/** Renders the insights view into `container`; returns a cleanup callback. */
export function renderInsightsView(container: HTMLElement): () => void {
  let filters = loadFilters();
  let churn: ChurnResult = { files: [], totalFiles: 0 };
  let friction: FrictionResult = { sessions: [], totalSessions: 0, interrupts: 0, denials: 0 };
  const expandedPaths = new Set<string>(); // churn rows currently showing their refs

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          ${scopeChipHtml()}
          <div class="filter-row" id="ins-days"></div>
        </div>
      </header>
      <div id="insights-sections">
        <section class="card churn-card">
          <h2 class="section-heading">rework radar</h2>
          <p class="section-desc">Where the loop went in circles: files ranked by the lines that got
          written and then unwritten again — not by how often they were touched, which just finds
          the files every session appends a line to.</p>
          <div id="churn-wrap" class="table-scroll"><div class="empty-state">loading…</div></div>
        </section>
        <section class="card friction-card">
          <h2 class="section-heading">friction</h2>
          <p class="section-desc">The sessions you kept stopping — a session you hit ESC in six
          times is one whose prompt, or CLAUDE.md, was wrong. What it cost sits next to the count.</p>
          <div id="friction-wrap" class="table-scroll"><div class="empty-state">loading…</div></div>
        </section>
        <section class="card sizing-card">
          <h2 class="section-heading">work sizing</h2>
          <p class="section-desc">Work that outgrew one session. A compaction means the context
          filled up and had to be squeezed — the mark of a task too big for one sitting.</p>
          <div id="sizing-wrap" class="table-scroll"><div class="empty-state">loading…</div></div>
        </section>
        <section class="card ledger-card">
          <h2 class="section-heading">cost per outcome</h2>
          <p class="section-desc">What a week of spending produced. Each week's <em>whole</em> cost
          divided by what came out of it — not the price of a PR: exploring, debugging and arguing
          about a plan all cost money and open nothing.</p>
          <div id="ledger-wrap" class="table-scroll"><div class="empty-state">loading…</div></div>
        </section>
      </div>
    </div>
  `;

  const daysEl = container.querySelector<HTMLElement>("#ins-days")!;
  const churnWrapEl = container.querySelector<HTMLElement>("#churn-wrap")!;
  const frictionWrapEl = container.querySelector<HTMLElement>("#friction-wrap")!;
  const sizingWrapEl = container.querySelector<HTMLElement>("#sizing-wrap")!;
  const ledgerWrapEl = container.querySelector<HTMLElement>("#ledger-wrap")!;

  function renderDaysChips(): void {
    daysEl.innerHTML = DAY_OPTS.map(
      (o) =>
        `<button type="button" ${chipAttrs(o.v === filters.days)} ` +
        `data-days="${o.v}">${escapeHtml(o.label)}</button>`,
    ).join("");
  }
  renderDaysChips();
  daysEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-days]");
    if (!btn) return;
    filters = { ...filters, days: Number(btn.dataset["days"]) };
    saveFilters(filters);
    renderDaysChips();
    void refresh();
  });

  function renderChurnTable(): void {
    churnWrapEl.innerHTML = churnTableHtml(churn, filters, expandedPaths);
    churnWrapEl.querySelectorAll<HTMLElement>(".churn-row").forEach((row) => {
      row.addEventListener("click", () => {
        const path = row.dataset["path"]!;
        if (expandedPaths.has(path)) expandedPaths.delete(path);
        else expandedPaths.add(path);
        renderChurnTable();
      });
    });
  }

  // Bumped on every refresh so a slower earlier fetch (an old day/project
  // filter) can't land after a newer one and leave two cards on different
  // windows. The four blocks share it — one filter change, one generation.
  let seq = 0;
  // Each block loads on its own so one failing endpoint doesn't blank the other.
  async function refresh(): Promise<void> {
    const mine = ++seq;
    const project = getScopeParam();
    await Promise.all([
      getChurn(filters.days, project, CHURN_MIN, CHURN_LIMIT)
        .then((res) => {
          if (mine !== seq) return;
          churn = res;
          // A fresh fetch can reorder/drop rows — stale expanded state would
          // show refs next to the wrong file, so start collapsed again.
          expandedPaths.clear();
          renderChurnTable();
        })
        .catch((err: unknown) => {
          if (mine !== seq) return;
          showError(churnWrapEl, "failed to load rework data", () => void refresh());
          console.error("failed to load churn", err);
        }),
      getFriction(filters.days, project, FRICTION_LIMIT)
        .then((res) => {
          if (mine !== seq) return;
          friction = res;
          frictionWrapEl.innerHTML = frictionHtml(friction, filters);
        })
        .catch((err: unknown) => {
          if (mine !== seq) return;
          showError(frictionWrapEl, "failed to load friction data", () => void refresh());
          console.error("failed to load friction", err);
        }),
      getSizing(filters.days, project, SIZING_LIMIT)
        .then((res) => {
          if (mine !== seq) return;
          sizingWrapEl.innerHTML = sizingHtml(res, filters);
        })
        .catch((err: unknown) => {
          if (mine !== seq) return;
          showError(sizingWrapEl, "failed to load sizing data", () => void refresh());
          console.error("failed to load sizing", err);
        }),
      getLedger(filters.days, project)
        .then((res) => {
          if (mine !== seq) return;
          ledgerWrapEl.innerHTML = ledgerHtml(res);
        })
        .catch((err: unknown) => {
          if (mine !== seq) return;
          showError(ledgerWrapEl, "failed to load ledger data", () => void refresh());
          console.error("failed to load ledger", err);
        }),
    ]);
  }

  void refresh();

  // No live subscription: churn is a cross-session aggregate over the chosen
  // window, not a single session's live state — a manual filter change or
  // revisit is enough to see it move. Kept simple until a later block needs it.
  return () => {};
}

// Shared by every block: a list that got capped must say so, or it reads as the
// whole picture. `total` is what passed the server's filter, `shown` what fit.
function captionText(shown: number, total: number, noun: string, days: number): string {
  const windowLabel = days === 0 ? "all time" : `last ${days}d`;
  const countLabel =
    shown < total ? `top ${shown} of ${total} ${noun}s` : `${total} ${noun}${total === 1 ? "" : "s"}`;
  return `${countLabel} · ${windowLabel}`;
}

function churnTableHtml(churn: ChurnResult, f: InsightsFilters, expanded: Set<string>): string {
  if (churn.files.length === 0) {
    return `<div class="empty-state">no file was reworked across sessions in this window — the loop isn't circling.</div>`;
  }
  const rows = churn.files.map((cf) => churnRowHtml(cf, expanded.has(cf.path))).join("");
  return `
    <table class="sessions-table churn-table">
      <thead>
        <tr>
          <th></th>
          <th>file</th>
          <th title="min(added, removed) — the writing that got unwritten">churned</th>
          <th>sessions</th>
          <th>edits</th>
          <th>lines</th>
          <th>last touched</th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
    </table>
    <div class="table-caption">${escapeHtml(captionText(churn.files.length, churn.totalFiles, "file", f.days))}</div>
  `;
}

function frictionHtml(fr: FrictionResult, f: InsightsFilters): string {
  if (fr.sessions.length === 0) {
    return `<div class="empty-state">nothing was interrupted or refused in this window — it ran clean.</div>`;
  }
  const rows = fr.sessions.map(frictionRowHtml).join("");
  return `
    <p class="friction-totals">${escapeHtml(totalsText(fr))}</p>
    <table class="sessions-table friction-table">
      <thead>
        <tr>
          <th>session</th>
          <th>project</th>
          <th title="times you stopped it mid-flight">esc</th>
          <th title="tool permissions you refused">denied</th>
          <th>cost</th>
          <th>tokens</th>
          <th>when</th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
    </table>
    <div class="table-caption">${escapeHtml(captionText(fr.sessions.length, fr.totalSessions, "session", f.days))}</div>
    ${denialFootnoteHtml(fr.denialTools)}
  `;
}

// The totals span every session in the window, not just the rows above — say so,
// or the numbers read as a broken sum of the table.
function totalsText(fr: FrictionResult): string {
  const esc = `${fr.interrupts} interrupt${fr.interrupts === 1 ? "" : "s"}`;
  const den = `${fr.denials} refused permission${fr.denials === 1 ? "" : "s"}`;
  return `${esc} · ${den} — across every session in the window`;
}

// Denials are footnote-scale (a handful in a whole corpus). One muted line, not
// a chart: a bar per tool would imply a signal that isn't there.
function denialFootnoteHtml(tools: ToolCount[] | undefined): string {
  if (!tools || tools.length === 0) return "";
  const list = tools.map((t) => `${escapeHtml(t.name)} ×${t.count}`).join(", ");
  return `<div class="friction-denials">permission refused: ${list}</div>`;
}

function ledgerHtml(l: LedgerResult): string {
  if (l.weeks.length === 0) {
    return `<div class="empty-state">nothing spent in this window.</div>`;
  }
  const rows = l.weeks.map((w) => ledgerRowHtml(w, false)).join("");
  return `
    <table class="sessions-table ledger-table">
      <thead>
        <tr>
          <th>week</th>
          <th>sessions</th>
          <th>cost</th>
          <th>PRs</th>
          <th title="the week's whole spend divided by the PRs it opened">$/PR</th>
          <th>lines</th>
          <th>$/1k lines</th>
          <th>releases</th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
      <tfoot>${ledgerRowHtml(l.total, true)}</tfoot>
    </table>
  `;
}

// One week, or the window's total when `isTotal` — same columns either way, so
// the summed row reads against the ones above it rather than in its own format.
function ledgerRowHtml(w: LedgerWeek, isTotal: boolean): string {
  const label = isTotal ? "all of it" : `${escapeHtml(w.week)} · ${escapeHtml(shortDate(w.startsOn))}`;
  const perPR = w.prs > 0 ? escapeHtml(formatCost(w.costPerPr)) : "—";
  const perK = w.lines > 0 ? escapeHtml(formatCost(w.costPer1kLines)) : "—";
  return `
    <tr class="ledger-row${isTotal ? " ledger-total" : ""}">
      <td class="cell-title">${label}</td>
      <td class="cell-muted">${w.sessions}</td>
      <td>${escapeHtml(formatCost(w.costUsd))}</td>
      <td class="cell-muted">${w.prs}</td>
      <td class="ledger-per">${perPR}</td>
      <td class="cell-muted">${w.lines.toLocaleString("en-US")}</td>
      <td class="ledger-per">${perK}</td>
      <td class="cell-muted">${w.releases || "—"}</td>
    </tr>`;
}

// "Jul 13" — the Monday a week starts on, next to its ISO label.
function shortDate(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
}

function sizingHtml(sz: SizingResult, f: InsightsFilters): string {
  if (sz.sessions.length === 0) {
    return `<div class="empty-state">nothing compacted in this window — every session fit in its context.</div>`;
  }
  const rows = sz.sessions.map(sizingRowHtml).join("");
  return `
    <p class="sizing-medians">${medianText(sz)}</p>
    <table class="sessions-table sizing-table">
      <thead>
        <tr>
          <th>session</th>
          <th>project</th>
          <th title="times the context filled up and was squeezed">compactions</th>
          <th>cost</th>
          <th>tokens</th>
          <th>ran for</th>
          <th>when</th>
        </tr>
      </thead>
      <tbody>${rows}</tbody>
    </table>
    <div class="table-caption">${escapeHtml(captionText(sz.sessions.length, sz.totalSessions, "session", f.days))} · ${sz.scanned} scanned</div>
  `;
}

// States the two medians and refuses the causal reading of them. Compacting
// doesn't spend the money; it marks the work that was already going to.
function medianText(sz: SizingResult): string {
  const clean = `<b>${escapeHtml(formatCost(sz.medianCostClean))}</b> median across ${sz.cleanCount} sessions that never compacted`;
  if (sz.heavyCount === 0) {
    return `${clean} — none hit ${sz.heavyThreshold}+ compactions in this window.`;
  }
  const heavy = `<b>${escapeHtml(formatCost(sz.medianCostHeavy))}</b> across the ${sz.heavyCount} that compacted ${sz.heavyThreshold}+ times`;
  return `${clean}, ${heavy}. The compacting isn't what costs — it's what work too big for one sitting leaves behind.`;
}

function sizingRowHtml(s: SizingSession): string {
  return `
    <tr class="sizing-row">
      <td class="cell-title">
        <a class="friction-link" href="/claude/session/${encodeURIComponent(s.id)}">${escapeHtml(
          truncate(s.title || "untitled session", 46),
        )}</a>
      </td>
      <td class="cell-muted">${escapeHtml(labelForFolder(s.project))}</td>
      <td class="sizing-compactions">${s.compactions}</td>
      <td>${escapeHtml(formatCost(s.costUsd))}</td>
      <td class="cell-muted">${escapeHtml(formatTokens(s.totalTokens))}</td>
      <td class="cell-muted">${escapeHtml(formatDuration(s.durationMs))}</td>
      <td>${escapeHtml(formatRelativeTime(s.endedAt))}</td>
    </tr>`;
}

function frictionRowHtml(s: FrictionSession): string {
  return `
    <tr class="friction-row">
      <td class="cell-title">
        <a class="friction-link" href="/claude/session/${encodeURIComponent(s.id)}">${escapeHtml(
          truncate(s.title || "untitled session", 52),
        )}</a>
      </td>
      <td class="cell-muted">${escapeHtml(labelForFolder(s.project))}</td>
      <td class="friction-esc">${s.interrupts}</td>
      <td>${s.denials || ""}</td>
      <td>${escapeHtml(formatCost(s.costUsd))}</td>
      <td class="cell-muted">${escapeHtml(formatTokens(s.totalTokens))}</td>
      <td>${escapeHtml(formatRelativeTime(s.endedAt))}</td>
    </tr>`;
}

/** Trailing 2–3 path segments for display; the full path stays in `title`. */
function shortenPath(path: string): string {
  const parts = path.split("/").filter(Boolean);
  if (parts.length <= 3) return path;
  return "…/" + parts.slice(-3).join("/");
}

function churnRowHtml(cf: ChurnFile, isOpen: boolean): string {
  const main = `
    <tr class="churn-row${isOpen ? " churn-row-open" : ""}" data-path="${escapeHtml(cf.path)}">
      <td class="churn-chevron">${isOpen ? "▾" : "▸"}</td>
      <td class="cell-title" title="${escapeHtml(cf.path)}">${escapeHtml(shortenPath(cf.path))}</td>
      <td class="churn-churned">${cf.churnedLines}</td>
      <td>${cf.sessions}</td>
      <td>${cf.edits}</td>
      <td class="cell-lines">${linesBadgeHtml(cf.linesAdded, cf.linesRemoved)}</td>
      <td>${escapeHtml(formatRelativeTime(cf.lastTouched))}</td>
    </tr>`;
  if (!isOpen) return main;
  return main + `<tr class="churn-detail-row"><td colspan="7">${churnRefsHtml(cf.refs)}</td></tr>`;
}

function churnRefsHtml(refs: ChurnSessionRef[]): string {
  if (refs.length === 0) return `<div class="empty-state">no session refs</div>`;
  return `<div class="churn-refs">${refs.map(churnRefHtml).join("")}</div>`;
}

// costUsd is that session's total cost, not a per-file share of it — the
// label says so explicitly rather than reading as "this file cost $x".
function churnRefHtml(r: ChurnSessionRef): string {
  const edits = `${r.edits} edit${r.edits === 1 ? "" : "s"} here`;
  return `
    <a class="churn-ref" href="/claude/session/${encodeURIComponent(r.id)}">
      <span class="churn-ref-title">${escapeHtml(truncate(r.title || "untitled session", 48))}</span>
      <span class="churn-ref-meta">${escapeHtml(labelForFolder(r.project))} · ${escapeHtml(formatRelativeTime(r.endedAt))} · ${edits} · ${linesBadgeHtml(r.linesAdded, r.linesRemoved)}</span>
      <span class="churn-ref-cost" title="this session's total cost, not a per-file share">session cost ${escapeHtml(formatCost(r.costUsd))}</span>
    </a>`;
}
