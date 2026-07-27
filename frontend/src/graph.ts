// SVG agent graph — renders a session's agent spawn hierarchy the way the
// Claude Code desktop app does: the main session sits dead-center and agent
// cards fan out in two columns (left / right), connected by curved edges.
// Built as a plain HTML string parsed via innerHTML (allowed by spec) for
// straightforward templating; all dynamic text is escaped.

import type { AgentRun, SessionDetail } from "./api";
import {
  escapeHtml,
  formatDuration,
  formatRelativeTime,
  formatTokens,
  modelColor,
  truncate,
} from "./format";

const SVG_NS = "http://www.w3.org/2000/svg";

const MAIN_W = 290;
const MAIN_H_WITH_FOOTER = 190;
const MAIN_H_NO_FOOTER = 130;
const CARD_W = 230;
const CARD_H = 112;
const GAP = 90;
const V_GAP = 18;
const MARGIN_X = 40;
const MARGIN_Y = 32;
const MIN_WIDTH = 720;

type Side = "main" | "left" | "right";

export interface TreeNode {
  agent: AgentRun | null; // null only for the synthetic root (the session itself)
  children: TreeNode[];
  side: Side;
  sideDepth: number; // 1-indexed depth within its side; unused (0) for the root
  x: number; // left edge, assigned during layout
  y: number; // vertical center, assigned during layout
}

/** Live agents pulse; the backend derives this from transcript freshness. */
function isActive(agent: AgentRun): boolean {
  return agent.running;
}

/**
 * Build the tree from the flat agents[] list via parentAgentId. "" (or any
 * dangling reference to an id not present in the list) is treated as a
 * direct child of the root so malformed data never silently drops a node.
 */
export function buildTree(session: SessionDetail): TreeNode {
  const knownIds = new Set(session.agents.map((a) => a.id));
  const byParent = new Map<string, AgentRun[]>();

  for (const agent of session.agents) {
    const key = agent.parentAgentId && knownIds.has(agent.parentAgentId) ? agent.parentAgentId : "";
    const bucket = byParent.get(key);
    if (bucket) bucket.push(agent);
    else byParent.set(key, [agent]);
  }

  const root: TreeNode = { agent: null, children: [], side: "main", sideDepth: 0, x: 0, y: 0 };

  function attach(parentNode: TreeNode, parentKey: string): void {
    const kids = byParent.get(parentKey) ?? [];
    for (const kid of kids) {
      const node: TreeNode = { agent: kid, children: [], side: "main", sideDepth: 0, x: 0, y: 0 };
      parentNode.children.push(node);
      attach(node, kid.id);
    }
  }

  attach(root, "");
  return root;
}

/** Tags every node in a subtree with its side and 1-indexed side-depth. */
function tagSide(node: TreeNode, side: Side, depth: number): void {
  node.side = side;
  node.sideDepth = depth;
  for (const child of node.children) tagSide(child, side, depth + 1);
}

/**
 * Stacks a forest of subtrees top-to-bottom (tidy-tree per side): leaves get
 * a `CARD_H` + `V_GAP` slot, a parent's y is the midpoint of its first/last
 * child. Returns the total block height. Y values are local (start at 0);
 * the caller re-centers them against the main node afterward.
 */
function layoutSide(roots: TreeNode[]): number {
  let cursor = 0;

  function visit(node: TreeNode): number {
    if (node.children.length === 0) {
      node.y = cursor + CARD_H / 2;
      cursor += CARD_H + V_GAP;
      return node.y;
    }
    const centers = node.children.map(visit);
    node.y = (centers[0]! + centers[centers.length - 1]!) / 2;
    return node.y;
  }

  for (const root of roots) visit(root);
  return Math.max(cursor - V_GAP, 0);
}

function maxSideDepth(node: TreeNode): number {
  let max = node.sideDepth;
  for (const child of node.children) max = Math.max(max, maxSideDepth(child));
  return max;
}

function assignX(node: TreeNode, mainCenterX: number): void {
  if (node.side === "right") {
    node.x = mainCenterX + MAIN_W / 2 + GAP + (node.sideDepth - 1) * (CARD_W + GAP);
  } else if (node.side === "left") {
    node.x = mainCenterX - MAIN_W / 2 - GAP - (node.sideDepth - 1) * (CARD_W + GAP) - CARD_W;
  }
  for (const child of node.children) assignX(child, mainCenterX);
}

function shiftY(node: TreeNode, offset: number): void {
  node.y += offset;
  for (const child of node.children) shiftY(child, offset);
}

export interface Layout {
  width: number;
  height: number;
  mainCenterY: number;
}

