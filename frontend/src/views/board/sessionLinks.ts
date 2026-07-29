import { getSessions } from "../../api";
import type { Session, Todo } from "../../api";
import { escapeHtml, formatCost, formatTokens, truncate } from "../../domain/format";
import { labelForFolder } from "../../scope";

/** The card's linked sessions we've cached so far, in link order. */
export function linkedSessions(t: Todo, sessCache: Map<string, Session>): Session[] {
  const out: Session[] = [];
  for (const id of t.linkedSessionIds ?? []) {
    const s = sessCache.get(id);
    if (s) out.push(s);
  }
  return out;
}

/**
 * Tokens/cost strip for a linked card: the frozen snapshot once the card is
 * done, otherwise the cached sessions' live numbers summed — a ticket
 * routinely spans several sessions, and only the total is meaningful.
 */
export function sessMetricsHtml(t: Todo, sessCache: Map<string, Session>): string {
  if (t.snapshot) {
    const tok = formatTokens(t.snapshot.tokens);
    const cost = formatCost(t.snapshot.costUsd);
    const over = t.snapshot.sessions > 1 ? ` over ${t.snapshot.sessions} sessions` : "";
    return `<div class="todo-sess todo-sess-frozen" title="frozen when done${over}">✓ ${escapeHtml(tok)} tok · ${escapeHtml(cost)}</div>`;
  }
  const linked = linkedSessions(t, sessCache);
  if (!linked.length) return "";
  const tok = formatTokens(linked.reduce((n, s) => n + s.totalTokens + s.agentTokens, 0));
  const cost = formatCost(linked.reduce((n, s) => n + s.costUsd + s.agentCostUsd, 0));
  const dot = linked.some((s) => s.running) ? `<span class="live-dot live-dot-inline"></span>` : "";
  const n =
    linked.length > 1
      ? `<span class="todo-sess-n" title="${linked.length} linked sessions">×${linked.length}</span>`
      : "";
  return `<div class="todo-sess">${dot}<span>${escapeHtml(tok)} tok · ${escapeHtml(cost)}</span>${n}</div>`;
}

/** `https://github.com/o/r/pull/123` → `PR #123`, falling back to "PR". */
export function prLabel(url: string): string {
  const n = /\/pull\/(\d+)/.exec(url)?.[1];
  return n ? `PR #${n}` : "PR";
}

/**
 * The review end of the loop: once a linked session opens a PR, the card
 * carries a chip straight to it, so the board alone answers "what's waiting
 * on me". Before that, it shows the branch the work is on — but never the
 * base branch, which is just noise.
 */
export function sessLinksHtml(t: Todo, sessCache: Map<string, Session>): string {
  const linked = linkedSessions(t, sessCache);
  const prs = [...new Set(linked.map((s) => s.prUrl).filter(Boolean))] as string[];
  if (prs.length) {
    return `<div class="todo-links">${prs
      .map(
        (url) =>
          `<a class="todo-pr" href="${escapeHtml(url)}" target="_blank" rel="noreferrer"
             title="${escapeHtml(url)}">${escapeHtml(prLabel(url))}</a>`,
      )
      .join("")}</div>`;
  }
  const branches = [
    ...new Set(linked.map((s) => s.gitBranch).filter((b) => b && b !== "main" && b !== "master")),
  ];
  if (branches.length !== 1) return ""; // 0 = nothing to say; >1 = noise
  return `<div class="todo-links"><span class="todo-branch" title="branch">⑂ ${escapeHtml(
    truncate(branches[0]!, 28),
  )}</span></div>`;
}

/** One linked-session row: live numbers, its PR, and an unlink button. */
export function panelSessRowHtml(id: string, sessCache: Map<string, Session>): string {
  const s = sessCache.get(id);
  const dot = s?.running ? `<span class="live-dot live-dot-inline"></span>` : "";
  const inner = s
    ? `${dot}<span class="cand-title">${escapeHtml(truncate(s.title || "untitled session", 30))}</span>
       <span class="cand-meta">${escapeHtml(labelForFolder(s.project))} · ${escapeHtml(
         formatTokens(s.totalTokens + s.agentTokens),
       )} tok · ${escapeHtml(formatCost(s.costUsd + s.agentCostUsd))}</span>`
    : `<span class="cand-meta">session ${escapeHtml(id.slice(0, 8))}…</span>`;
  const pr = s?.prUrl
    ? `<a class="panel-sess-pr" href="${escapeHtml(s.prUrl)}" target="_blank" rel="noreferrer"
         title="${escapeHtml(s.prUrl)}">${escapeHtml(prLabel(s.prUrl))}</a>`
    : "";
  return `
    <div class="panel-sess-row">
      <a class="panel-sess" href="/claude/session/${encodeURIComponent(id)}">${inner}</a>
      ${pr}
      <button type="button" class="panel-sess-unlink" data-sid="${escapeHtml(id)}" title="unlink">✕</button>
    </div>`;
}

/**
 * Panel block: every linked session as a row, plus a pick-list to link
 * another — a ticket that spans sessions is the norm, not the exception.
 */
export function panelSessHtml(t: Todo, sessCache: Map<string, Session>): string {
  const ids = t.linkedSessionIds ?? [];
  return `
    <div class="panel-field">
      <div class="panel-label">${ids.length ? "sessions" : "link a session"}</div>
      ${ids.map((id) => panelSessRowHtml(id, sessCache)).join("")}
      <div class="panel-sess-list"><div class="panel-sess-empty">loading…</div></div>
    </div>`;
}

/** Running-first recent sessions in the todo's repo (all repos if unset).
 *  A card can carry a scope label rather than a real repo — an umbrella
 *  group name covering several repos — so an empty filtered list falls back
 *  to all repos instead of offering nothing to link. */
export async function sessionCandidates(repo?: string): Promise<Session[]> {
  let sessions = await getSessions(7, repo || undefined, "active");
  if (repo && sessions.length === 0) {
    sessions = await getSessions(7, undefined, "active");
  }
  return sessions
    .sort(
      (a, b) =>
        Number(b.running) - Number(a.running) || Date.parse(b.startedAt) - Date.parse(a.startedAt),
    )
    .slice(0, 8);
}
