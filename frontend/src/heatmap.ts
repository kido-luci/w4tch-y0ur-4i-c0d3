// GitHub-style contribution heatmap: one cell per calendar day, columns are
// weeks (Sunday-start) and rows are weekdays. Cell intensity tracks the number
// of sessions started that day; the tooltip carries tokens + cost too. Built as
// an innerHTML SVG string like the agent graph; all dynamic text is escaped.

import type { Activity, ActivityDay } from "./api";
import { escapeHtml, formatCost, formatTokens } from "./format";

const SVG_NS = "http://www.w3.org/2000/svg";

const CELL = 11;
const GAP = 3;
const STEP = CELL + GAP;
const LEFT_GUTTER = 26; // room for weekday labels
const TOP_GUTTER = 16; // room for month labels
const BOTTOM_PAD = 22; // room for the legend

/** Local YYYY-MM-DD key, matching the server's local-zone bucketing. */
function ymd(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}

/**
 * 0..4 intensity level, scaled relative to the window's busiest day so the ramp
 * adapts to each user (a 2-sessions/day and a 20-sessions/day user both read
 * well). 0 = no activity.
 */
function levelFor(count: number, max: number): number {
  if (count <= 0) return 0;
  if (max <= 0) return 1;
  return Math.min(4, Math.max(1, Math.ceil((count / max) * 4)));
}

function cellTitle(d: Date, day: ActivityDay | undefined): string {
  const label = d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
  if (!day || day.sessions === 0) return `${label} · no activity`;
  const plural = day.sessions === 1 ? "session" : "sessions";
  return `${label} · ${day.sessions} ${plural} · ${formatTokens(day.tokens)} tok · ${formatCost(day.costUsd)}`;
}

/** Render the activity heatmap into a scrollable wrapper div. */
export function renderActivityHeatmap(activity: Activity): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "heatmap-wrapper";

  const weeks = activity.weeks;
  const byDate = new Map<string, ActivityDay>();
  let maxSessions = 0;
  for (const d of activity.days) {
    byDate.set(d.date, d);
    if (d.sessions > maxSessions) maxSessions = d.sessions;
  }

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  // Top-left cell = the Sunday (weeks-1) weeks before this week's Sunday.
  const start = new Date(today);
  start.setDate(start.getDate() - today.getDay() - (weeks - 1) * 7);

  const width = LEFT_GUTTER + weeks * STEP - GAP;
  const height = TOP_GUTTER + 7 * STEP - GAP + BOTTOM_PAD;

  let cells = "";
  let months = "";
  let lastMonthLabelCol = -2;
  let lastMonth = -1;

  for (let col = 0; col < weeks; col++) {
    for (let row = 0; row < 7; row++) {
      const date = new Date(start);
      date.setDate(start.getDate() + col * 7 + row);
      if (date.getTime() > today.getTime()) continue; // future cell in the current week

      const day = byDate.get(ymd(date));
      const level = levelFor(day?.sessions ?? 0, maxSessions);
      const x = LEFT_GUTTER + col * STEP;
      const y = TOP_GUTTER + row * STEP;
      cells +=
        `<rect x="${x}" y="${y}" width="${CELL}" height="${CELL}" rx="0" class="heat-cell heat-l${level}">` +
        `<title>${escapeHtml(cellTitle(date, day))}</title></rect>`;

      // Month label above the first column that starts a new month (row 0).
      if (row === 0) {
        const m = date.getMonth();
        if (m !== lastMonth && col - lastMonthLabelCol >= 3) {
          const label = date.toLocaleDateString("en-US", { month: "short" });
          months += `<text x="${x}" y="11" class="heat-month">${escapeHtml(label)}</text>`;
          lastMonthLabelCol = col;
        }
        lastMonth = m;
      }
    }
  }

  // Weekday labels: Mon / Wed / Fri (rows 1, 3, 5).
  const weekdayLabels = [
    [1, "Mon"],
    [3, "Wed"],
    [5, "Fri"],
  ] as const;
  let weekdays = "";
  for (const [row, label] of weekdayLabels) {
    const y = TOP_GUTTER + row * STEP + CELL - 1;
    weekdays += `<text x="0" y="${y}" class="heat-weekday">${label}</text>`;
  }

  // Legend: "less ▢▢▢▢▢ more", bottom-right.
  const legendY = TOP_GUTTER + 7 * STEP + 4;
  let legend = `<text x="${width - 106}" y="${legendY + 9}" class="heat-legend-text" text-anchor="end">less</text>`;
  for (let l = 0; l <= 4; l++) {
    const x = width - 100 + l * (CELL + 2);
    legend += `<rect x="${x}" y="${legendY}" width="${CELL}" height="${CELL}" rx="0" class="heat-cell heat-l${l}"></rect>`;
  }
  legend += `<text x="${width - 100 + 5 * (CELL + 2) + 4}" y="${legendY + 9}" class="heat-legend-text">more</text>`;

  wrapper.innerHTML = `
    <svg class="heatmap-svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" xmlns="${SVG_NS}">
      <g class="heat-months">${months}</g>
      <g class="heat-weekdays">${weekdays}</g>
      <g class="heat-cells">${cells}</g>
      <g class="heat-legend">${legend}</g>
    </svg>`;

  return wrapper;
}
