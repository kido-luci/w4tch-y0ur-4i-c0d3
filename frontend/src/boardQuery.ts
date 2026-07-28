// The board's filter vocabulary, and the timeline shape.
//
// matchesQuery is the ONE place a BoardQuery is interpreted. The server stores
// a saved view's query as opaque JSON precisely so this file can grow a filter
// without a migration — which only holds while every renderer routes through
// here rather than reimplementing "does this card match".

import type { BoardQuery, Cycle, Todo, TodoState } from "./api";
import { escapeHtml } from "./format";

/** Does one card survive the filter? An absent field filters nothing. */
export function matchesQuery(t: Todo, q: BoardQuery): boolean {
  if (q.text) {
    const needle = q.text.toLowerCase();
    const hay = `${t.title} ${t.note ?? ""} #${t.seq}`.toLowerCase();
    if (!hay.includes(needle)) return false;
  }
  if (q.kinds?.length && !q.kinds.includes(t.kind ?? "task")) return false;
  if (q.statuses?.length && !q.statuses.includes(t.status)) return false;
  if (q.labels?.length) {
    const own = t.labels ?? [];
    if (!q.labels.some((l) => own.includes(l))) return false;
  }
  if (q.cycleId && t.cycleId !== q.cycleId) return false;
  if (q.minPriority && (t.priority ?? 0) < q.minPriority) return false;
  // "Unestimated" is the planning gap worth finding: a card with no points is
  // invisible to every burndown, so the filter that surfaces them is the one
  // that makes a cycle plannable.
  if (q.unestimatedOnly && (t.estimate ?? 0) > 0) return false;
  return true;
}

const DAY_MS = 86_400_000;

function dayIndex(from: Date, at: Date): number {
  return Math.floor((at.getTime() - from.getTime()) / DAY_MS);
}

/**
 * The timeline: one row per cycle, cards laid out inside their cycle's window,
 * plus an "unplanned" row for everything with no cycle.
 *
 * A card carries no dates of its own — only a cycle does — so a bar spans its
 * cycle and is shaded by progress rather than pretending to know when the work
 * itself starts. Inventing per-card dates from the event log was the
 * alternative, and it would draw a Gantt chart out of guesses.
 */
export function renderTimeline(todos: Todo[], cycles: Cycle[], states: TodoState[]): string {
  const doneIds = new Set(states.filter((s) => s.category === "done").map((s) => s.id));
  const planned = cycles.filter((c) => todos.some((t) => t.cycleId === c.id));
  const unplanned = todos.filter((t) => !t.cycleId || !cycles.some((c) => c.id === t.cycleId));

  if (!planned.length && !unplanned.length) {
    return `<div class="empty-state">nothing to lay out — no cards in this scope</div>`;
  }

  const rows = planned
    .map((c) => {
      const start = new Date(c.startsAt);
      const end = new Date(c.endsAt);
      const span = Math.max(1, dayIndex(start, end));
      const cards = todos.filter((t) => t.cycleId === c.id);
      const done = cards.filter((t) => doneIds.has(t.status)).length;
      const pct = cards.length ? Math.round((done / cards.length) * 100) : 0;
      const today = dayIndex(start, new Date());
      const marker =
        today >= 0 && today <= span
          ? `<span class="tl-today" style="left:${(today / span) * 100}%" title="today"></span>`
          : "";
      const bars = cards
        .map((t) => {
          const isDone = doneIds.has(t.status);
          return `<button type="button" class="tl-card${isDone ? " done" : ""}" data-todo-id="${escapeHtml(
            t.id,
          )}" title="#${t.seq} ${escapeHtml(t.title)}">
            <span class="tl-card-seq">#${t.seq}</span>
            <span class="tl-card-title">${escapeHtml(t.title)}</span>
            ${t.estimate ? `<span class="tl-card-pts">${t.estimate}</span>` : ""}
          </button>`;
        })
        .join("");
      return `
        <div class="tl-row">
          <div class="tl-head">
            <div class="tl-name">${escapeHtml(c.name)}${
              c.closedAt ? `<span class="tl-closed">closed</span>` : ""
            }</div>
            <div class="tl-dates">${escapeHtml(c.startsAt.slice(0, 10))} → ${escapeHtml(
              c.endsAt.slice(0, 10),
            )}</div>
            <div class="tl-progress" title="${done}/${cards.length} cards done">
              <span style="width:${pct}%"></span>
            </div>
          </div>
          <div class="tl-track">${marker}<div class="tl-cards">${bars}</div></div>
        </div>`;
    })
    .join("");

  const loose = unplanned.length
    ? `<div class="tl-row tl-row-unplanned">
         <div class="tl-head"><div class="tl-name">unplanned</div>
           <div class="tl-dates">${unplanned.length} card${unplanned.length > 1 ? "s" : ""} in no cycle</div>
         </div>
         <div class="tl-track"><div class="tl-cards">${unplanned
           .map(
             (t) =>
               `<button type="button" class="tl-card" data-todo-id="${escapeHtml(t.id)}" title="#${t.seq} ${escapeHtml(
                 t.title,
               )}">
                  <span class="tl-card-seq">#${t.seq}</span>
                  <span class="tl-card-title">${escapeHtml(t.title)}</span>
                </button>`,
           )
           .join("")}</div></div>
       </div>`
    : "";

  return `<div class="timeline">${rows}${loose}</div>`;
}
