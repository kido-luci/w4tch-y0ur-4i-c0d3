// View 1 — sessions list (default route `/`).

import { getActivity, getSessions, getStats, subscribeSessionEvents } from "../api";
import type { Activity, ModelUsage, Session, Stats } from "../api";
import {
  escapeHtml,
  formatCost,
  formatDuration,
  formatRelativeTime,
  formatTokens,
  linesBadgeHtml,
  modelBadgeHtml,
  truncate,
} from "../format";
import { announce } from "../live";
import { renderModelDistribution } from "../distribution";
import { renderActivityHeatmap } from "../heatmap";
import { createTokensOverTime } from "../tokensOverTime";
import { loadFilters, renderFilterBar, saveFilters } from "../filters";
import type { FilterState, StatusFilter } from "../filters";
import { isNotifyOn, notifySupported, toggleNotify } from "../notify";
import { getScopeParam, getScopeSet, labelForFolder, navigate } from "../scope";

// The heatmap uses its own fixed window, independent of the day filter.
const HEATMAP_WEEKS = 26;

/** Renders the sessions list view into `container`; returns a cleanup callback. */
export function renderSessionsView(container: HTMLElement): () => void {
  let filters: FilterState = loadFilters();
  // The nav's global project scope is the only project filter (a scope change
  // re-renders the whole view, so reading it once is enough).
  const scopeParam = getScopeParam();
  const scopeSet = getScopeSet();
  let sessions: Session[] = [];
  let unsubscribe: (() => void) | null = null;
  let filterBar: { el: HTMLElement; destroy: () => void } | null = null;
  // Set in cleanup. Anything that resolves after teardown must bail: rendering
  // into detached nodes is merely wasted, but building the filter bar late
  // registers a document-level click listener whose destroy() would never run.
  let disposed = false;

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          <button type="button" class="notify-btn" id="notify-btn" aria-label="toggle notifications">🔔</button>
          <div id="filter-slot"></div>
        </div>
      </header>

      <section class="stat-cards" id="stat-cards"></section>
      <section class="card heatmap-card">
        <h2 class="section-heading">activity · last ${HEATMAP_WEEKS} weeks</h2>
        <div id="heatmap-slot"></div>
      </section>
      <section class="card tot-card" id="tot-card"></section>
      <section class="card">
        <h2 class="section-heading">model distribution</h2>
        <div id="dist-slot"></div>
      </section>
      <section class="live-strip hidden" id="live-strip"></section>
      <section class="sessions-table-wrap" id="table-wrap">
        <div class="empty-state">loading…</div>
      </section>
    </div>
  `;

  const notifyBtn = container.querySelector<HTMLButtonElement>("#notify-btn")!;
  const filterSlot = container.querySelector<HTMLElement>("#filter-slot")!;
  const statCardsEl = container.querySelector<HTMLElement>("#stat-cards")!;
  const heatmapSlotEl = container.querySelector<HTMLElement>("#heatmap-slot")!;
  const totCardEl = container.querySelector<HTMLElement>("#tot-card")!;
  const distSlotEl = container.querySelector<HTMLElement>("#dist-slot")!;
  const liveStripEl = container.querySelector<HTMLElement>("#live-strip")!;
  const tableWrapEl = container.querySelector<HTMLElement>("#table-wrap")!;

  const tokensOverTime = createTokensOverTime();
  totCardEl.append(tokensOverTime.el);


  function syncNotifyBtn(): void {
    if (!notifySupported()) {
      notifyBtn.hidden = true;
      return;
    }
    const on = isNotifyOn();
    notifyBtn.classList.toggle("notify-btn-on", on);
    notifyBtn.title = on ? "notifications on" : "notifications off";
  }
  syncNotifyBtn();
  notifyBtn.addEventListener("click", () => {
    void toggleNotify().then(syncNotifyBtn);
  });

  function onFilterChange(next: FilterState): void {
    filters = next;
    saveFilters(next);
    void refresh();
  }

  // Mounts synchronously: with the project chips gone the bar needs no data, so
  // there is no late callback that could outlive teardown.
  filterBar = renderFilterBar(filters, onFilterChange);
  filterSlot.replaceChildren(filterBar.el);

  async function refresh(): Promise<void> {
    try {
      // The chart shows global all-time usage, independent of every filter
      // (day / project / status), so it gets its own unfiltered fetch.
      const [freshSessions, chartSessions, stats, activity] = await Promise.all([
        getSessions(filters.days, scopeParam, filters.status),
        getSessions(0),
        getStats(filters.days, scopeParam, filters.status),
        getActivity(HEATMAP_WEEKS, scopeParam),
      ]);
      if (disposed) return; // navigated away mid-fetch
      sessions = freshSessions;
      renderStatCards(statCardsEl, stats);
      renderHeatmap(heatmapSlotEl, activity);
      renderDist(distSlotEl, sessions);
      tokensOverTime.update(chartSessions, 0);
      renderLiveStrip(liveStripEl, sessions);
      renderTable(tableWrapEl, sessions, filters);
    } catch (err) {
      tableWrapEl.innerHTML = `<div class="empty-state">failed to load sessions</div>`;
      announce("failed to load sessions");
      console.error("failed to load sessions", err);
    }
  }

  void refresh();

  unsubscribe = subscribeSessionEvents((updated) => {
    const matches =
      (scopeSet ? scopeSet.has(updated.project) : true) && matchesStatus(updated, filters.status);
    const idx = sessions.findIndex((s) => s.id === updated.id);

    if (idx >= 0) {
      // Drop rows that no longer match the active filter (e.g. just archived).
      sessions = matches
        ? sessions.map((s, i) => (i === idx ? updated : s))
        : sessions.filter((_, i) => i !== idx);
    } else if (matches) {
      sessions = [updated, ...sessions];
    } else {
      return;
    }

    renderLiveStrip(liveStripEl, sessions);
    renderTable(tableWrapEl, sessions, filters);
    renderDist(distSlotEl, sessions);
    // Chart is global all-time (independent of every filter) — re-fetch
    // unfiltered rather than derive from the filtered in-memory list.
    getSessions(0)
      .then((all) => {
        if (!disposed) tokensOverTime.update(all, 0);
      })
      .catch(() => {
        /* best-effort; keep the last-rendered chart */
      });
    getStats(filters.days, scopeParam, filters.status)
      .then((stats) => {
        if (!disposed) renderStatCards(statCardsEl, stats);
      })
      .catch(() => {
        /* best-effort refresh; keep showing last-known stats */
      });
    getActivity(HEATMAP_WEEKS, scopeParam)
      .then((activity) => {
        if (!disposed) renderHeatmap(heatmapSlotEl, activity);
      })
      .catch(() => {
        /* best-effort; keep the last-rendered heatmap */
      });
  });

  return () => {
    disposed = true;
    unsubscribe?.();
    filterBar?.destroy();
  };
}

function matchesStatus(s: Session, status: StatusFilter): boolean {
  if (status === "active") return !s.archived;
  if (status === "archived") return s.archived;
  return true;
}

function renderHeatmap(el: HTMLElement, activity: Activity): void {
  el.replaceChildren(renderActivityHeatmap(activity));
}

/** Sum each session's per-model breakdown into one global distribution. */
function aggregateBreakdown(sessions: Session[]): ModelUsage[] {
  const acc = new Map<string, ModelUsage>();
  for (const s of sessions) {
    for (const mu of s.modelBreakdown ?? []) {
      const cur = acc.get(mu.model);
      if (cur) {
        cur.tokens += mu.tokens;
        cur.costUsd += mu.costUsd;
      } else {
        acc.set(mu.model, { model: mu.model, tokens: mu.tokens, costUsd: mu.costUsd });
      }
    }
  }
  return [...acc.values()];
}

function renderDist(el: HTMLElement, sessions: Session[]): void {
  el.replaceChildren(renderModelDistribution(aggregateBreakdown(sessions)));
}

function renderStatCards(el: HTMLElement, stats: Stats): void {
  el.innerHTML = `
    <div class="stat-card">
      <div class="stat-label">sessions</div>
      <div class="stat-value">${stats.sessions.toLocaleString("en-US")}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">total tokens</div>
      <div class="stat-value">${formatTokens(stats.totalTokens)}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">est. cost</div>
      <div class="stat-value">${formatCost(stats.totalCostUsd)}</div>
    </div>
    <div class="stat-card">
      <div class="stat-label">agent spawns</div>
      <div class="stat-value">${formatTokens(stats.agentSpawns)}</div>
    </div>
  `;
}

function renderLiveStrip(el: HTMLElement, sessions: Session[]): void {
  const running = sessions.filter((s) => s.running);
  if (running.length === 0) {
    el.innerHTML = "";
    el.classList.add("hidden");
    return;
  }

  el.classList.remove("hidden");
  el.innerHTML = running
    .map((s) => {
      const combinedTokens = s.totalTokens + s.agentTokens;
      return `
        <a class="live-card" href="/claude/session/${encodeURIComponent(s.id)}">
          <span class="live-dot"></span>
          <div class="live-card-body">
            <div class="live-card-title">${escapeHtml(truncate(s.title || "untitled session", 32))}</div>
            <div class="live-card-meta">${escapeHtml(labelForFolder(s.project))} · ${escapeHtml(formatTokens(combinedTokens))} tok</div>
          </div>
        </a>`;
    })
    .join("");
}

function sessionTitle(s: Session): string {
  return s.title || "untitled session";
}

/** Order sessions: running first, then by the chosen sort key. */
function orderSessions(sessions: Session[], f: FilterState): Session[] {
  return [...sessions].sort((a, b) => {
    if (a.running !== b.running) return Number(b.running) - Number(a.running);
    if (f.sortBy === "alpha") return sessionTitle(a).localeCompare(sessionTitle(b));
    return Date.parse(b.startedAt) - Date.parse(a.startedAt);
  });
}

const TABLE_COLS = 11;

function renderTable(el: HTMLElement, sessions: Session[], f: FilterState): void {
  if (sessions.length === 0) {
    el.innerHTML = `<div class="empty-state">no sessions match these filters</div>`;
    return;
  }

  const ordered = orderSessions(sessions, f);
  let bodyRows: string;

  if (f.groupBy === "project") {
    const groups = new Map<string, Session[]>();
    for (const s of ordered) {
      const arr = groups.get(labelForFolder(s.project));
      if (arr) arr.push(s);
      else groups.set(labelForFolder(s.project), [s]);
    }
    bodyRows = [...groups.entries()]
      .sort((a, b) => a[0].localeCompare(b[0]))
      .map(
        ([project, rows]) =>
          `<tr class="group-row"><td class="group-cell" colspan="${TABLE_COLS}">` +
          `${escapeHtml(project)}<span class="group-count">${rows.length}</span></td></tr>` +
          rows.map(sessionRowHtml).join(""),
      )
      .join("");
  } else {
    bodyRows = ordered.map(sessionRowHtml).join("");
  }

  el.innerHTML = `
    <table class="sessions-table">
      <thead>
        <tr>
          <th>title</th>
          <th>project</th>
          <th>started</th>
          <th>duration</th>
          <th>models</th>
          <th>msgs</th>
          <th>agents</th>
          <th>tokens</th>
          <th>files</th>
          <th>lines</th>
          <th>cost</th>
        </tr>
      </thead>
      <tbody>${bodyRows}</tbody>
    </table>
  `;

  el.querySelectorAll<HTMLTableRowElement>("tr[data-session-id]").forEach((row) => {
    row.addEventListener("click", () => {
      navigate(`/claude/session/${encodeURIComponent(row.dataset["sessionId"]!)}`);
    });
  });
}

function sessionRowHtml(s: Session): string {
  const modelBadges = s.models.map((m) => modelBadgeHtml(m)).join("");
  const combinedTokens = s.totalTokens + s.agentTokens;
  const combinedCost = s.costUsd + s.agentCostUsd;
  const liveDot = s.running ? `<span class="live-dot live-dot-inline"></span>` : "";
  const archTag = s.archived ? `<span class="arch-tag">archived</span>` : "";
  const cls = [s.running ? "row-running" : "", s.archived ? "row-archived" : ""]
    .filter(Boolean)
    .join(" ");

  return `
    <tr data-session-id="${escapeHtml(s.id)}" class="${cls}">
      <td class="cell-title">${liveDot}${escapeHtml(truncate(sessionTitle(s), 64))}${archTag}</td>
      <td>${escapeHtml(labelForFolder(s.project))}</td>
      <td>${escapeHtml(formatRelativeTime(s.startedAt))}</td>
      <td>${escapeHtml(formatDuration(s.durationMs))}</td>
      <td class="cell-models">${modelBadges}</td>
      <td>${s.messageCount}</td>
      <td>${s.agentCount}</td>
      <td>${escapeHtml(formatTokens(combinedTokens))}</td>
      <td>${s.filesChanged || "—"}</td>
      <td class="cell-lines">${linesBadgeHtml(s.linesAdded, s.linesRemoved)}</td>
      <td>${escapeHtml(formatCost(combinedCost))}</td>
    </tr>`;
}
