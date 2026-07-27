// Left rail on the detail view — a Claude-Code-style session sidebar: a live,
// filterable, optionally project-grouped switcher so you can jump between
// sessions without going back to the list. It shares the sessions view's filter
// state (localStorage), so Status/Project/etc. stay in sync across both.

import { getSessions } from "./api";
import type { Session } from "./api";
import { DEFAULT_FILTERS, loadFilters, renderFilterBar, saveFilters } from "./filters";
import type { FilterState, GroupBy } from "./filters";
import { escapeHtml, formatRelativeTime, truncate } from "./format";
import { getScopeParam, getScopeSet, labelForFolder } from "./scope";

// Cap how many sessions the rail shows (plus the current one, always) so a busy
// window stays a switcher rather than an endless scroll.
const RAIL_MAX = 60;
const COLLAPSED_KEY = "wyac.rail.collapsed";
const RAIL_GROUPBY_KEY = "wyac.rail.groupby";

// Group-by is rail-specific (a switcher wants project grouping by default,
// while the sessions list stays flat); everything else is shared with the list.
function loadRailGroupBy(): GroupBy {
  try {
    const v = localStorage.getItem(RAIL_GROUPBY_KEY);
    if (v === "none" || v === "project") return v;
  } catch {
    /* ignore corrupt/unavailable storage */
  }
  return "project";
}

function saveRailGroupBy(g: GroupBy): void {
  try {
    localStorage.setItem(RAIL_GROUPBY_KEY, g);
  } catch {
    /* storage unavailable — rail grouping just won't persist */
  }
}

function loadCollapsed(): Set<string> {
  try {
    const raw = localStorage.getItem(COLLAPSED_KEY);
    if (raw) return new Set(JSON.parse(raw) as string[]);
  } catch {
    /* ignore corrupt/unavailable storage */
  }
  return new Set();
}

function saveCollapsed(set: Set<string>): void {
  try {
    localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...set]));
  } catch {
    /* storage unavailable — collapse state just won't persist */
  }
}

function sessionTitle(s: Session): string {
  return s.title || "untitled session";
}

/**
 * Builds the session rail for the detail view. Returns its element (rendered
 * async once sessions load), an `update(session)` fed by the detail view's SSE
 * subscription, and a `destroy` for teardown.
 */
