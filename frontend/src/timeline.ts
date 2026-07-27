// Session timeline: the main thread's tool-call activity as a shaded strip
// (darker = more tool calls in that slot), plus one span bar per subagent on a
// shared time axis. An "events" toggle swaps the shading for per-tool tick
// marks (one per tool call, colored by phase kind) on every lane. A legend +
// caption explain the mode, and every slot/tick has a hover tooltip. Rows are
// clickable (data attrs) so they open the same inspector as the graph nodes.
// Built as an innerHTML SVG string; text escaped.

import type { ActivitySlot, SessionDetail, ToolEvent } from "./api";
import { kindColor, toolKind } from "./flow";
import {
  escapeHtml,
  formatAbsoluteTime,
  formatCost,
  formatDuration,
  formatTokens,
  modelColor,
  truncate,
} from "./format";

const SVG_NS = "http://www.w3.org/2000/svg";

const LABEL_W = 120;
const TRACK_W = 740;
const RIGHT_PAD = 16;
const WIDTH = LABEL_W + TRACK_W + RIGHT_PAD;
const ROW_H = 24;
const BAR_H = 14;
const TOP = 6;
const AXIS_H = 18;
const MIN_BAR = 3;

// 5-step opacity ramp for the activity shading (matches the "less → more" legend).
const HEAT_OPACITY = [0.1, 0.32, 0.54, 0.76, 1.0];

// The per-tool tick mode is a per-browser preference, like the theme.
const EVENTS_KEY = "wyac-timeline-events";

function eventsOn(): boolean {
  return localStorage.getItem(EVENTS_KEY) === "1";
}

// 0..4 shade level, scaled to the busiest slot so the ramp adapts per session.
function levelFor(count: number, max: number): number {
  if (count <= 0) return 0;
  if (max <= 0) return 1;
  return Math.min(4, Math.max(1, Math.ceil((count / max) * 4)));
}

function hhmm(ms: number): string {
  const d = new Date(ms);
  return `${String(d.getHours()).padStart(2, "0")}:${String(d.getMinutes()).padStart(2, "0")}`;
}

function hhmmss(ms: number): string {
  const d = new Date(ms);
  return `${hhmm(ms)}:${String(d.getSeconds()).padStart(2, "0")}`;
}

// "Bash ×7, Edit ×6, Read ×6" — top `max` tools by count, with a "+N more" tail.
function toolList(entries: [string, number][], max = 6): string {
  const sorted = [...entries].sort((a, b) => b[1] - a[1]);
  const shown = sorted.slice(0, max).map(([name, n]) => `${name} ×${n}`);
  if (sorted.length > max) shown.push(`+${sorted.length - max} more`);
  return shown.join(", ");
}

// Tooltip for one activity slot: "10:15 · 20 tool calls" plus the tools that ran
// in it (top by count, "name ×n") on a second line.
function slotTooltip(time: string, slot: ActivitySlot): string {
  const head = `${time} · ${slot.count} tool call${slot.count === 1 ? "" : "s"}`;
  const list = slot.tools ? toolList(Object.entries(slot.tools)) : "";
  return list ? `${head}\n${list}` : head;
}

// Per-tool tick marks for one lane, colored by coarse phase kind.
function tickMarksHtml(
  events: ToolEvent[] | undefined,
  barY: number,
  h: number,
  xFor: (ms: number) => number,
): string {
  return (events ?? [])
    .map((e) => {
      const ms = new Date(e.ts).getTime();
      const tip = `${hhmmss(ms)} · ${e.name}`;
      return `<rect x="${(xFor(ms) - 1).toFixed(1)}" y="${barY}" width="2" height="${h}" fill="${kindColor(toolKind(e.name))}"><title>${escapeHtml(tip)}</title></rect>`;
    })
    .join("");
}

// The moments the user stopped the session, on the main lane. Drawn in both
// modes and full row height, because an interrupt is not a tool call: it sits
// across the lane rather than on the bar. No tool is named in the tooltip —
// which one was running isn't recoverable from the transcripts.
function interruptMarksHtml(times: string[] | undefined, xFor: (ms: number) => number): string {
  return (times ?? [])
    .map((iso) => {
      const ms = new Date(iso).getTime();
      const tip = `${hhmmss(ms)} · you stopped it here`;
      return `<rect x="${(xFor(ms) - 0.75).toFixed(1)}" y="${TOP}" width="1.5" height="${ROW_H}" fill="var(--px-red)"><title>${escapeHtml(tip)}</title></rect>`;
    })
    .join("");
}

