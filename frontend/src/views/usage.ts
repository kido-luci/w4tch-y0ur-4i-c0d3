// View 1b — usage (route `/claude/<scope>/usage`): what the sessions COST and
// when they happened. It used to sit on top of the sessions list, which made
// that route two pages stacked: a dashboard you read, and a list you search.
// They answer different questions and are looked at at different moments, so
// they are two tabs now. The list keeps what is per-session; everything that
// aggregates lives here.

import { getActivity, getSessions, getStats, subscribeSessionEvents } from "../api";
import type { ModelUsage, Session, Stats } from "../api";
import { formatCost, formatTokens } from "../domain/format";
import { showError } from "../app/live";
import { renderModelDistribution } from "../domain/distribution";
import { renderActivityHeatmap } from "../ui/heatmap";
import { createTokensOverTime } from "../ui/tokensOverTime";
import { loadFilters, renderFilterBar, saveFilters } from "../domain/filters";
import type { FilterState } from "../domain/filters";
import { getScopeParam, scopeChipHtml } from "../scope";

// The heatmap uses its own fixed window, independent of the day filter.
const HEATMAP_WEEKS = 26;

// A running session emits an SSE tick per tool use, and every number here is a
// fetch — so events schedule ONE trailing refresh instead of firing four
// requests each. The list view next door needs no fetch at all on a tick: it
// patches the row it already holds.
const SSE_REFRESH_MS = 1500;

/** Renders the usage dashboard into `container`; returns a cleanup callback. */
export function renderUsageView(container: HTMLElement): () => void {
  let filters: FilterState = loadFilters();
  // The scope is read once: a scope change re-renders the whole view.
  const scopeParam = getScopeParam();
  let unsubscribe: (() => void) | null = null;
  let filterBar: { el: HTMLElement; destroy: () => void } | null = null;
  let pending: ReturnType<typeof setTimeout> | null = null;
  // Set in cleanup. Anything that resolves after teardown must bail: rendering
  // into detached nodes is merely wasted, but building the filter bar late
  // registers a document-level click listener whose destroy() would never run.
  let disposed = false;

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          ${scopeChipHtml()}
          <div id="filter-slot"></div>
        </div>
      </header>

      <section class="stat-cards" id="stat-cards"></section>
      <div class="summary-row">
        <section class="card heatmap-card">
          <h2 class="section-heading">activity · last ${HEATMAP_WEEKS} weeks</h2>
          <p class="panel-scope">this scope · fixed ${HEATMAP_WEEKS}-week window, ignores the day filter</p>
          <div id="heatmap-slot"></div>
        </section>
        <section class="card">
          <h2 class="section-heading">model distribution</h2>
          <p class="panel-scope">this scope · follows every filter above</p>
          <div id="dist-slot"></div>
        </section>
      </div>
      <section class="card tot-card" id="tot-card"></section>
    </div>
  `;

  const filterSlot = container.querySelector<HTMLElement>("#filter-slot")!;
  const statCardsEl = container.querySelector<HTMLElement>("#stat-cards")!;
  const heatmapSlotEl = container.querySelector<HTMLElement>("#heatmap-slot")!;
  const totCardEl = container.querySelector<HTMLElement>("#tot-card")!;
  const distSlotEl = container.querySelector<HTMLElement>("#dist-slot")!;

  const tokensOverTime = createTokensOverTime();
  totCardEl.append(tokensOverTime.el);

  function onFilterChange(next: FilterState): void {
    filters = next;
    saveFilters(next); // shared with the list view — one filter state, two tabs
    void refresh();
  }

  filterBar = renderFilterBar(filters, onFilterChange);
  filterSlot.replaceChildren(filterBar.el);

  async function refresh(): Promise<void> {
    try {
      // The chart is global all-time usage, independent of every filter (day /
      // scope / status), so it gets its own unfiltered fetch. The distribution
      // is the opposite: it follows the filters, so it rides the scoped list.
      const [scoped, chartSessions, stats, activity] = await Promise.all([
        getSessions(filters.days, scopeParam, filters.status),
        getSessions(0),
        getStats(filters.days, scopeParam, filters.status),
        getActivity(HEATMAP_WEEKS, scopeParam),
      ]);
      if (disposed) return; // navigated away mid-fetch
      renderStatCards(statCardsEl, stats);
      heatmapSlotEl.replaceChildren(renderActivityHeatmap(activity));
      distSlotEl.replaceChildren(renderModelDistribution(aggregateBreakdown(scoped)));
      tokensOverTime.update(chartSessions, 0);
    } catch (err) {
      showError(statCardsEl, "failed to load usage", () => void refresh());
      console.error("failed to load usage", err);
    }
  }

  void refresh();

  unsubscribe = subscribeSessionEvents(() => {
    // Which session changed does not matter — every panel here is an aggregate.
    if (pending) return;
    pending = setTimeout(() => {
      pending = null;
      if (!disposed) void refresh();
    }, SSE_REFRESH_MS);
  });

  return () => {
    disposed = true;
    if (pending) clearTimeout(pending);
    unsubscribe?.();
    filterBar?.destroy();
  };
}

/** Sum each session's per-model breakdown into one distribution. */
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