export function renderSessionRail(currentId: string): {
  el: HTMLElement;
  update: (session: Session) => void;
  destroy: () => void;
} {
  // The nav's global project scope is the only project filter (a scope change
  // re-renders the detail view, so reading it once is enough).
  const scopeParam = getScopeParam();
  const scopeSet = getScopeSet();
  // Shared filters (status/days/sort) + the rail's own group-by default.
  let filters: FilterState = { ...loadFilters(), groupBy: loadRailGroupBy() };
  let sessions: Session[] = [];
  // The current session, pinned separately so it survives a status-filtered
  // fetch that would exclude it (e.g. viewing an archived session while Active).
  let pinned: Session | null = null;
  const collapsed = loadCollapsed();
  let destroyFilterBar: (() => void) | null = null;

  const el = document.createElement("nav");
  el.className = "session-rail";
  el.setAttribute("aria-label", "sessions");

  const head = document.createElement("div");
  head.className = "rail-head";
  const list = document.createElement("div");
  list.className = "rail-list";
  list.innerHTML = `<div class="rail-empty">loading…</div>`;
  el.append(head, list);

  // Mount the (shared) filter bar. Synchronous: with the project chips gone the
  // bar needs no data, so nothing can register a listener after teardown.
  const bar = renderFilterBar(
    filters,
    (next) => {
      filters = next;
      // Persist group-by to the rail's own key; share the rest with the
      // list, keeping the list's own group-by choice untouched.
      saveRailGroupBy(next.groupBy);
      saveFilters({ ...next, groupBy: loadFilters().groupBy });
      void fetchAndRender();
    },
    {
      align: "left",
      compact: true,
      resetTo: { ...DEFAULT_FILTERS, groupBy: "project" },
    },
  );
  head.appendChild(bar.el);
  destroyFilterBar = bar.destroy;

  function matchesFilter(s: Session): boolean {
    if (filters.status === "active" && s.archived) return false;
    if (filters.status === "archived" && !s.archived) return false;
    if (scopeSet && !scopeSet.has(s.project)) return false;
    return true;
  }

  function ordered(items: Session[]): Session[] {
    return [...items].sort((a, b) => {
      if (a.running !== b.running) return Number(b.running) - Number(a.running);
      if (filters.sortBy === "alpha") return sessionTitle(a).localeCompare(sessionTitle(b));
      return Date.parse(b.startedAt) - Date.parse(a.startedAt);
    });
  }

  function render(): void {
    let items = sessions;
    if (pinned && !items.some((s) => s.id === pinned!.id)) items = [pinned, ...items];
    if (items.length === 0) {
      list.innerHTML = `<div class="rail-empty">no sessions</div>`;
      return;
    }
    const sorted = ordered(items);
    let shown = sorted.slice(0, RAIL_MAX);
    // Keep the session being viewed on the rail even if it's past the cap.
    if (!shown.some((s) => s.id === currentId)) {
      const current = sorted.find((s) => s.id === currentId);
      if (current) shown = [...shown.slice(0, RAIL_MAX - 1), current];
    }
    list.innerHTML =
      filters.groupBy === "project"
        ? renderGrouped(shown)
        : shown.map((s) => railItemHtml(s, false)).join("");
  }

  function renderGrouped(items: Session[]): string {
    const groups = new Map<string, Session[]>();
    for (const s of items) {
      const arr = groups.get(labelForFolder(s.project));
      if (arr) arr.push(s);
      else groups.set(labelForFolder(s.project), [s]);
    }
    return [...groups.entries()]
      .sort((a, b) => a[0].localeCompare(b[0]))
      .map(([project, rows]) => {
        const isCollapsed = collapsed.has(project);
        const body = isCollapsed ? "" : rows.map((s) => railItemHtml(s, true)).join("");
        return (
          `<button type="button" class="rail-group-head${isCollapsed ? " rail-group-collapsed" : ""}" ` +
          `data-project="${escapeHtml(project)}">` +
          `<span class="rail-group-chevron">▾</span>` +
          `<span class="rail-group-name">${escapeHtml(project)}</span>` +
          `<span class="rail-group-count">${rows.length}</span>` +
          `</button>${body}`
        );
      })
      .join("");
  }

  function railItemHtml(s: Session, grouped: boolean): string {
    const dot = s.running ? `<span class="live-dot live-dot-inline"></span>` : "";
    const cls =
      `rail-item${s.id === currentId ? " rail-item-active" : ""}` +
      `${s.archived ? " rail-item-archived" : ""}`;
    // When grouped the project is in the header, so the item shows only the time.
    const meta = grouped
      ? escapeHtml(formatRelativeTime(s.startedAt))
      : `${escapeHtml(labelForFolder(s.project))} · ${escapeHtml(formatRelativeTime(s.startedAt))}`;
    return `
      <a class="${cls}" href="/claude/session/${encodeURIComponent(s.id)}">
        <div class="rail-item-title">${dot}${escapeHtml(truncate(sessionTitle(s), 34))}</div>
        <div class="rail-item-meta">${meta}</div>
      </a>`;
  }

  // Toggle a project group's collapsed state (clicks on items fall through to
  // their <a> navigation, since those don't match .rail-group-head).
  list.addEventListener("click", (e) => {
    const groupHead = (e.target as HTMLElement).closest<HTMLElement>(".rail-group-head");
    if (!groupHead) return;
    e.preventDefault();
    const project = groupHead.dataset["project"]!;
    if (collapsed.has(project)) collapsed.delete(project);
    else collapsed.add(project);
    saveCollapsed(collapsed);
    render();
  });

  async function fetchAndRender(): Promise<void> {
    try {
      sessions = await getSessions(filters.days, scopeParam, filters.status);
    } catch (err) {
      console.error("failed to load session rail", err);
    }
    render();
  }

  function update(updated: Session): void {
    if (updated.id === currentId) pinned = updated;
    const idx = sessions.findIndex((s) => s.id === updated.id);
    if (idx >= 0) {
      // Drop rows that no longer match the filter (e.g. just archived).
      sessions = matchesFilter(updated)
        ? sessions.map((s, i) => (i === idx ? updated : s))
        : sessions.filter((_, i) => i !== idx);
    } else if (matchesFilter(updated)) {
      sessions = [updated, ...sessions];
    }
    render();
  }

  void fetchAndRender();

  return { el, update, destroy: () => destroyFilterBar?.() };
}
