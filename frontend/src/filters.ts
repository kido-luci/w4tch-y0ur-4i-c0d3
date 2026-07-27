// Shared filter state for the sessions view, plus a Claude-Code-style filter
// panel. State is persisted to localStorage so it survives reloads and the
// detail rail can honor the chosen status.

import { escapeHtml } from "./format";

export type StatusFilter = "active" | "archived" | "all";
export type GroupBy = "none" | "project";
export type SortBy = "recent" | "alpha";

export interface FilterState {
  status: StatusFilter;
  days: number; // 0 = all time
  groupBy: GroupBy;
  sortBy: SortBy;
}

export const DEFAULT_FILTERS: FilterState = {
  status: "active",
  days: 7,
  groupBy: "none",
  sortBy: "recent",
};

const STORAGE_KEY = "wyac.filters";

export function loadFilters(): FilterState {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (raw) {
      // Pick known keys only: blobs written before the scope rail took over
      // project selection still carry a `project`, and a blind spread would
      // keep re-persisting it. Re-save so the stale key actually goes away.
      const saved = JSON.parse(raw) as Partial<FilterState>;
      const state: FilterState = {
        status: saved.status ?? DEFAULT_FILTERS.status,
        days: saved.days ?? DEFAULT_FILTERS.days,
        groupBy: saved.groupBy ?? DEFAULT_FILTERS.groupBy,
        sortBy: saved.sortBy ?? DEFAULT_FILTERS.sortBy,
      };
      saveFilters(state);
      return state;
    }
  } catch {
    /* corrupt or unavailable storage — fall back to defaults */
  }
  return { ...DEFAULT_FILTERS };
}

export function saveFilters(state: FilterState): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(state));
  } catch {
    /* storage unavailable — filters just won't persist */
  }
}

interface Opt {
  v: string | number;
  label: string;
}

const STATUS_OPTS: Opt[] = [
  { v: "active", label: "Active" },
  { v: "archived", label: "Archived" },
  { v: "all", label: "All" },
];
const DAY_OPTS: Opt[] = [
  { v: 1, label: "today" },
  { v: 7, label: "7d" },
  { v: 30, label: "30d" },
  { v: 0, label: "all" },
];
const GROUP_OPTS: Opt[] = [
  { v: "none", label: "None" },
  { v: "project", label: "Project" },
];
const SORT_OPTS: Opt[] = [
  { v: "recent", label: "Recent" },
  { v: "alpha", label: "Alphabetical" },
];

// feather "sliders" — the filter affordance, matching Claude Code's panel icon.
const SLIDERS_SVG =
  `<svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" ` +
  `stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` +
  `<line x1="4" x2="4" y1="21" y2="14"/><line x1="4" x2="4" y1="10" y2="3"/>` +
  `<line x1="12" x2="12" y1="21" y2="12"/><line x1="12" x2="12" y1="8" y2="3"/>` +
  `<line x1="20" x2="20" y1="21" y2="16"/><line x1="20" x2="20" y1="12" y2="3"/>` +
  `<line x1="1" x2="7" y1="14" y2="14"/><line x1="9" x2="15" y1="8" y2="8"/>` +
  `<line x1="17" x2="23" y1="16" y2="16"/></svg>`;

export interface FilterBarOptions {
  align?: "left" | "right"; // which edge the dropdown aligns to (default right)
  compact?: boolean; // icon-only button (for the narrow detail rail)
  resetTo?: FilterState; // what "Clear filters" resets to (default DEFAULT_FILTERS)
}

function chipRow(kind: string, opts: Opt[], current: string | number): string {
  return `<div class="filter-row">${opts
    .map(
      (o) =>
        `<button type="button" class="filter-chip${o.v === current ? " filter-chip-on" : ""}" ` +
        `data-kind="${kind}" data-val="${escapeHtml(String(o.v))}">${escapeHtml(o.label)}</button>`,
    )
    .join("")}</div>`;
}

/**
 * Renders the "Filters" button + dropdown panel. `onChange` fires with the full
 * next state on every selection. Returns the element and a `destroy` that
 * detaches the outside-click listener.
 */
export function renderFilterBar(
  initial: FilterState,
  onChange: (next: FilterState) => void,
  opts: FilterBarOptions = {},
): { el: HTMLElement; destroy: () => void } {
  let state = initial;

  const wrap = document.createElement("div");
  wrap.className = "filter-bar";
  const btn = document.createElement("button");
  btn.type = "button";
  btn.className = opts.compact ? "filter-btn filter-btn-compact" : "filter-btn";
  const panel = document.createElement("div");
  panel.className =
    opts.align === "left" ? "filter-panel filter-panel-left hidden" : "filter-panel hidden";
  wrap.append(btn, panel);

  function summary(): string {
    const bits = [
      STATUS_OPTS.find((o) => o.v === state.status)?.label ?? "All",
      DAY_OPTS.find((o) => o.v === state.days)?.label ?? `${state.days}d`,
    ];
    return bits.join(" · ");
  }

  function renderBtn(): void {
    const icon = `<span class="filter-btn-icon">${SLIDERS_SVG}</span>`;
    if (opts.compact) {
      btn.innerHTML = icon;
      btn.title = summary();
    } else {
      btn.innerHTML = `${icon}<span>${escapeHtml(summary())}</span>`;
    }
  }

  function renderPanel(): void {
    panel.innerHTML = `
      <div class="filter-section"><div class="filter-label">Status</div>${chipRow("status", STATUS_OPTS, state.status)}</div>
      <div class="filter-section"><div class="filter-label">Last activity</div>${chipRow("days", DAY_OPTS, state.days)}</div>
      <hr class="filter-div" />
      <div class="filter-section"><div class="filter-label">Group by</div>${chipRow("groupBy", GROUP_OPTS, state.groupBy)}</div>
      <div class="filter-section"><div class="filter-label">Sort by</div>${chipRow("sortBy", SORT_OPTS, state.sortBy)}</div>
      <hr class="filter-div" />
      <button type="button" class="filter-clear" data-kind="clear">Clear filters</button>
    `;
  }

  function apply(patch: Partial<FilterState>): void {
    state = { ...state, ...patch };
    onChange(state);
    renderPanel();
    renderBtn();
  }

  panel.addEventListener("click", (e) => {
    // Keep the panel open across selections: without this the outside-click
    // handler would fire (apply() re-renders the panel, detaching the clicked
    // node, so document sees the click as "outside") and close it.
    e.stopPropagation();
    const el = (e.target as HTMLElement).closest<HTMLElement>("[data-kind]");
    if (!el) return;
    const kind = el.dataset["kind"]!;
    if (kind === "clear") {
      apply({ ...(opts.resetTo ?? DEFAULT_FILTERS) });
      return;
    }
    const raw = el.dataset["val"] ?? "";
    if (kind === "days") apply({ days: Number(raw) });
    else if (kind === "status") apply({ status: raw as StatusFilter });
    else if (kind === "groupBy") apply({ groupBy: raw as GroupBy });
    else if (kind === "sortBy") apply({ sortBy: raw as SortBy });
  });

  btn.addEventListener("click", (e) => {
    e.stopPropagation();
    panel.classList.toggle("hidden");
  });
  const onDocClick = (e: MouseEvent): void => {
    if (!wrap.contains(e.target as Node)) panel.classList.add("hidden");
  };
  document.addEventListener("click", onDocClick);

  renderBtn();
  renderPanel();

  return { el: wrap, destroy: () => document.removeEventListener("click", onDocClick) };
}
