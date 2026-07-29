// View 7 — ships (route `/project/ships`): the ship history. The board says what you
// meant to do; this page says what actually went out — every recorded
// `make check` / `make release` run across the solo projects (dropped into
// ~/.wyac/ships by scripts/wyac-ship, indexed by ships.go), with whether its
// gates were green. Click a run to read its captured log tail.

import { getShips, subscribeRawEvents } from "../api";
import type { ShipRecord } from "../api";
import { chipAttrs, escapeHtml, formatDuration, formatRelativeTime, truncate } from "../domain/format";
import { showError } from "../app/live";
import { getScope, getScopeSet, labelForFolder, scopeChipHtml } from "../scope";

const DAY_OPTS: { v: number; label: string }[] = [
  { v: 7, label: "7d" },
  { v: 30, label: "30d" },
  { v: 90, label: "90d" },
  { v: 0, label: "all" },
];

// The server clamps at 500; ask for all of it — the tag backfill put the
// window total past 200, and a group scope filters client-side, so a small
// slice starves old projects out of the list entirely.
const LIMIT = 500;

/** Renders the ships view into `container`; returns a cleanup callback. */
export function renderShipsView(container: HTMLElement): () => void {
  let days = 30;
  // The nav's global project scope is the only project filter (a scope change
  // re-renders the whole view, so reading it once is enough).
  // The whole scope set travels to the server comma-separated: filtering
  // client-side over the capped newest-first slice starved a small project's
  // old records out of the list entirely once the window total passed the cap.
  const scope = getScope();
  const scopeSet = getScopeSet();
  const project = scopeSet ? [...scopeSet].join(",") : "";
  let records: ShipRecord[] = [];
  let total = 0;
  const gatesOn = new Set<string>(); // green | failed; empty = both
  const kindOn = new Set<string>(); // check | release; empty = both
  let expanded: string | null = null; // file key of the open row
  // Logs are fetched lazily on first expand and remembered per file — the
  // list payload stays light, a re-click costs nothing.
  const logs = new Map<string, string>();

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          ${scopeChipHtml()}
          <div class="filter-row" id="sh-days"></div>
          <div class="filter-row" id="sh-gates"></div>
          <div class="filter-row" id="sh-kind"></div>
        </div>
      </header>
      <section class="card ships-card">
        <div id="sh-list"><div class="empty-state">loading…</div></div>
      </section>
    </div>
  `;

  const daysEl = container.querySelector<HTMLElement>("#sh-days")!;
  const gatesEl = container.querySelector<HTMLElement>("#sh-gates")!;
  const kindEl = container.querySelector<HTMLElement>("#sh-kind")!;
  const listEl = container.querySelector<HTMLElement>("#sh-list")!;

  function renderDays(): void {
    daysEl.innerHTML = DAY_OPTS.map(
      (o) =>
        `<button type="button" ${chipAttrs(o.v === days)} ` +
        `data-days="${o.v}">${escapeHtml(o.label)}</button>`,
    ).join("");
  }
  renderDays();

  // Gates and kind narrow the loaded window client-side — unlike `days`, which is
  // the server query. Nothing on = nothing filtered; both chips in a group on is
  // the same as neither. The failures are the point: 3 red runs in ~100 is exactly
  // the needle you'd otherwise scroll for.
  function renderChips(): void {
    const chip = (attr: string, key: string, label: string, on: boolean): string =>
      `<button type="button" ${chipAttrs(on)} ${attr}="${key}">${label}</button>`;
    gatesEl.innerHTML =
      chip("data-gate", "green", "green", gatesOn.has("green")) +
      chip("data-gate", "failed", "failed", gatesOn.has("failed"));
    kindEl.innerHTML =
      chip("data-kind", "check", "check", kindOn.has("check")) +
      chip("data-kind", "release", "release", kindOn.has("release"));
  }
  renderChips();

  /** The records left after the client-side chips. */
  function visible(): ShipRecord[] {
    return records.filter((r) => {
      if (gatesOn.size && !gatesOn.has(r.exit === 0 ? "green" : "failed")) return false;
      if (kindOn.size && !kindOn.has(r.kind)) return false;
      return true;
    });
  }

  async function load(): Promise<void> {
    try {
      const res = await getShips(days, project || undefined, LIMIT);
      records = res.ships;
      if (scopeSet && scopeSet.size > 1) {
        records = records.filter((r) => scopeSet.has(labelForFolder(r.project)));
      }
      total = res.total;
      renderList();
    } catch (err) {
      showError(listEl, "failed to load ship history", () => void load());
      console.error("ships load failed", err);
    }
  }

  function rowHtml(r: ShipRecord): string {
    const exitBadge =
      r.exit === 0
        ? `<span class="ship-exit ship-exit--ok">green</span>`
        : `<span class="ship-exit ship-exit--fail">exit ${r.exit}</span>`;
    const open = expanded === r.file;
    const logHtml = open
      ? `<tr class="ship-log"><td colspan="7"><pre>${escapeHtml(logs.get(r.file) ?? "loading log…")}</pre></td></tr>`
      : "";
    const sessionCell = r.sessionId
      ? `<a class="ship-sess" href="/claude/session/${encodeURIComponent(r.sessionId)}">${escapeHtml(
          truncate(r.sessionTitle || "untitled session", 24),
        )}</a>`
      : "—";
    return `
      <tr class="ship-row${open ? " ship-row--open" : ""}" data-file="${escapeHtml(r.file)}">
        <td title="${escapeHtml(r.ts)}">${escapeHtml(formatRelativeTime(r.ts))}</td>
        <td>${escapeHtml(labelForFolder(r.project))}</td>
        <td><span class="ship-kind">${escapeHtml(r.kind)}</span></td>
        <td>${r.version ? escapeHtml(r.version) : "—"}</td>
        <td>${exitBadge}</td>
        <td>${escapeHtml(formatDuration(r.durationMs))}</td>
        <td>${sessionCell}</td>
      </tr>${logHtml}`;
  }

  function renderList(): void {
    if (records.length === 0) {
      // "Empty" has three shapes and only one of them means nothing ever ran:
      // no records anywhere (say how to start recording); a scope whose window
      // holds other projects' runs (say how many, or the scoped view reads as
      // a broken install — the git-tab rule: a filtered list must never read
      // as an empty repo); and a scope with genuinely nothing in the window
      // (name the scope, so "empty" is a fact about it, not about the app).
      const hint = `<code>make check</code> / <code>make release</code> wrapped in scripts/wyac-ship drop records into ~/.wyac/ships`;
      listEl.innerHTML = scope
        ? `<div class="empty-state">no ship records for ${escapeHtml(scope)} in this window${
            total > 0 ? ` — ${total} run${total === 1 ? "" : "s"} recorded in this window overall` : ""
          } · ${hint}.</div>`
        : `<div class="empty-state">no ship records in this window — ${hint}.</div>`;
      return;
    }
    const rows = visible();
    // Two different reasons the list can be short — the server window, and the
    // chips. Say both, so a filtered list never reads as "nothing ever ran".
    const windowNote =
      records.length < total ? `showing ${records.length} of ${total} runs` : `${total} run${total === 1 ? "" : "s"}`;
    const filterNote = rows.length < records.length ? ` — ${records.length - rows.length} filtered out` : "";
    if (rows.length === 0) {
      listEl.innerHTML = `
        <div class="ships-meta">${windowNote}${filterNote}</div>
        <div class="empty-state">no runs match the filters — drop a chip to see them again.</div>`;
      return;
    }
    listEl.innerHTML = `
      <div class="ships-meta">${windowNote}${filterNote}</div>
      <div class="table-scroll">
        <table class="sessions-table ships-table">
          <thead><tr><th>when</th><th>project</th><th>kind</th><th>version</th><th>gates</th><th>took</th><th>session</th></tr></thead>
          <tbody>${rows.map(rowHtml).join("")}</tbody>
        </table>
      </div>
    `;
  }

  async function toggle(file: string): Promise<void> {
    expanded = expanded === file ? null : file;
    renderList();
    if (expanded === file && !logs.has(file)) {
      try {
        // One fetch fills the cache for the whole window — the next expands
        // are free until the filters change the window.
        const res = await getShips(days, project || undefined, LIMIT, true);
        for (const r of res.ships) logs.set(r.file, r.log ?? "");
      } catch (err) {
        logs.set(file, "failed to load log");
        console.error("ship log load failed", err);
      }
      if (expanded === file) renderList();
    }
  }

  listEl.addEventListener("click", (e) => {
    if ((e.target as HTMLElement).closest("a")) return; // the session link navigates, not toggles
    const row = (e.target as HTMLElement).closest<HTMLTableRowElement>(".ship-row");
    if (row?.dataset["file"]) void toggle(row.dataset["file"]);
  });
  daysEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-days]");
    if (!btn) return;
    days = Number(btn.dataset["days"]);
    logs.clear(); // the cached window changed with the filter
    renderDays();
    void load();
  });
  // Gates/kind narrow what's already loaded, so they re-render instead of
  // re-fetching — and the log cache stays valid.
  function toggleChip(set: Set<string>, key: string): void {
    if (set.has(key)) set.delete(key);
    else set.add(key);
    renderChips();
    renderList();
  }
  gatesEl.addEventListener("click", (e) => {
    const k = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-gate]")?.dataset["gate"];
    if (k) toggleChip(gatesOn, k);
  });
  kindEl.addEventListener("click", (e) => {
    const k = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-kind]")?.dataset["kind"];
    if (k) toggleChip(kindOn, k);
  });

  // A run finishing anywhere shows up here without a refresh — the server
  // broadcasts ship-recorded when a drop file lands.
  const unsubscribe = subscribeRawEvents((type) => {
    if (type === "ship-recorded") void load();
  });

  void load();
  return unsubscribe;
}