/**
 * Splits main's direct children into two alternating columns (spawn order
 * zigzags outward: index 0,2,4… → right, 1,3,5… → left), lays out each side
 * as its own mirrored tidy tree, then centers the shorter side against the
 * taller one (and both against the main node). Mutates `root` and every
 * descendant's `x`/`y` in place.
 */
export function layout(root: TreeNode, mainH: number): Layout {
  const children = [...root.children].sort(
    (a, b) => new Date(a.agent!.startedAt).getTime() - new Date(b.agent!.startedAt).getTime(),
  );
  const rightRoots = children.filter((_, i) => i % 2 === 0);
  const leftRoots = children.filter((_, i) => i % 2 === 1);

  for (const node of rightRoots) tagSide(node, "right", 1);
  for (const node of leftRoots) tagSide(node, "left", 1);

  const rightHeight = layoutSide(rightRoots);
  const leftHeight = layoutSide(leftRoots);

  const leftMaxDepth = leftRoots.reduce((m, n) => Math.max(m, maxSideDepth(n)), 0);
  const rightMaxDepth = rightRoots.reduce((m, n) => Math.max(m, maxSideDepth(n)), 0);

  const leftExtent = leftMaxDepth > 0 ? GAP + (leftMaxDepth - 1) * (CARD_W + GAP) + CARD_W : 0;
  const rightExtent = rightMaxDepth > 0 ? GAP + (rightMaxDepth - 1) * (CARD_W + GAP) + CARD_W : 0;

  const mainCenterX = MARGIN_X + MAIN_W / 2 + leftExtent;
  const width = Math.max(mainCenterX + MAIN_W / 2 + rightExtent + MARGIN_X, MIN_WIDTH);

  assignX(root, mainCenterX);

  const innerHeight = Math.max(leftHeight, rightHeight, mainH);
  const mainCenterY = MARGIN_Y + innerHeight / 2;

  if (leftRoots.length > 0) {
    const offset = mainCenterY - leftHeight / 2;
    for (const node of leftRoots) shiftY(node, offset);
  }
  if (rightRoots.length > 0) {
    const offset = mainCenterY - rightHeight / 2;
    for (const node of rightRoots) shiftY(node, offset);
  }

  root.x = mainCenterX - MAIN_W / 2;
  root.y = mainCenterY;

  const height = innerHeight + MARGIN_Y * 2;

  return { width, height, mainCenterY };
}

function modelBadgeSvg(model: string, xOffset: number): string {
  const color = modelColor(model);
  const label = escapeHtml(truncate(model, 8));
  return `
    <g transform="translate(${xOffset},0)">
      <rect width="62" height="16" rx="0" fill="${color}" opacity="0.15"></rect>
      <circle cx="9" cy="8" r="3" fill="${color}"></circle>
      <text x="17" y="12" class="node-badge-text" fill="${color}">${label}</text>
    </g>`;
}

function hexIconSvg(x: number, y: number): string {
  const pts = [
    [x + 6, y],
    [x + 11, y + 3],
    [x + 11, y + 9],
    [x + 6, y + 12],
    [x + 1, y + 9],
    [x + 1, y + 3],
  ]
    .map(([px, py]) => `${px},${py}`)
    .join(" ");
  return `<polygon points="${pts}" class="node-icon"></polygon>`;
}

function forkIconSvg(x: number, y: number): string {
  return `
    <g class="node-icon" transform="translate(${x}, ${y})">
      <circle cx="2" cy="2" r="1.5"></circle>
      <circle cx="2" cy="10" r="1.5"></circle>
      <circle cx="9" cy="6" r="1.5"></circle>
      <path d="M2 3.5 V6 Q2 6 4.5 6 H9 M2 6 V8.5" fill="none"></path>
    </g>`;
}

/** Right-aligned status pill: green "running" (with pulsing dot) or muted "idle"/nothing. */
function statusPillSvg(running: boolean, rightX: number, cy: number): string {
  const width = running ? 74 : 42;
  const x = rightX - width;
  const cls = running ? "status-pill status-pill-running" : "status-pill status-pill-idle";
  const dot = running ? `<circle cx="${x + 13}" cy="${cy}" r="3" class="status-pill-dot"></circle>` : "";
  const textX = running ? x + 22 : x + width / 2;
  const anchor = running ? "" : ` text-anchor="middle"`;
  const label = running ? "running" : "idle";
  return `
    <g class="${cls}">
      <rect x="${x}" y="${cy - 9}" width="${width}" height="18" rx="0"></rect>
      ${dot}
      <text x="${textX}" y="${cy + 4}"${anchor} class="status-pill-text">${label}</text>
    </g>`;
}

