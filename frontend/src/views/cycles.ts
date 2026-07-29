// View 9 — cycles (route `/project/<scope>/cycles`): the board's sprints, and
// the two reports that make sizing cards worth the effort.
//
// This is a TAB, not a fourth shape in the board's view switcher, because it is
// not a view of cards: board / table / timeline all draw the same card list
// differently, while this manages the windows those cards are planned into and
// draws history. Keeping it out of the switcher also keeps a saved view's
// `kind` meaning exactly "how to draw the cards".
//
// It exists because the backend shipped ahead of it: cycles, estimates and a
// burndown endpoint all landed with no way to reach them, so a card's `points`
// field led nowhere and the panel's cycle picker was permanently empty. Nothing
// here is new server-side — it is the missing half.

import {
  createCycle,
  deleteCycle,
  getBoardEvents,
  getBoardStates,
  getBurndown,
  getTodos,
  getVelocity,
  patchCycle,
  subscribeRawEvents,
} from "../api";
import type { Burndown, CycleReport, Todo, TodoEvent, TodoState } from "../api";
import { describeEventOrCreated } from "../boardEvents";
import { escapeHtml, formatDay, formatRelativeTime } from "../format";
import { showError } from "../live";
import { getScope, getScopeSet, scopeChipHtml } from "../scope";

function points(v: number): string {
  return Number.isInteger(v) ? String(v) : v.toFixed(1);
}

/**
 * The burndown chart: remaining points per day against the ideal line.
 *
 * Hand-rolled SVG in the same spirit as this app's other charts. `viewBox` +
 * `preserveAspectRatio="none"` lets it stretch to the card width, so the y
 * scale is the only thing that has to be computed.
 */
function burndownSvg(bd: Burndown): string {
  const pts = bd.points;
  if (pts.length === 0) {
    return `<div class="empty-state">the cycle hasn't started yet</div>`;
  }
  const w = 100;
  const h = 40;
  const max = Math.max(...pts.map((p) => Math.max(p.remaining, p.ideal)), 1);
  // A single-day cycle would divide by zero; clamp the span, not the data.
  const dx = pts.length > 1 ? w / (pts.length - 1) : 0;
  const y = (v: number) => h - (v / max) * h;
  const path = (get: (i: number) => number) =>
    pts.map((_, i) => `${i === 0 ? "M" : "L"}${(i * dx).toFixed(2)},${y(get(i)).toFixed(2)}`).join(" ");

  const remaining = path((i) => pts[i]!.remaining);
  const ideal = path((i) => pts[i]!.ideal);
  const dots = pts
    .map(
      (p, i) =>
        `<circle class="cy-dot" cx="${(i * dx).toFixed(2)}" cy="${y(p.remaining).toFixed(2)}" r="0.9">` +
        `<title>${escapeHtml(p.date)} — ${points(p.remaining)} left of ${points(p.total)}` +
        `${p.done ? `, ${points(p.done)} done` : ""}</title></circle>`,
    )
    .join("");
  return `
    <svg class="cy-chart" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img"
      aria-label="burndown: points remaining per day against the ideal line">
      <path class="cy-ideal" d="${ideal}" />
      <path class="cy-remaining" d="${remaining}" />
      ${dots}
    </svg>
    <div class="cy-axis">
      <span>${escapeHtml(pts[0]!.date)}</span>
      <span class="cy-axis-max">peak ${points(max)} pts</span>
      <span>${escapeHtml(pts[pts.length - 1]!.date)}</span>
    </div>`;
}

