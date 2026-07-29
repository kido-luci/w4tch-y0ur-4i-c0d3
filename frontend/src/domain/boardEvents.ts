// Rendering the board's history in prose.
//
// One module because two places show the same log from different angles: a
// card's panel folds its own events into the journey stream, and the cycles tab
// shows the whole board's recent activity. A second copy of "status means A → B"
// would drift the moment a new event kind lands.
//
// The log stores ids, not names — a state id, a cycle id, a card id — because
// names change and history must not. So the lookups are injected: the caller
// owns the live name tables, this module owns the wording.

import type { TodoEvent } from "../api";
import { escapeHtml } from "./format";

export interface EventNames {
  /** Column display name for a state id; the id itself is a fine fallback. */
  state(id: string): string;
  /** Cycle display name for a cycle id; "" means the card left every cycle. */
  cycle(id: string): string;
  /** A card's #seq for a card id, for re-parenting. */
  card(id: string): string;
}

const PRIORITY_LABEL = ["none", "low", "medium", "high", "urgent"];

/**
 * One event as an HTML fragment, or null for a kind that should not surface.
 *
 * `created` returns null on purpose: the card panel already synthesises that
 * row from the card's own createdAt, which also covers cards that predate the
 * event log. Callers that DO want it (the board-wide feed, where there is no
 * card to synthesise from) handle it themselves.
 */
export function describeEvent(e: TodoEvent, names: EventNames): string | null {
  const from = e.from ?? "";
  const to = e.to ?? "";
  switch (e.kind) {
    case "status":
      return `<span class="ev-move">${escapeHtml(names.state(from))} → ${escapeHtml(
        names.state(to),
      )}</span>`;
    case "estimate": {
      if (!Number(to)) return `<span>estimate cleared</span>`;
      return `<span>${from ? `estimate ${escapeHtml(from)} →` : "estimated"} ${escapeHtml(
        to,
      )} pts</span>`;
    }
    case "cycle":
      return `<span>${from ? `${escapeHtml(names.cycle(from))} → ` : "planned into "}${escapeHtml(
        names.cycle(to),
      )}</span>`;
    case "priority":
      return `<span>priority ${escapeHtml(PRIORITY_LABEL[Number(to)] ?? to)}</span>`;
    case "parent":
      return to
        ? `<span>nested under ${escapeHtml(names.card(to))}</span>`
        : `<span>un-nested to top level</span>`;
    case "created":
      return null;
    default:
      return null;
  }
}

/** Like describeEvent, but `created` renders too — for a feed with no card
 *  context of its own to say it. */
export function describeEventOrCreated(e: TodoEvent, names: EventNames): string | null {
  if (e.kind === "created") {
    return `<span>created in ${escapeHtml(names.state(e.to ?? ""))}</span>`;
  }
  return describeEvent(e, names);
}