function hollowStatusDot(x: number, y: number): string {
  return `<circle cx="${x}" cy="${y}" r="4" class="status-dot-hollow"></circle>`;
}

/** Donut ring showing a fraction filled, drawn clockwise from the top. */
function contextRingSvg(cx: number, cy: number, r: number, strokeW: number, fraction: number): string {
  const circumference = 2 * Math.PI * r;
  const clamped = Math.max(0, Math.min(1, fraction));
  const dash = clamped * circumference;
  return `
    <g transform="rotate(-90 ${cx} ${cy})">
      <circle cx="${cx}" cy="${cy}" r="${r}" class="ctx-ring-track" stroke-width="${strokeW}" fill="none"></circle>
      <circle cx="${cx}" cy="${cy}" r="${r}" class="ctx-ring-fill" stroke-width="${strokeW}" fill="none"
        stroke-dasharray="${dash} ${circumference}" stroke-linecap="round"></circle>
    </g>`;
}

/** Token counts in flat "k" units (319000 -> "319k"), matching the ring's compact label style. */
function formatContextK(n: number): string {
  return `${Math.round(n / 1000)}k`;
}

interface MainNodeOptions {
  mainH: number;
  leftHasChildren: boolean;
  rightHasChildren: boolean;
  leftActive: boolean;
  rightActive: boolean;
}

function renderMainNode(node: TreeNode, session: SessionDetail, opts: MainNodeOptions): string {
  const { mainH, leftHasChildren, rightHasChildren, leftActive, rightActive } = opts;
  const x = node.x;
  const y = node.y - mainH / 2;
  const hasFooter = session.contextTokens > 0 && session.contextWindow > 0;

  const title = escapeHtml(truncate(session.title || "untitled session", 28));
  const badges = session.models
    .slice(0, 2)
    .map((m, i) => modelBadgeSvg(m, i * 68))
    .join("");
  const turns = escapeHtml(`${session.messageCount} · ${formatRelativeTime(session.endedAt)}`);

  const headerY = 26;
  const pill = statusPillSvg(session.running, MAIN_W - 16, headerY);

  const rowModelY = 58;
  const rowTaskY = 82;
  const rowTurnsY = 106;
  const dividerY = 120;

  let footer = "";
  if (hasFooter) {
    const fraction = session.contextTokens / session.contextWindow;
    const pct = Math.round(fraction * 100);
    const ringCy = 148;
    const ring = contextRingSvg(28, ringCy, 10, 3, fraction);
    const ctxText = escapeHtml(
      `${formatContextK(session.contextTokens)} / ${formatContextK(session.contextWindow)} · ${pct}%`,
    );
    footer = `
      <line x1="16" y1="${dividerY}" x2="${MAIN_W - 16}" y2="${dividerY}" class="node-divider"></line>
      ${ring}
      <text x="48" y="${ringCy + 4}" class="node-meta">${ctxText}</text>`;
  }

  const leftDot = leftHasChildren
    ? `<circle cx="0" cy="${mainH / 2}" r="3.5" class="connection-dot${leftActive ? " connection-dot-active" : ""}"></circle>`
    : "";
  const rightDot = rightHasChildren
    ? `<circle cx="${MAIN_W}" cy="${mainH / 2}" r="3.5" class="connection-dot${rightActive ? " connection-dot-active" : ""}"></circle>`
    : "";

  return `
    <g class="graph-node graph-node-main" data-node="main" transform="translate(${x}, ${y})">
      <rect x="-3" y="-3" width="${MAIN_W + 6}" height="${mainH + 6}" rx="0" class="node-main-glow"></rect>
      <rect width="${MAIN_W}" height="${mainH}" rx="0" class="node-main-rect"></rect>
      ${hexIconSvg(16, headerY - 6)}
      <text x="38" y="${headerY + 4}" class="node-title">main</text>
      ${pill}
      <line x1="16" y1="42" x2="${MAIN_W - 16}" y2="42" class="node-divider"></line>
      <text x="16" y="${rowModelY}" class="node-label">model</text>
      <g transform="translate(78, ${rowModelY - 12})">${badges}</g>
      <text x="16" y="${rowTaskY}" class="node-label">task</text>
      <text x="78" y="${rowTaskY}" class="node-value">${title}</text>
      <text x="16" y="${rowTurnsY}" class="node-label">turns</text>
      <text x="78" y="${rowTurnsY}" class="node-value">${turns}</text>
      ${footer}
      ${leftDot}
      ${rightDot}
    </g>`;
}

