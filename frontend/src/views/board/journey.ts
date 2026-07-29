import { getSession, getShips, getTodoEvents } from "../../api";
import type { Session, Todo } from "../../api";
import { describeEvent } from "../../domain/boardEvents";
import type { EventNames } from "../../domain/boardEvents";
import { escapeHtml, formatCost, formatDuration, formatRelativeTime, formatTokens, truncate } from "../../domain/format";
import { prLabel } from "./sessionLinks";

/** Journey block shell: the card's whole story as one chronological stream —
 *  created → sessions (PR riding its session) → check/release runs → done.
 *  fillJourney builds the rows; the shell only decides the hint. */
export function panelJourneyHtml(t: Todo): string {
  // A card with no linked session still has a journey now — every column
  // move, estimate and cycle change is in the event log — so the hint only
  // speaks to what linking would ADD.
  const hint = (t.linkedSessionIds ?? []).length
    ? ""
    : `<div class="panel-sess-empty">link a session to add its cost and runs to the journey</div>`;
  return `
    <div class="panel-field">
      <div class="panel-label">journey</div>
      <div class="panel-journey"><div class="panel-sess-empty">loading…</div></div>
      ${hint}
    </div>`;
}

/** One timeline entry: relative time, then whatever the event is. */
export function journeyRow(ts: string, body: string, cls = ""): string {
  return `
    <div class="panel-journey-row${cls}" title="${escapeHtml(ts)}">
      <span class="panel-journey-when">${escapeHtml(formatRelativeTime(ts))}</span>
      ${body}
    </div>`;
}

// A run's record can land moments after a session's last transcript line —
// widen the match window past both ends so it isn't missed.
export const SHIP_WINDOW_PAD_MS = 5 * 60 * 1000;

/** Fills the journey: created and done render from the card alone; linked
 *  sessions anchor the middle (their PR chips ride along — the data has no
 *  PR-opened timestamp, so a separate event would be an invented position),
 *  and the repo's runs inside the sessions' window slot in between, newest
 *  8 kept with a link into ships for the rest. */
export function fillJourney(
  t: Todo,
  panelEl: HTMLElement,
  sessCache: Map<string, Session>,
  eventNames: EventNames,
): void {
  const listEl = panelEl.querySelector<HTMLElement>(".panel-journey");
  if (!listEl) return;
  const linked = t.linkedSessionIds ?? [];

  const events: { ts: number; html: string }[] = [
    { ts: Date.parse(t.createdAt), html: journeyRow(t.createdAt, `<span>created</span>`) },
  ];
  if (t.snapshot) {
    const s = t.snapshot;
    events.push({
      ts: Date.parse(s.takenAt),
      html: journeyRow(
        s.takenAt,
        `<span class="panel-journey-done">✓ done</span><span>${escapeHtml(formatTokens(s.tokens))} tok · ${escapeHtml(
          formatCost(s.costUsd),
        )} · ${escapeHtml(formatDuration(s.durationMs))}</span>`,
        " panel-journey-row--done",
      ),
    });
  }
  // Board history is fetched for EVERY card, linked or not: a column move is
  // part of the story even when no session was ever attached.
  const historyPromise = getTodoEvents(t.id)
    .then((evs) =>
      (evs ?? [])
        .map((e) => {
          const body = describeEvent(e, eventNames);
          return body ? { ts: Date.parse(e.ts), html: journeyRow(e.ts, body) } : null;
        })
        .filter((r): r is { ts: number; html: string } => r !== null),
    )
    .catch((): { ts: number; html: string }[] => []); // history is an extra, never the section

  const finish = (extra: { ts: number; html: string }[], footer = ""): void => {
    if (!listEl.isConnected) return;
    void historyPromise.then((hist) => {
      if (!listEl.isConnected) return;
      const all = [...events, ...hist, ...extra].sort((a, b) => a.ts - b.ts);
      listEl.innerHTML = all.length
        ? all.map((e) => e.html).join("") + footer
        : `<div class="panel-sess-empty">nothing has happened to this card yet</div>`;
    });
  };
  if (!linked.length) {
    finish([]);
    return;
  }

  Promise.all(
    linked.map((id): Promise<Session | null> => {
      const cached = sessCache.get(id);
      if (cached) return Promise.resolve(cached);
      return getSession(id)
        .then((s) => {
          sessCache.set(id, s);
          return s;
        })
        .catch(() => null); // gone from disk — skipped, not fatal
    }),
  )
    .then((results) => {
      const sessions = results.filter((s): s is Session => s !== null);
      const sessEvents = sessions.map((s) => ({
        ts: Date.parse(s.startedAt),
        html: journeyRow(
          s.startedAt,
          `<a class="panel-journey-sess" href="/claude/session/${encodeURIComponent(s.id)}">${escapeHtml(
            truncate(s.title || "untitled session", 26),
          )}</a><span>${escapeHtml(formatDuration(s.durationMs))}</span>${
            s.prUrl
              ? `<a class="panel-sess-pr" href="${escapeHtml(s.prUrl)}" target="_blank" rel="noreferrer"
                   title="${escapeHtml(s.prUrl)}">${escapeHtml(prLabel(s.prUrl))}</a>`
              : ""
          }`,
        ),
      }));
      if (!t.repo || !sessions.length) {
        finish(sessEvents);
        return undefined;
      }
      const start = Math.min(...sessions.map((s) => Date.parse(s.startedAt))) - SHIP_WINDOW_PAD_MS;
      const end =
        (t.snapshot ? Date.parse(t.snapshot.takenAt) : Date.now()) + SHIP_WINDOW_PAD_MS;
      return getShips(0, t.repo, 200).then((res) => {
        const inWindow = res.ships.filter((r) => {
          const ts = Date.parse(r.ts);
          return ts >= start && ts <= end;
        });
        const shipEvents = inWindow.slice(0, 8).map((r) => {
          const exitBadge =
            r.exit === 0
              ? `<span class="ship-exit ship-exit--ok">green</span>`
              : `<span class="ship-exit ship-exit--fail">exit ${r.exit}</span>`;
          return {
            ts: Date.parse(r.ts),
            html: journeyRow(
              r.ts,
              `<span class="ship-kind">${escapeHtml(r.kind)}</span>${
                r.version ? `<span>${escapeHtml(r.version)}</span>` : ""
              }${exitBadge}`,
            ),
          };
        });
        const footer =
          inWindow.length > 8
            ? `<a class="panel-ships-more" href="/project/ships">${inWindow.length - 8} earlier runs in ships →</a>`
            : "";
        finish([...sessEvents, ...shipEvents], footer);
      });
    })
    .catch(() => {
      if (listEl.isConnected) {
        listEl.innerHTML = `<div class="panel-sess-empty">failed to load the journey</div>`;
      }
    });
}
