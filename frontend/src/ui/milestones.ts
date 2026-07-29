// Session milestones: the semantic arc of a long session drawn as a centered
// org-chart graph — a root card in the middle, elbow connectors fanning down
// to branch columns of milestone cards (a branch cut, its commits, the PR,
// the release that closed it). Always fully expanded, no toggling. Grouping
// is mined mechanically server-side; each group can carry an AI one-liner,
// generated on demand (button, never automatic) through the user's own local
// `claude` CLI and cached on disk. The last node pulses while the session
// runs.
//
// Shape: a single-group session puts the group card itself at the root and
// fans its milestones into columns of four below it; a multi-group session
// roots at a small session card, fans out one column per group (group card,
// count badge, then its chain).

import { getSummaries, postSummarize } from "../api";
import type { Milestone, MilestoneGroup, SessionDetail } from "../api";
import { escapeHtml, truncate } from "../domain/format";

// One accent + glyph + noun per kind. Distinct from the action-flow palette so
// a milestone never reads as a phase node.
const KIND: Record<string, { color: string; icon: string; noun: string }> = {
  plan: { color: "#a78bfa", icon: "◇", noun: "plan" },
  branch: { color: "#38bdf8", icon: "⑂", noun: "branch" },
  commit: { color: "#34d399", icon: "●", noun: "commit" },
  pr: { color: "#f472b6", icon: "⇡", noun: "PR" },
  release: { color: "#f59e0b", icon: "⚑", noun: "release" },
};

const FALLBACK = { color: "#9ca3af", icon: "•", noun: "step" };
const KIND_ORDER = ["plan", "branch", "commit", "pr", "release"];

// Milestones per branch column when a single group fans straight from the
// root — keeps eight commits from stacking into one tall spine.
const CHAIN_COL = 4;

function meta(kind: string): { color: string; icon: string; noun: string } {
  return KIND[kind] ?? FALLBACK;
}

function plural(noun: string, n: number): string {
  if (n === 1) return noun;
  return noun === "branch" ? "branches" : `${noun}s`;
}

