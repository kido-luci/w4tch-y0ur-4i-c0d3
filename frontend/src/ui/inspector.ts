// Inspector drawer body: a full breakdown of one clicked graph/timeline node —
// either a subagent or the main session. Uses only data already on the session
// payload. Returns an HTML string for the drawer panel; all dynamic text escaped.

import type { AgentRun, SessionDetail, TokenBreakdown } from "../api";
import {
  escapeHtml,
  formatAbsoluteTime,
  formatCost,
  formatDuration,
  formatTokens,
  modelBadgeHtml,
} from "../domain/format";

function tile(label: string, value: string): string {
  return `<div class="insp-tile"><div class="insp-tile-label">${label}</div><div class="insp-tile-value">${value}</div></div>`;
}

function kv(label: string, value: string): string {
  return `<div class="insp-kv"><span class="insp-kv-label">${label}</span><span class="insp-kv-value">${value}</span></div>`;
}

/** A thin track with the [start,end] window highlighted within the session span. */
function miniTimeline(session: SessionDetail, startMs: number, endMs: number): string {
  const t0 = new Date(session.startedAt).getTime();
  const t1 = Math.max(new Date(session.endedAt).getTime(), endMs);
  const span = Math.max(1, t1 - t0);
  const left = Math.min(100, Math.max(0, ((startMs - t0) / span) * 100));
  const width = Math.min(100 - left, Math.max(0.6, ((endMs - startMs) / span) * 100));
  return `<div class="insp-track"><div class="insp-track-fill" style="left:${left.toFixed(1)}%;width:${width.toFixed(1)}%"></div></div>`;
}

function tokenSection(t: TokenBreakdown): string {
  const total = t.inputTokens + t.outputTokens + t.cacheReadTokens + t.cacheWrite5mTokens + t.cacheWrite1hTokens;
  return `
    <div class="insp-section">
      <div class="insp-section-title">tokens</div>
      ${kv("input", formatTokens(t.inputTokens))}
      ${kv("output", formatTokens(t.outputTokens))}
      ${kv("cache read", formatTokens(t.cacheReadTokens))}
      ${kv("cache write 5m", formatTokens(t.cacheWrite5mTokens))}
      ${kv("cache write 1h", formatTokens(t.cacheWrite1hTokens))}
      ${kv("total", `<strong>${formatTokens(total)}</strong>`)}
    </div>`;
}

function statusPill(running: boolean, label: string): string {
  const cls = running ? "insp-pill insp-pill-running" : "insp-pill insp-pill-idle";
  const text = running ? "running" : label;
  return `<span class="${cls}">${running ? '<span class="insp-pill-dot"></span>' : ""}${escapeHtml(text)}</span>`;
}

function header(eyebrow: string, title: string, sub: string): string {
  return `
    <div class="insp-header">
      <div class="insp-eyebrow">${escapeHtml(eyebrow)}</div>
      <h3 class="insp-title">${escapeHtml(title)}</h3>
      <div class="insp-sub">${sub}</div>
    </div>`;
}

function agentBody(session: SessionDetail, agent: AgentRun): string {
  const sub = `${modelBadgeHtml(agent.model)}${statusPill(agent.running, agent.status || "done")}`;
  const start = new Date(agent.startedAt).getTime();
  const end = new Date(agent.endedAt).getTime();

  const desc = agent.description
    ? `<div class="insp-desc">${escapeHtml(agent.description)}</div>`
    : "";

  const grid = `
    <div class="insp-grid">
      ${tile("duration", formatDuration(agent.durationMs))}
      ${tile("cost", formatCost(agent.costUsd))}
      ${tile("messages", String(agent.messageCount))}
      ${tile("tool uses", String(agent.toolUseCount))}
      ${tile("depth", String(agent.spawnDepth))}
      ${tile("model", escapeHtml(agent.modelId || agent.model))}
    </div>`;

  let tools = "";
  const ts = agent.toolStats;
  if (ts) {
    tools = `
      <div class="insp-section">
        <div class="insp-section-title">tools</div>
        ${kv("reads", String(ts.readCount))}
        ${kv("searches", String(ts.searchCount))}
        ${kv("bash", String(ts.bashCount))}
        ${kv("edits", String(ts.editFileCount))}
        ${kv("lines", `<span class="insp-add">+${ts.linesAdded}</span> <span class="insp-del">-${ts.linesRemoved}</span>`)}
        ${kv("other tools", String(ts.otherToolCount))}
      </div>`;
  }

  return (
    header("subagent", agent.agentType || "agent", sub) +
    `<div class="insp-body">
      ${desc}
      <div class="insp-section">
        <div class="insp-section-title">timeline · ${escapeHtml(formatAbsoluteTime(agent.startedAt))}</div>
        ${miniTimeline(session, start, end)}
      </div>
      ${grid}
      ${tokenSection(agent.tokens)}
      ${tools}
    </div>`
  );
}

function mainBody(session: SessionDetail): string {
  const badges = session.models.map((m) => modelBadgeHtml(m)).join("");
  const sub = `${badges}${statusPill(session.running, "idle")}`;
  const start = new Date(session.startedAt).getTime();
  const end = new Date(session.endedAt).getTime();

  const ctx =
    session.contextWindow > 0
      ? `${Math.round((session.contextTokens / session.contextWindow) * 100)}%`
      : "—";

  const grid = `
    <div class="insp-grid">
      ${tile("duration", formatDuration(session.durationMs))}
      ${tile("main cost", formatCost(session.costUsd))}
      ${tile("messages", String(session.messageCount))}
      ${tile("compactions", String(session.compactCount))}
      ${tile("subagents", String(session.agentCount))}
      ${tile("context", ctx)}
    </div>`;

  const agentsSection =
    session.agentCount > 0
      ? `
      <div class="insp-section">
        <div class="insp-section-title">subagents</div>
        ${kv("count", String(session.agentCount))}
        ${kv("agent tokens", formatTokens(session.agentTokens))}
        ${kv("agent cost", formatCost(session.agentCostUsd))}
      </div>`
      : "";

  return (
    header("main session", session.title || "untitled session", sub) +
    `<div class="insp-body">
      <div class="insp-section">
        <div class="insp-section-title">timeline · ${escapeHtml(formatAbsoluteTime(session.startedAt))}</div>
        ${miniTimeline(session, start, end)}
      </div>
      ${grid}
      ${tokenSection(session.tokens)}
      ${agentsSection}
    </div>`
  );
}

/**
 * Build the drawer panel HTML for a target: `"main"` for the session, or an
 * agent id. Returns "" if the agent id is unknown (caller should not open).
 */
export function renderInspectorBody(session: SessionDetail, target: string): string {
  if (target === "main") return mainBody(session);
  const agent = session.agents.find((a) => a.id === target);
  return agent ? agentBody(session, agent) : "";
}
