// Session action flow: the main thread's tool activity collapsed into a spine
// of phase nodes (consecutive same-kind tool calls folded together), each
// subagent spawn a clickable "delegate" node linking to that subagent. A
// bird's-eye "what is this session doing" view — categories + counts only,
// never prompt or file content. Built as an escaped innerHTML string; the last
// node pulses while the session is running. Delegate nodes carry data-agent-id
// so they open the same inspector as the graph / timeline.

import type { FlowNode, SessionDetail } from "./api";
import { escapeHtml, truncate } from "./format";

// Accent per phase kind, deliberately distinct from the model-family palette
// so a flow node never reads as a model badge.
const KIND_COLOR: Record<string, string> = {
  explore: "#60a5fa",
  edit: "#e0703c",
  run: "#a78bfa",
  delegate: "#f472b6",
  other: "#9ca3af",
};

export function kindColor(kind: string): string {
  return KIND_COLOR[kind] ?? KIND_COLOR["other"]!;
}

/** Coarse phase kind for a tool name — mirrors the backend's flowKind. */
export function toolKind(name: string): string {
  switch (name) {
    case "Read":
    case "Grep":
    case "Glob":
      return "explore";
    case "Edit":
    case "Write":
    case "MultiEdit":
      return "edit";
    case "Bash":
      return "run";
    case "Agent":
    case "Task":
      return "delegate";
    default:
      return "other";
  }
}

/** hh:mm from an ISO timestamp; "" for a zero/unknown time (Go's year-1 zero). */
function hhmm(iso: string): string {
  const d = new Date(iso);
  if (!iso || d.getFullYear() < 2000) return "";
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function timeRange(node: FlowNode): string {
  const a = hhmm(node.startTs);
  const b = hhmm(node.endTs);
  if (!a) return "";
  return !b || a === b ? a : `${a}–${b}`;
}

/** "Read×5, Grep×2" from a phase's per-tool breakdown. */
function toolSummary(node: FlowNode): string {
  if (!node.tools || node.tools.length === 0) return "";
  return node.tools.map((t) => `${t.name}×${t.count}`).join(", ");
}

function nodeTitle(node: FlowNode): string {
  const parts: string[] = [];
  if (node.kind === "delegate") {
    parts.push(`delegate → ${node.label || "subagent"}`);
  } else {
    parts.push(node.kind);
    const tools = toolSummary(node);
    if (tools) parts.push(tools);
  }
  const range = timeRange(node);
  if (range) parts.push(range);
  return parts.join(" · ");
}

function renderNode(node: FlowNode, isLast: boolean, running: boolean): string {
  const color = kindColor(node.kind);
  const runCls = isLast && running ? " flow-node-running" : "";
  const title = escapeHtml(nodeTitle(node));

  if (node.kind === "delegate") {
    const clickable = node.agentId ? " flow-node-clickable" : "";
    const attr = node.agentId ? ` data-agent-id="${escapeHtml(node.agentId)}"` : "";
    const label = escapeHtml(truncate(node.label || "delegate", 16));
    return `
      <div class="flow-node flow-delegate${clickable}${runCls}" style="--flow-color:${color}"${attr} title="${title}">
        <span class="flow-node-icon">⑃</span>
        <span class="flow-node-label">${label}</span>
      </div>`;
  }

  return `
    <div class="flow-node flow-${node.kind}${runCls}" style="--flow-color:${color}" title="${title}">
      <span class="flow-node-label">${escapeHtml(node.kind)}</span>
      <span class="flow-node-count">${node.count}</span>
    </div>`;
}

/** Render the main-thread action flow into a wrapping spine div. */
export function renderSessionFlow(session: SessionDetail): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "flow-wrapper";

  const flow = session.mainFlow ?? [];
  if (flow.length === 0) {
    wrapper.innerHTML = `<div class="empty-state">no tool activity on the main thread</div>`;
    return wrapper;
  }

  const nodes = flow
    .map((n, i) => renderNode(n, i === flow.length - 1, session.running))
    .join(`<span class="flow-arrow">→</span>`);

  // Legend: the kinds actually present, in order of first appearance.
  const kinds: string[] = [];
  for (const n of flow) if (!kinds.includes(n.kind)) kinds.push(n.kind);
  const legend = kinds
    .map(
      (k) =>
        `<span class="flow-legend-item"><i class="flow-swatch" style="background:${kindColor(k)}"></i>${escapeHtml(k)}</span>`,
    )
    .join("");

  const caption = `${flow.length} phase${flow.length === 1 ? "" : "s"} · main-thread activity, grouped`;

  wrapper.innerHTML = `
    <div class="wrap-flow">${nodes}</div>
    <div class="flow-foot">
      <span class="flow-caption">${escapeHtml(caption)}</span>
      <span class="flow-legend">${legend}</span>
    </div>`;

  return wrapper;
}