/** "hh:mm" from an ISO timestamp; "" for a zero/unknown time. */
function hhmm(iso: string): string {
  const d = new Date(iso);
  if (!iso || d.getFullYear() < 2000) return "";
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function timeRange(ms: Milestone[]): string {
  const a = hhmm(ms[0]?.ts ?? "");
  const b = hhmm(ms[ms.length - 1]?.ts ?? "");
  if (!a) return "";
  return a === b || !b ? a : `${a}–${b}`;
}

function chunk<T>(arr: T[], size: number): T[][] {
  const out: T[][] = [];
  for (let i = 0; i < arr.length; i += size) out.push(arr.slice(i, i + size));
  return out;
}

// --- per-session UI state ---------------------------------------------------
// The detail view rebuilds its whole DOM on every SSE update, so anything the
// user paid for (summaries) must live outside it.

// Last summaries seen for a session (from cache or a paid generation).
const sumState = new Map<string, { summaries: string[]; fresh: boolean }>();
// An in-flight generation; re-renders show "summarizing…" instead of resetting.
const inflight = new Map<string, Promise<string[]>>();

// --- rendering ---------------------------------------------------------------

// One milestone as a graph node: a kind-colored icon circle (the "avatar"),
// the label clamped to two lines, and a small kind · time line. Only PR cards
// are interactive — they link out to the pull request.
function renderMilestoneCard(m: Milestone, pulse: boolean): string {
  const { color, icon, noun } = meta(m.kind);
  const time = hhmm(m.ts);
  const title = escapeHtml(`${noun}: ${m.label || noun}${time ? ` · ${time}` : ""}`);
  const runCls = pulse ? " ms-mcard-running" : "";
  const inner = `
      <span class="ms-mcard-icon" style="--ms-kind:${color}">${icon}</span>
      <span class="ms-mcard-body">
        <span class="ms-mcard-label">${escapeHtml(m.label || noun)}${m.kind === "pr" && m.url ? " ↗" : ""}</span>
        <span class="ms-mcard-sub">${escapeHtml(noun)}${time ? ` · ${time}` : ""}</span>
      </span>`;

  if (m.kind === "pr" && m.url) {
    return `
      <a href="${escapeHtml(m.url)}" target="_blank" rel="noopener noreferrer" class="ms-mcard ms-mcard-link${runCls}" title="${title}">${inner}</a>`;
  }
  return `
    <div class="ms-mcard${runCls}" title="${title}">${inner}</div>`;
}

/** Group accent: the release it shipped, else its branch, else its first kind. */
function groupColor(g: MilestoneGroup): string {
  if (g.milestones.some((m) => m.kind === "release")) return KIND["release"]!.color;
  if (g.milestones.some((m) => m.kind === "branch")) return KIND["branch"]!.color;
  return meta(g.milestones[0]?.kind ?? "").color;
}

function groupCounts(g: MilestoneGroup): string {
  const counts = new Map<string, number>();
  for (const m of g.milestones) counts.set(m.kind, (counts.get(m.kind) ?? 0) + 1);
  const order = [...KIND_ORDER, ...[...counts.keys()].filter((k) => !KIND_ORDER.includes(k))];
  return order
    .filter((k) => counts.has(k))
    .map((k) => `${counts.get(k)} ${plural(meta(k).noun, counts.get(k)!)}`)
    .join(" · ");
}

// The group card: title, kind counts + time range, and the AI one-liner where
// the example chart puts a person's role.
function groupCard(g: MilestoneGroup, idx: number, summary: string | undefined, dotRunning: boolean): string {
  const color = groupColor(g);
  const range = timeRange(g.milestones);
  return `
    <div class="ms-gcard" style="--ms-accent:${color}">
      <span class="ms-gcard-top">
        <span class="ms-group-dot${dotRunning ? " ms-group-dot-running" : ""}" style="background:${color}"></span>
        <span class="ms-gcard-title">${escapeHtml(truncate(g.title, 48))}</span>
      </span>
      <span class="ms-gcard-meta">${escapeHtml(groupCounts(g))}${range ? ` · ${escapeHtml(range)}` : ""}</span>
      <span class="ms-group-summary" data-sum-idx="${idx}"${summary ? "" : " hidden"}>${escapeHtml(summary ?? "")}</span>
    </div>`;
}

// The synthetic root for a multi-group session, in the app accent like the
// agent graph's main node.
function rootCard(session: SessionDetail, groupCount: number, msCount: number): string {
  return `
    <div class="ms-gcard ms-gcard-root">
      <span class="ms-gcard-top">
        <span class="ms-group-dot${session.running ? " ms-group-dot-running" : ""}"></span>
        <span class="ms-gcard-title">${escapeHtml(truncate(session.title || "session", 40))}</span>
      </span>
      <span class="ms-gcard-meta">${msCount} milestone${msCount === 1 ? "" : "s"} · ${groupCount} group${groupCount === 1 ? "" : "s"}</span>
    </div>`;
}

/** Patch summaries + button state into whatever wrapper is currently mounted —
 * the SSE re-render may have replaced the one a request started from. */
function applySummaries(sessionId: string): void {
  const wrapper = document.querySelector<HTMLElement>(`[data-ms-session="${sessionId}"]`);
  if (!wrapper) return;
  const state = sumState.get(sessionId);
  const sums = state?.summaries ?? [];
  wrapper.querySelectorAll<HTMLElement>("[data-sum-idx]").forEach((el) => {
    const i = Number(el.dataset["sumIdx"]);
    const s = sums[i];
    el.hidden = !s;
    el.textContent = s ?? "";
  });
  const btn = wrapper.querySelector<HTMLButtonElement>(".ms-sum-btn");
  if (!btn) return;
  if (inflight.has(sessionId)) {
    btn.hidden = false;
    btn.disabled = true;
    btn.textContent = "summarizing…";
  } else if (state?.fresh) {
    btn.hidden = true;
  } else {
    btn.hidden = false;
    btn.disabled = false;
    btn.textContent = sums.length ? "✦ re-summarize" : "✦ summarize";
  }
}

function startSummarize(sessionId: string): void {
  if (inflight.has(sessionId)) return;
  const oldErr = document.querySelector<HTMLElement>(
    `[data-ms-session="${sessionId}"] .ms-sum-err`,
  );
  if (oldErr) {
    oldErr.hidden = true;
    oldErr.textContent = "";
  }
  const p = postSummarize(sessionId);
  inflight.set(sessionId, p);
  applySummaries(sessionId);
  p.then((summaries) => {
    sumState.set(sessionId, { summaries, fresh: true });
  })
    .catch((err: unknown) => {
      const wrapper = document.querySelector<HTMLElement>(`[data-ms-session="${sessionId}"]`);
      const errEl = wrapper?.querySelector<HTMLElement>(".ms-sum-err");
      if (errEl) {
        errEl.hidden = false;
        errEl.textContent = err instanceof Error ? err.message : "summarize failed";
      }
    })
    .finally(() => {
      inflight.delete(sessionId);
      applySummaries(sessionId);
    });
}

/** Render the session's milestone graph into a wrapper div. */
export function renderSessionMilestones(session: SessionDetail): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "flow-wrapper";
  wrapper.dataset["msSession"] = session.id;

  const ms = session.milestones ?? [];
  const groups = session.milestoneGroups ?? [];
  if (ms.length === 0 || groups.length === 0) {
    wrapper.innerHTML = `<div class="empty-state">no milestones yet — plans, commits, PRs and releases surface here as the session works</div>`;
    return wrapper;
  }

  const sums = sumState.get(session.id)?.summaries ?? [];
  const lastGroup = groups[groups.length - 1]!;
  const lastM = lastGroup.milestones[lastGroup.milestones.length - 1];
  const chainHtml = (col: Milestone[]): string =>
    col
      .map((m) => renderMilestoneCard(m, session.running && m === lastM))
      .join(`<span class="ms-edge ms-edge-arrow"></span>`);

  let root: string;
  let branches: string;
  if (groups.length === 1) {
    // The group card is the root; its milestones fan below in short columns.
    const g = groups[0]!;
    root = groupCard(g, 0, sums[0], session.running);
    branches = chunk(g.milestones, CHAIN_COL)
      .map((col) => `<div class="ms-branch"><i class="ms-vstub"></i><div class="ms-chain">${chainHtml(col)}</div></div>`)
      .join("");
  } else {
    root = rootCard(session, groups.length, ms.length);
    branches = groups
      .map((g, i) => {
        const dotRunning = session.running && i === groups.length - 1;
        return `
          <div class="ms-branch"><i class="ms-vstub"></i>${groupCard(g, i, sums[i], dotRunning)}
            <div class="ms-chain">
              <span class="ms-edge ms-edge-badge"><span class="ms-badge">${g.milestones.length}</span></span>
              ${chainHtml(g.milestones)}
            </div>
          </div>`;
      })
      .join("");
  }

  // Legend: the kinds actually present, in order of first appearance.
  const kinds: string[] = [];
  for (const m of ms) if (!kinds.includes(m.kind)) kinds.push(m.kind);
  const legend = kinds
    .map((k) => {
      const { color, noun } = meta(k);
      return `<span class="flow-legend-item"><i class="flow-swatch" style="background:${color}"></i>${escapeHtml(noun)}</span>`;
    })
    .join("");

  const caption = `${ms.length} milestone${ms.length === 1 ? "" : "s"} · ${groups.length} group${groups.length === 1 ? "" : "s"} · what this session shipped`;

  wrapper.innerHTML = `
    <div class="ms-graph-scroll">
      <div class="ms-graph">
        <div class="ms-root">${root}</div>
        <div class="ms-branches">${branches}</div>
      </div>
    </div>
    <div class="flow-foot">
      <span class="flow-caption">${escapeHtml(caption)}</span>
      <span class="ms-sum-err" hidden></span>
      <button type="button" class="ms-sum-btn" hidden></button>
      <span class="flow-legend">${legend}</span>
    </div>`;

  wrapper.addEventListener("click", (evt) => {
    if ((evt.target as HTMLElement).closest(".ms-sum-btn")) startSummarize(session.id);
  });

  // Show what we already know, then refresh from the server's cache (a local
  // disk read). Skip while a generation is in flight so its result can't be
  // overwritten by a pre-write read racing back late.
  applySummaries(session.id);
  if (!inflight.has(session.id)) {
    void getSummaries(session.id)
      .then((res) => {
        if (inflight.has(session.id)) return;
        const cur = sumState.get(session.id);
        if (!res.summaries && cur) return; // never downgrade to nothing
        if (res.summaries) sumState.set(session.id, { summaries: res.summaries, fresh: res.fresh });
        applySummaries(session.id);
      })
      .catch(() => applySummaries(session.id));
  }

  return wrapper;
}