/** Renders the cycles view into `container`; returns a cleanup callback. */
export function renderCyclesView(container: HTMLElement): () => void {
  let rows: CycleReport[] = [];
  let todos: Todo[] = [];
  let states: TodoState[] = [];
  let selectedId: string | null = null;
  let burndown: Burndown | null = null;
  let creating = false;
  let events: TodoEvent[] = [];
  const scope = getScope();
  const scopeSet = getScopeSet();

  container.innerHTML = `
    <div class="page">
      <header class="topbar"><div class="topbar-controls">${scopeChipHtml()}</div></header>
      <div class="section-heading">cycles</div>
      <div class="section-desc">
        Sprints the board's cards are planned into. A cycle's burndown reads the
        card history, so it only knows what actually moved — cards carrying no
        points are counted separately rather than quietly left out of the chart.
      </div>
      <div id="cycles-body"><div class="empty-state">loading…</div></div>
    </div>
  `;
  const bodyEl = container.querySelector<HTMLElement>("#cycles-body")!;

  const doneIds = (): Set<string> =>
    new Set(states.filter((s) => s.category === "done").map((s) => s.id));

  /** Cards in this scope with no cycle — the pool a new cycle draws from. */
  function unplanned(): Todo[] {
    const done = doneIds();
    return todos.filter(
      (t) => (!scopeSet || (!!t.repo && scopeSet.has(t.repo))) && !t.cycleId && !done.has(t.status),
    );
  }

  function createFormHtml(): string {
    if (!creating) {
      return `<button type="button" class="todo-btn cy-new">+ new cycle</button>`;
    }
    const today = new Date();
    const in2w = new Date(today.getTime() + 13 * 86_400_000);
    return `
      <form class="cy-form">
        <input class="cy-f-name" placeholder="name (e.g. Sprint 1)" autocomplete="off" required>
        <input class="cy-f-goal" placeholder="goal (optional)" autocomplete="off">
        <label>starts <input class="cy-f-start" type="date" value="${formatDay(today.toISOString())}" required></label>
        <label>ends <input class="cy-f-end" type="date" value="${formatDay(in2w.toISOString())}" required></label>
        <button type="submit" class="todo-btn">create</button>
        <button type="button" class="todo-btn cy-cancel">cancel</button>
      </form>`;
  }

  function rowHtml(r: CycleReport): string {
    const c = r.cycle;
    const pct = r.points ? Math.round((r.pointsDone / r.points) * 100) : 0;
    const open = !c.closedAt;
    const overdue = open && new Date(c.endsAt) < new Date();
    return `
      <div class="cy-row${c.id === selectedId ? " selected" : ""}" data-id="${escapeHtml(c.id)}">
        <div class="cy-row-head">
          <span class="cy-name">${escapeHtml(c.name)}</span>
          ${open ? "" : `<span class="cy-badge" title="closed ${escapeHtml(formatRelativeTime(c.closedAt!))}">closed</span>`}
          ${overdue ? `<span class="cy-badge cy-badge-late">past its end date</span>` : ""}
          <span class="cy-dates">${escapeHtml(formatDay(c.startsAt))} → ${escapeHtml(
            formatDay(c.endsAt),
          )}</span>
          <span class="cy-actions">
            <button type="button" class="todo-btn cy-toggle">${open ? "close" : "reopen"}</button>
            <button type="button" class="todo-btn todo-btn-danger cy-del" title="delete this cycle; its cards stay on the board">✕</button>
          </span>
        </div>
        ${c.goal ? `<div class="cy-goal">${escapeHtml(c.goal)}</div>` : ""}
        <div class="cy-stats">
          <span><b>${r.cardsDone}</b>/${r.cards} cards</span>
          <span><b>${points(r.pointsDone)}</b>/${points(r.points)} pts</span>
          ${
            r.unestimated
              ? `<span class="cy-warn" title="the burndown cannot see these">${r.unestimated} unestimated</span>`
              : ""
          }
        </div>
        <div class="cy-bar"><i style="width:${pct}%"></i></div>
      </div>`;
  }

  function detailHtml(): string {
    if (!selectedId) {
      return `<div class="empty-state">pick a cycle to see its burndown</div>`;
    }
    const r = rows.find((x) => x.cycle.id === selectedId);
    if (!r) return `<div class="empty-state">that cycle is gone</div>`;
    if (!burndown) return `<div class="empty-state">loading the burndown…</div>`;
    return `
      <div class="card cy-detail">
        <div class="web-subheading">${escapeHtml(r.cycle.name)} — burndown</div>
        ${burndownSvg(burndown)}
        <div class="cy-legend">
          <span class="cy-key cy-key-remaining">remaining</span>
          <span class="cy-key cy-key-ideal">ideal</span>
          ${
            burndown.unestimated
              ? `<span class="cy-warn">${burndown.unestimated} card${
                  burndown.unestimated > 1 ? "s carry" : " carries"
                } no points — the chart is blind to ${burndown.unestimated > 1 ? "them" : "it"}</span>`
              : ""
          }
        </div>
      </div>`;
  }

  /**
   * What moved lately, board-wide. This is the other half of the event log's
   * reason for existing: the burndown consumes it as numbers, this reads it as
   * a sentence — "what happened this sprint" without opening seven cards.
   *
   * Scoped by the CARD's repo, not the event's, because an event stores only a
   * card id; a card since deleted is skipped rather than rendered as an orphan.
   */
  function activityHtml(): string {
    const names = {
      state: (id: string): string => states.find((x) => x.id === id)?.name ?? id ?? "—",
      cycle: (id: string): string =>
        id ? (rows.find((r) => r.cycle.id === id)?.cycle.name ?? id) : "no cycle",
      card: (id: string): string => {
        const c = todos.find((x) => x.id === id);
        return c ? `#${c.seq}` : "a card";
      },
    };
    const byId = new Map(todos.map((t) => [t.id, t]));
    const inScope = (t: Todo | undefined): boolean =>
      !!t && (!scopeSet || (!!t.repo && scopeSet.has(t.repo)));

    const items = events
      .map((e) => {
        const card = byId.get(e.todoId);
        if (!inScope(card)) return null;
        const body = describeEventOrCreated(e, names);
        if (!body) return null;
        return `
          <div class="ev-row" title="${escapeHtml(e.ts)}">
            <span class="ev-when">${escapeHtml(formatRelativeTime(e.ts))}</span>
            <a class="ev-card" href="/project/board/${escapeHtml(e.todoId)}">#${card!.seq}</a>
            <span class="ev-what">${body}</span>
          </div>`;
      })
      .filter((x): x is string => x !== null)
      .slice(0, 40);

    if (!items.length) {
      return `
        <div class="card cy-activity">
          <div class="web-subheading">recent activity</div>
          <div class="empty-state">nothing has moved in this scope yet</div>
        </div>`;
    }
    return `
      <div class="card cy-activity">
        <div class="web-subheading">recent activity</div>
        ${items.join("")}
      </div>`;
  }

  function render(): void {
    const pool = unplanned();
    bodyEl.innerHTML = `
      <div class="cy-toolbar">${createFormHtml()}</div>
      ${
        rows.length === 0
          ? `<div class="empty-state">no cycles yet — create one, then set a card's cycle in its board panel</div>`
          : `<div class="cy-list">${rows.map(rowHtml).join("")}</div>`
      }
      ${detailHtml()}
      ${activityHtml()}
      <div class="cy-pool">${pool.length} open card${
        pool.length === 1 ? " is" : "s are"
      } in this scope with no cycle${
        pool.length ? ` — set one from a card's panel on the board` : ""
      }</div>
    `;
    wire();
  }

  function wire(): void {
    bodyEl.querySelector<HTMLButtonElement>(".cy-new")?.addEventListener("click", () => {
      creating = true;
      render();
      bodyEl.querySelector<HTMLInputElement>(".cy-f-name")?.focus();
    });
    bodyEl.querySelector<HTMLButtonElement>(".cy-cancel")?.addEventListener("click", () => {
      creating = false;
      render();
    });
    bodyEl.querySelector<HTMLFormElement>(".cy-form")?.addEventListener("submit", (e) => {
      e.preventDefault();
      const q = <E extends HTMLElement>(s: string): E => bodyEl.querySelector<E>(s)!;
      const name = q<HTMLInputElement>(".cy-f-name").value.trim();
      const start = q<HTMLInputElement>(".cy-f-start").value;
      const end = q<HTMLInputElement>(".cy-f-end").value;
      if (!name || !start || !end) return;
      // Local midnight, then ISO — a bare YYYY-MM-DD parses as UTC, which
      // shifts the window a day for anyone east of Greenwich.
      const at = (d: string, endOfDay = false) =>
        new Date(`${d}T${endOfDay ? "23:59:59" : "00:00:00"}`).toISOString();
      createCycle({
        name,
        goal: q<HTMLInputElement>(".cy-f-goal").value.trim() || undefined,
        repo: scope || undefined,
        startsAt: at(start),
        endsAt: at(end, true),
      })
        .then((c) => {
          creating = false;
          selectedId = c.id;
          return refresh();
        })
        .catch((err: unknown) => alert(err instanceof Error ? err.message : "could not create the cycle"));
    });

    bodyEl.querySelectorAll<HTMLElement>(".cy-row").forEach((row) => {
      const id = row.dataset["id"]!;
      row.addEventListener("click", (e) => {
        if ((e.target as HTMLElement).closest(".cy-actions")) return;
        selectedId = id;
        burndown = null;
        render();
        void loadBurndown();
      });
      row.querySelector<HTMLButtonElement>(".cy-toggle")!.addEventListener("click", () => {
        const r = rows.find((x) => x.cycle.id === id);
        patchCycle(id, { closed: !r?.cycle.closedAt })
          .then(refresh)
          .catch((err: unknown) => alert(err instanceof Error ? err.message : "update failed"));
      });
      row.querySelector<HTMLButtonElement>(".cy-del")!.addEventListener("click", () => {
        const r = rows.find((x) => x.cycle.id === id);
        // Deleting a cycle is not deleting work — say so, because "delete" next
        // to a card count reads like it takes the cards with it.
        if (
          !confirm(
            `Delete the cycle "${r?.cycle.name ?? ""}"?\n\nIts ${r?.cards ?? 0} card(s) stay on the board — they just stop being planned into a cycle.`,
          )
        ) {
          return;
        }
        deleteCycle(id)
          .then(() => {
            if (selectedId === id) {
              selectedId = null;
              burndown = null;
            }
            return refresh();
          })
          .catch((err: unknown) => alert(err instanceof Error ? err.message : "delete failed"));
      });
    });
  }

  async function loadBurndown(): Promise<void> {
    if (!selectedId) return;
    const want = selectedId;
    try {
      const bd = await getBurndown(want);
      if (want !== selectedId) return; // a newer pick already went out
      burndown = bd;
      render();
    } catch (err) {
      console.error("failed to load the burndown", err);
      if (want === selectedId) {
        burndown = { cycleId: want, points: [], cards: 0, cardsDone: 0, unestimated: 0 };
        render();
      }
    }
  }

  async function refresh(): Promise<void> {
    try {
      const [v, t, st, ev] = await Promise.all([
        getVelocity(scope || undefined),
        getTodos(),
        getBoardStates(scope || undefined),
        // The feed is an extra: a failure here must not blank the cycle list.
        getBoardEvents(200).catch((): TodoEvent[] => []),
      ]);
      rows = v;
      todos = t;
      states = st;
      events = ev ?? []; // nil Go slice arrives as null — the api/ convention
      // A cycle deleted elsewhere must not leave a dead selection behind.
      if (selectedId && !rows.some((r) => r.cycle.id === selectedId)) {
        selectedId = null;
        burndown = null;
      }
      // With exactly one cycle there is nothing to pick between, and "pick a
      // cycle to see its burndown" was a click you had to make every visit to
      // reach the only answer. Only for one: with several, choosing for you
      // would be guessing.
      if (!selectedId && rows.length === 1) selectedId = rows[0]!.cycle.id;
      render();
      if (selectedId && !burndown) void loadBurndown();
    } catch (err) {
      showError(bodyEl, "failed to load cycles", () => void refresh());
      console.error("failed to load cycles", err);
    }
  }

  void refresh();

  // A card moved on the board changes these totals, so listen for it too.
  const unsubscribe = subscribeRawEvents((type) => {
    if (type === "cycles-updated" || type === "todos-updated" || type === "board-states-updated") {
      // Don't yank a half-typed create form out from under the user.
      if (creating && document.activeElement && bodyEl.contains(document.activeElement)) return;
      void refresh();
    }
  });

  return unsubscribe;
}