/** Render the session timeline into a scrollable wrapper div. */
export function renderSessionTimeline(session: SessionDetail): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "timeline-wrapper";

  // Redrawn in place when the events toggle flips; row clicks are delegated by
  // the detail view on a parent container, so redraws don't lose them.
  const draw = (): void => {
    wrapper.innerHTML = buildTimelineHtml(session);
    wrapper.querySelector<HTMLButtonElement>(".timeline-events-btn")?.addEventListener("click", () => {
      localStorage.setItem(EVENTS_KEY, eventsOn() ? "0" : "1");
      draw();
    });
  };
  draw();
  return wrapper;
}

function buildTimelineHtml(session: SessionDetail): string {
  const agents = [...session.agents].sort(
    (a, b) => new Date(a.startedAt).getTime() - new Date(b.startedAt).getTime(),
  );

  // Time window covers the session and every agent (an agent can outlive the
  // parent's last write).
  let t0 = new Date(session.startedAt).getTime();
  let t1 = new Date(session.endedAt).getTime();
  for (const a of agents) {
    t0 = Math.min(t0, new Date(a.startedAt).getTime());
    t1 = Math.max(t1, new Date(a.endedAt).getTime());
  }
  const span = Math.max(1, t1 - t0);
  const xFor = (ms: number): number => LABEL_W + ((ms - t0) / span) * TRACK_W;

  const shownEvents =
    (session.mainToolEvents?.length ?? 0) +
    agents.reduce((n, a) => n + (a.toolEvents?.length ?? 0), 0);
  const droppedEvents =
    (session.mainToolEventsDropped ?? 0) +
    agents.reduce((n, a) => n + (a.toolEventsDropped ?? 0), 0);
  const eventsMode = shownEvents > 0 && eventsOn();

  const rows = 1 + agents.length; // main strip + one row per agent
  const height = TOP + rows * ROW_H + AXIS_H;

  // --- main thread: a shaded activity strip (or event ticks) over its span ---
  const mainColor = session.models.length ? modelColor(session.models[0]) : "var(--border-hover)";
  const activity = session.mainActivity ?? [];
  const total = activity.reduce((a, s) => a + s.count, 0);
  const mainBarY = TOP + (ROW_H - BAR_H) / 2;
  const sessStart = new Date(session.startedAt).getTime();
  const sessEnd = new Date(session.endedAt).getTime();
  const sessSpan = Math.max(1, sessEnd - sessStart);

  let mainBar: string;
  if (eventsMode) {
    const baseX = xFor(sessStart);
    const baseW = Math.max(MIN_BAR, xFor(sessEnd) - baseX);
    mainBar =
      `<rect x="${baseX.toFixed(1)}" y="${mainBarY}" width="${baseW.toFixed(1)}" height="${BAR_H}" rx="0" fill="${mainColor}" opacity="0.12"></rect>` +
      tickMarksHtml(session.mainToolEvents, mainBarY, BAR_H, xFor);
  } else if (total > 0) {
    const max = Math.max(...activity.map((s) => s.count));
    const segW = TRACK_W / activity.length;
    mainBar = activity
      .map((slot, i) => {
        const x = LABEL_W + i * segW;
        const tMs = sessStart + ((i + 0.5) / activity.length) * sessSpan;
        const tip = slotTooltip(hhmm(tMs), slot);
        return `<rect x="${x.toFixed(1)}" y="${mainBarY}" width="${(segW + 0.6).toFixed(1)}" height="${BAR_H}" rx="0" fill="${mainColor}" opacity="${HEAT_OPACITY[levelFor(slot.count, max)]}"><title>${escapeHtml(tip)}</title></rect>`;
      })
      .join("");
  } else {
    mainBar = `<rect x="${LABEL_W}" y="${mainBarY}" width="${TRACK_W}" height="${BAR_H}" rx="0" fill="${mainColor}" opacity="0.28"></rect>`;
  }
  const mainG = `
    <g class="timeline-row" data-node="main">
      <rect x="0" y="${TOP}" width="${WIDTH}" height="${ROW_H}" class="timeline-hit"></rect>
      <text x="0" y="${TOP + ROW_H / 2 + 4}" class="timeline-label">main</text>
      ${mainBar}
      ${interruptMarksHtml(session.interruptTimes, xFor)}
    </g>`;

  // --- subagents: one span bar each (faint under ticks in events mode) ---
  const agentRows = agents
    .map((a, i) => {
      const start = new Date(a.startedAt).getTime();
      const end = new Date(a.endedAt).getTime();
      const rowY = TOP + (i + 1) * ROW_H;
      const barY = rowY + (ROW_H - BAR_H) / 2;
      const barX = xFor(start);
      const barW = Math.max(MIN_BAR, ((end - start) / span) * TRACK_W);
      const runCls = a.running ? " timeline-bar-running" : "";
      const base = `${a.agentType || "agent"} · ${formatDuration(a.durationMs)} · ${formatTokens(a.totalTokens)} tok · ${formatCost(a.costUsd)}`;
      const list = a.tools ? toolList(a.tools.map((t) => [t.name, t.count] as [string, number])) : "";
      const title = list ? `${base}\n${list}` : base;
      const barOpacity = eventsMode ? ` opacity="0.22"` : "";
      const ticks = eventsMode ? tickMarksHtml(a.toolEvents, barY, BAR_H, xFor) : "";
      return `
        <g class="timeline-row" data-agent-id="${escapeHtml(a.id)}">
          <rect x="0" y="${rowY}" width="${WIDTH}" height="${ROW_H}" class="timeline-hit"></rect>
          <text x="0" y="${rowY + ROW_H / 2 + 4}" class="timeline-label">${escapeHtml(truncate(a.agentType || "agent", 16))}</text>
          <rect x="${barX.toFixed(1)}" y="${barY}" width="${barW.toFixed(1)}" height="${BAR_H}" rx="0" class="timeline-bar${runCls}" fill="${modelColor(a.model)}"${barOpacity}><title>${escapeHtml(title)}</title></rect>
          ${ticks}
        </g>`;
    })
    .join("");

  const axisY = TOP + rows * ROW_H + 12;
  const axis = `
    <line x1="${LABEL_W}" y1="${TOP + rows * ROW_H + 1}" x2="${LABEL_W + TRACK_W}" y2="${TOP + rows * ROW_H + 1}" class="timeline-axis"></line>
    <text x="${LABEL_W}" y="${axisY}" class="timeline-axis-label">${escapeHtml(formatAbsoluteTime(session.startedAt))}</text>
    <text x="${LABEL_W + TRACK_W}" y="${axisY}" class="timeline-axis-label" text-anchor="end">${escapeHtml(formatAbsoluteTime(session.endedAt))}</text>`;

  let caption: string;
  let legend: string;
  if (eventsMode) {
    caption =
      `ticks = individual tool calls · ${shownEvents}` +
      (droppedEvents > 0 ? ` of ${shownEvents + droppedEvents} (downsampled)` : "");
    const kinds: string[] = [];
    for (const e of session.mainToolEvents ?? []) {
      const k = toolKind(e.name);
      if (!kinds.includes(k)) kinds.push(k);
    }
    for (const a of agents) {
      for (const e of a.toolEvents ?? []) {
        const k = toolKind(e.name);
        if (!kinds.includes(k)) kinds.push(k);
      }
    }
    legend = `<span class="timeline-legend">${kinds
      .map((k) => `<i class="timeline-swatch" style="background:${kindColor(k)}"></i>${escapeHtml(k)}`)
      .join(" ")}</span>`;
  } else {
    caption =
      total > 0
        ? `shading = main-thread tool calls per slot · ${total} total${agents.length ? " · rows below = subagent run spans" : ""}`
        : agents.length
          ? "rows = subagent run spans"
          : "no tool calls on the main thread";
    legend =
      total > 0
        ? `<span class="timeline-legend">less${HEAT_OPACITY.map(
            (op) => `<i class="timeline-swatch" style="background:${mainColor};opacity:${op}"></i>`,
          ).join("")}more</span>`
        : "";
  }

  // A red rule across the lane means nothing without being named; say it only
  // when there is one, so a clean session's caption is untouched.
  const interrupts = session.interruptTimes?.length ?? 0;
  if (interrupts > 0) {
    caption += ` · red = you stopped it (${interrupts}×)`;
  }

  const toggle =
    shownEvents > 0
      ? `<button type="button" class="timeline-events-btn${eventsMode ? " timeline-events-on" : ""}" title="per-tool event ticks">events</button>`
      : "";

  return `
    <svg class="timeline-svg" viewBox="0 0 ${WIDTH} ${height}" width="${WIDTH}" height="${height}" xmlns="${SVG_NS}">
      <g class="timeline-rows">${mainG}${agentRows}</g>
      ${axis}
    </svg>
    <div class="timeline-foot">
      <span class="timeline-caption">${escapeHtml(caption)}</span>
      ${legend}
      ${toggle}
    </div>`;
}