function renderAgentNode(node: TreeNode): string {
  const agent = node.agent;
  if (!agent) return "";
  const x = node.x;
  const y = node.y - CARD_H / 2;
  const active = isActive(agent);
  const typeLabel = escapeHtml(truncate(agent.agentType, 20));
  const desc = escapeHtml(truncate(agent.description || "—", 34));
  const meta = escapeHtml(
    `${formatTokens(agent.totalTokens)} tok · ${agent.messageCount} msgs · ${formatDuration(agent.durationMs)}`,
  );
  const color = modelColor(agent.model);
  const statusEl = active ? statusPillSvg(true, CARD_W - 12, 18) : hollowStatusDot(CARD_W - 16, 15);

  return `
    <g class="graph-node graph-node-agent${active ? " graph-node-active" : ""}" data-agent-id="${escapeHtml(agent.id)}" transform="translate(${x}, ${y})">
      <rect width="${CARD_W}" height="${CARD_H}" rx="0" class="node-rect"></rect>
      ${forkIconSvg(14, 12)}
      <text x="32" y="22" class="node-type">${typeLabel}</text>
      ${statusEl}
      <text x="16" y="42" class="node-eyebrow">TASK</text>
      <text x="16" y="58" class="node-value">${desc}</text>
      <circle cx="19" cy="72" r="2.5" fill="${color}"></circle>
      <text x="26" y="76" class="node-modelid">${escapeHtml(agent.modelId)}</text>
      <text x="16" y="96" class="node-meta">${meta}</text>
    </g>`;
}

function bezierPath(x1: number, y1: number, x2: number, y2: number): string {
  const dx = x2 - x1;
  const c1x = x1 + dx * 0.45;
  const c2x = x1 + dx * 0.55;
  return `M ${x1} ${y1} C ${c1x} ${y1}, ${c2x} ${y2}, ${x2} ${y2}`;
}

function renderEdge(x1: number, y1: number, x2: number, y2: number, active: boolean): string {
  const cls = active ? "graph-edge graph-edge-active" : "graph-edge";
  return `<path d="${bezierPath(x1, y1, x2, y2)}" class="${cls}"></path>`;
}

/**
 * Edge endpoints: main→direct-child edges originate from the fixed
 * connection-dot point on main's border; nested (agent→agent) edges
 * originate from the parent card's outer edge at the parent's own y. Both
 * terminate on the child's inner edge (the side facing back toward main).
 */
function edgeEndpoints(
  parent: TreeNode,
  child: TreeNode,
  mainCenterY: number,
): { x1: number; y1: number; x2: number; y2: number } {
  const fromMain = parent.agent === null;
  const side = child.side;
  const x1 = fromMain
    ? side === "right"
      ? parent.x + MAIN_W
      : parent.x
    : side === "right"
      ? parent.x + CARD_W
      : parent.x;
  const y1 = fromMain ? mainCenterY : parent.y;
  const x2 = side === "right" ? child.x : child.x + CARD_W;
  const y2 = child.y;
  return { x1, y1, x2, y2 };
}

/**
 * Render the full agent graph for a session into a scrollable wrapper div.
 * A session with no subagents still renders its lone main node (layout and
 * renderMainNode both handle a childless root).
 */
export function renderAgentGraph(session: SessionDetail): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "agent-graph-wrapper";

  const root = buildTree(session);
  const hasFooter = session.contextTokens > 0 && session.contextWindow > 0;
  const mainH = hasFooter ? MAIN_H_WITH_FOOTER : MAIN_H_NO_FOOTER;
  const { width, height, mainCenterY } = layout(root, mainH);

  const leftRoots = root.children.filter((c) => c.side === "left");
  const rightRoots = root.children.filter((c) => c.side === "right");
  const leftActive = leftRoots.some((c) => isActive(c.agent!));
  const rightActive = rightRoots.some((c) => isActive(c.agent!));

  let edgesSvg = "";
  let nodesSvg = renderMainNode(root, session, {
    mainH,
    leftHasChildren: leftRoots.length > 0,
    rightHasChildren: rightRoots.length > 0,
    leftActive,
    rightActive,
  });

  (function walk(node: TreeNode): void {
    for (const child of node.children) {
      const { x1, y1, x2, y2 } = edgeEndpoints(node, child, mainCenterY);
      const active = child.agent ? isActive(child.agent) : false;
      edgesSvg += renderEdge(x1, y1, x2, y2, active);
      nodesSvg += renderAgentNode(child);
      walk(child);
    }
  })(root);

  wrapper.innerHTML = `
    <svg class="agent-graph-svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" xmlns="${SVG_NS}">
      <g class="graph-edges">${edgesSvg}</g>
      <g class="graph-nodes">${nodesSvg}</g>
    </svg>`;

  return wrapper;
}
