// View 1 — sessions list (default route `/`). The per-session half: what ran,
// when, how big. Everything that AGGREGATES — totals, heatmap, model mix, the
// tokens chart — moved to the usage tab next door, because this route was two
// pages stacked and they are read at different moments.

import { getSessions, subscribeSessionEvents } from "../api";
import type { Session } from "../api";
import {
  escapeHtml,
  formatCost,
  formatDuration,
  formatRelativeTime,
  formatTokens,
  linesBadgeHtml,
  modelBadgeHtml,
  truncate,
} from "../domain/format";
import { showError } from "../app/live";
import { loadFilters, renderFilterBar, saveFilters } from "../domain/filters";
import type { FilterState, StatusFilter } from "../domain/filters";
import { isNotifyOn, notifySupported, toggleNotify } from "../app/notify";
import { getScopeParam, getScopeSet, labelForFolder, navigate, scopeChipHtml } from "../scope";

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
          ${scopeChipHtml()}
          <button type="button" class="notify-btn" id="notify-btn" aria-label="toggle notifications">🔔</button>
          <div id="filter-slot"></div>
        </div>
      </header>

      <section class="live-strip hidden" id="live-strip"></section>
      <section class="sessions-table-wrap" id="table-wrap">
        <div class="empty-state">loading…</div>
      </section>
    </div>
  `;

  const notifyBtn = container.querySelector<HTMLButtonElement>("#notify-btn")!;
  const filterSlot = container.querySelector<HTMLElement>("#filter-slot")!;
  const liveStripEl = container.querySelector<HTMLElement>("#live-strip")!;
  const tableWrapEl = container.querySelector<HTMLElement>("#table-wrap")!;

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
      const fresh = await getSessions(filters.days, scopeParam, filters.status);
      if (disposed) return; // navigated away mid-fetch
      sessions = fresh;
      renderLiveStrip(liveStripEl, sessions);
      renderTable(tableWrapEl, sessions, filters);
    } catch (err) {
      showError(tableWrapEl, "failed to load sessions", () => void refresh());
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

    // No fetch on a tick: the row we were sent IS the update, and the
    // aggregates that did need refetching live on the usage tab now. A running
    // session emits an event per tool use, so this used to be three requests
    // each time.
    renderLiveStrip(liveStripEl, sessions);
    renderTable(tableWrapEl, sessions, filters);
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
