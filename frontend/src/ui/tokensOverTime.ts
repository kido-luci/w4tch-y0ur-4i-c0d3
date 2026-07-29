// "Tokens over time" — daily token usage by model, as a smoothed dual-area +
// per-family line chart with a dashed moving-average trend and a peak-day
// callout. Ported from the admin blog's analytics card (which is itself
// hand-rolled SVG — Chart.js couldn't do the look), re-themed for this app's
// dark palette and fed from the loaded session list instead of a usage table.
//
// Data source: each session's per-model breakdown bucketed into its local start
// date (same start-date bucketing the heatmap uses), so no backend change is
// needed. The card owns its Trend / Annotations / window controls and redraws
// from the last aggregate on toggle — call `update()` to feed fresh sessions.

import type { ModelUsage, Session } from "../api";
import { escapeHtml, formatTokens, modelColor } from "../domain/format";

// Known families first (stable order + colors), then any others by tokens desc.
const MODEL_ORDER = ["opus", "sonnet", "haiku", "fable"];
const ALL_LINE = "#8b929e"; // neutral grey for the "All" total (== --text-muted)

const MONTHS = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

interface Agg {
  dates: string[]; // continuous YYYY-MM-DD across the window
  byDate: number[]; // total tokens per date (index-aligned with `dates`)
  series: Record<string, number[]>; // family -> per-date tokens
  byModel: Map<string, number>; // family -> total tokens
  families: string[]; // ordered for legend/lines
  total: number;
  peakIndex: number;
  peakValue: number;
  topModel: string; // family with the most tokens overall
}

/** Local YYYY-MM-DD key, matching the server's local-zone day bucketing. */
function ymd(d: Date): string {
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${d.getFullYear()}-${m}-${day}`;
}

/** Every date from `start` to `end` inclusive (local calendar days). */
function dateRange(start: string, end: string): string[] {
  const [sy, sm, sd] = start.split("-").map(Number);
  const [ey, em, ed] = end.split("-").map(Number);
  const cur = new Date(sy, sm - 1, sd);
  const last = new Date(ey, em - 1, ed);
  const out: string[] = [];
  // Guard against a pathological range blowing up the point count.
  for (let i = 0; cur.getTime() <= last.getTime() && i < 800; i++) {
    out.push(ymd(cur));
    cur.setDate(cur.getDate() + 1);
  }
  return out;
}

function orderedFamilies(byModel: Map<string, number>): string[] {
  const present = [...byModel.keys()];
  const known = MODEL_ORDER.filter((m) => byModel.has(m));
  const extra = present
    .filter((m) => !MODEL_ORDER.includes(m))
    .sort((a, b) => (byModel.get(b) ?? 0) - (byModel.get(a) ?? 0));
  return [...known, ...extra];
}

/**
 * Bucket sessions by local start date into a continuous per-day, per-model
 * series. `windowDays` (0 = all time) sets the date domain so idle days show as
 * dips to zero; the domain always stretches to cover the earliest data point.
 */
function aggregate(sessions: Session[], windowDays: number): Agg | null {
  const byDateModel = new Map<string, Map<string, number>>();
  const byModel = new Map<string, number>();
  let minDate: string | null = null;

  for (const s of sessions) {
    const breakdown: ModelUsage[] = s.modelBreakdown ?? [];
    if (breakdown.length === 0) continue;
    const date = ymd(new Date(s.startedAt));
    if (minDate === null || date < minDate) minDate = date;
    let dm = byDateModel.get(date);
    if (!dm) {
      dm = new Map();
      byDateModel.set(date, dm);
    }
    for (const mu of breakdown) {
      if (mu.tokens <= 0) continue;
      dm.set(mu.model, (dm.get(mu.model) ?? 0) + mu.tokens);
      byModel.set(mu.model, (byModel.get(mu.model) ?? 0) + mu.tokens);
    }
  }

  if (byModel.size === 0) return null;

  const today = ymd(new Date());
  let start: string;
  if (windowDays > 0) {
    const d = new Date();
    d.setDate(d.getDate() - (windowDays - 1));
    start = ymd(d);
    if (minDate && minDate < start) start = minDate; // never clip real data
  } else {
    start = minDate ?? today;
  }
  const dates = dateRange(start, today);

  const families = orderedFamilies(byModel);
  const series: Record<string, number[]> = {};
  families.forEach((m) => {
    series[m] = dates.map((d) => byDateModel.get(d)?.get(m) ?? 0);
  });
  const byDate = dates.map((d) => {
    const dm = byDateModel.get(d);
    if (!dm) return 0;
    let sum = 0;
    for (const v of dm.values()) sum += v;
    return sum;
  });

  let total = 0;
  for (const v of byModel.values()) total += v;

  let peakIndex = 0;
  let peakValue = -1;
  byDate.forEach((v, i) => {
    if (v > peakValue) {
      peakValue = v;
      peakIndex = i;
    }
  });

  const topModel = families.reduce(
    (best, m) => ((byModel.get(m) ?? 0) > (byModel.get(best) ?? -1) ? m : best),
    families[0],
  );

  return { dates, byDate, series, byModel, families, total, peakIndex, peakValue, topModel };
}

// ── formatting helpers ───────────────────────────────────────────────────────

function shortDate(iso: string): string {
  const p = iso.split("-");
  const m = Number(p[1]);
  const d = Number(p[2]);
  return m >= 1 && m <= 12 && d ? `${MONTHS[m - 1]} ${d}` : iso;
}

function pct(x: number, total: number): string {
  return total > 0 ? Math.round((x / total) * 100) + "%" : "0%";
}

function capitalize(s: string): string {
  return s ? s[0].toUpperCase() + s.slice(1) : s;
}

/** Compact token count with the unit suffix wrapped for smaller styling. */
function compactUnitHtml(n: number): string {
  const s = formatTokens(n);
  const m = /^(-?[\d.]+)([kMB])$/.exec(s);
  return m ? `${m[1]}<span class="tot-unit">${m[2]}</span>` : escapeHtml(s);
}

/** Nice axis ceiling for a value → 1 / 2 / 2.5 / 5 × 10ⁿ. */
function niceCeil(v: number): number {
  if (v <= 0) return 1;
  const exp = Math.floor(Math.log10(v));
  const base = Math.pow(10, exp);
  const f = v / base;
  const nf = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return nf * base;
}

// ── SVG builder ──────────────────────────────────────────────────────────────

interface Controls {
  showTrend: boolean;
  showAnnotations: boolean;
  smaWindow: number;
}

function buildSvg(a: Agg, c: Controls): string {
  const { dates, byDate, series, families, peakIndex, peakValue, topModel } = a;
  const N = dates.length;
  const topColor = modelColor(topModel);

  // scales (viewBox 1344 × 504)
  const padL = 64, padR = 28, padT = 44, padB = 40;
  const plotW = 1344 - padL - padR;
  const plotH = 504 - padT - padB;
  const yMax = niceCeil(peakValue);
  const X = (i: number): number => (N > 1 ? padL + plotW * (i / (N - 1)) : padL + plotW / 2);
  const Y = (v: number): number => padT + plotH * (1 - v / yMax);
  const baseY = Y(0);
  const r2 = (n: number): number => Math.round(n * 100) / 100;

  // smoothing: Catmull-Rom → cubic bézier (tension + clamp to the plot band)
  const tension = 0.22;
  const k = (1 - tension) / 6;
  const top = (arr: number[]): string => {
    const p = arr.map((v, i) => ({ x: X(i), y: Y(v) }));
    if (p.length === 1) return `M ${r2(p[0].x)} ${r2(p[0].y)}`;
    let d = "M " + r2(p[0].x) + " " + r2(p[0].y);
    for (let i = 0; i < p.length - 1; i++) {
      const p0 = p[i - 1] || p[i];
      const p1 = p[i];
      const p2 = p[i + 1];
      const p3 = p[i + 2] || p[i + 1];
      let c1y = p1.y + (p2.y - p0.y) * k;
      let c2y = p2.y - (p3.y - p1.y) * k;
      c1y = Math.max(padT, Math.min(baseY, c1y));
      c2y = Math.max(padT, Math.min(baseY, c2y));
      d +=
        " C " + r2(p1.x + (p2.x - p0.x) * k) + " " + r2(c1y) + " " +
        r2(p2.x - (p3.x - p1.x) * k) + " " + r2(c2y) + " " + r2(p2.x) + " " + r2(p2.y);
    }
    return d;
  };
  const area = (arr: number[]): string =>
    N > 1
      ? top(arr) + " L " + r2(X(N - 1)) + " " + r2(baseY) + " L " + r2(X(0)) + " " + r2(baseY) + " Z"
      : "";

  // trailing moving average over the "All" series
  const W = c.smaWindow;
  const ma = byDate.map((_, i) => {
    let s = 0, count = 0;
    for (let j = Math.max(0, i - W + 1); j <= i; j++) {
      s += byDate[j];
      count++;
    }
    return s / count;
  });

  // gridlines (major + minor) + axis labels
  const STEPS = 5;
  let grid = "";
  for (let s = 0; s <= STEPS; s++) {
    const y = r2(Y((yMax / STEPS) * s));
    const stroke = s === 0 ? "var(--border-hover)" : "var(--border)";
    grid += `<line x1="${padL}" y1="${y}" x2="${padL + plotW}" y2="${y}" stroke="${stroke}" stroke-width="1"></line>`;
    if (s < STEPS) {
      const ym = r2(Y((yMax / STEPS) * (s + 0.5)));
      grid += `<line x1="${padL}" y1="${ym}" x2="${padL + plotW}" y2="${ym}" stroke="var(--border)" stroke-width="1" stroke-opacity="0.4"></line>`;
    }
  }
  let yLabels = "";
  for (let s = 0; s <= STEPS; s++) {
    const v = (yMax / STEPS) * s;
    yLabels += `<text x="${padL - 10}" y="${r2(Y(v) + 4)}">${escapeHtml(formatTokens(v))}</text>`;
  }
  const labelEvery = Math.max(1, Math.ceil(N / 10));
  let xLabels = "";
  for (let i = 0; i < N; i += labelEvery) {
    xLabels += `<text x="${r2(X(i))}" y="${baseY + 26}">${escapeHtml(shortDate(dates[i]))}</text>`;
  }

  // latest-day highlight band
  let band = "";
  if (N > 1) {
    const dayW = X(1) - X(0);
    const bx = r2(Math.max(padL, X(N - 1) - dayW * 0.6));
    const bw = r2(Math.min(padL + plotW, X(N - 1) + dayW * 0.6) - bx);
    band = `<rect x="${bx}" y="${padT}" width="${bw}" height="${plotH}" fill="#ffffff" opacity="0.04"></rect>`;
  }

  // area fills (All + top model)
  let areas = `<path d="${area(byDate)}" fill="url(#totAllGrad)" stroke="none"></path>`;
  areas += `<path d="${area(series[topModel])}" fill="url(#totTopGrad)" stroke="none"></path>`;

  // lines: All, then non-top families, then the top model on top
  let lines = `<path d="${top(byDate)}" fill="none" stroke="${ALL_LINE}" stroke-width="1.5" stroke-linejoin="round" stroke-linecap="round"></path>`;
  families
    .filter((m) => m !== topModel)
    .forEach((m) => {
      lines += `<path d="${top(series[m])}" fill="none" stroke="${modelColor(m)}" stroke-width="1.9" stroke-linejoin="round" stroke-linecap="round"></path>`;
    });
  lines += `<path d="${top(series[topModel])}" fill="none" stroke="${topColor}" stroke-width="2.5" stroke-linejoin="round" stroke-linecap="round"></path>`;

  // dots on the All-line (skip when too dense)
  let dots = "";
  if (N <= 45) {
    byDate.forEach((v, i) => {
      dots += `<circle cx="${r2(X(i))}" cy="${r2(Y(v))}" r="2.2" fill="var(--card)" stroke="var(--border-hover)" stroke-width="1.2"></circle>`;
    });
  }

  // dashed moving-average trend
  const trend = c.showTrend
    ? `<path d="${top(ma)}" fill="none" stroke="var(--text-muted)" stroke-width="2" stroke-dasharray="1 5.5" stroke-linecap="round" stroke-linejoin="round" opacity="0.82"></path>`
    : "";

  // peak-day callout (top-2 models on the peak day)
  let annot = "";
  if (c.showAnnotations && N > 1 && peakValue > 0) {
    const px = r2(X(peakIndex));
    const py = r2(Y(peakValue));
    const boxW = 250, boxH = 86, gap = 16;
    const clamp = (v: number, lo: number, hi: number): number => Math.max(lo, Math.min(hi, v));
    const onRight = px > padL + plotW / 2;
    const boxX = r2(clamp(onRight ? px - boxW - gap : px + gap, padL, 1344 - padR - boxW));
    const boxY = r2(clamp(py - boxH - gap, 6, baseY - boxH));
    const anchorX = r2(onRight ? boxX + boxW : boxX);
    const anchorY = r2(clamp(py, boxY + 8, boxY + boxH - 8));

    const dayModels = a.series;
    const top2 = families
      .map((m) => [m, dayModels[m][peakIndex]] as [string, number])
      .filter(([, v]) => v > 0)
      .sort((x, y) => y[1] - x[1])
      .slice(0, 2);
    let chips = "";
    top2.forEach(([name, val], idx) => {
      const cx = boxX + 18 + idx * 120;
      chips +=
        `<circle cx="${cx}" cy="${boxY + 70}" r="4" fill="${modelColor(name)}"></circle>` +
        `<text x="${cx + 11}" y="${boxY + 74}" font-size="12.5" font-weight="600" fill="var(--text)">${escapeHtml(capitalize(name))} ${escapeHtml(formatTokens(val))}</text>`;
    });
    annot =
      `<line x1="${px}" y1="${py}" x2="${anchorX}" y2="${anchorY}" stroke="var(--border-hover)" stroke-width="1.2"></line>` +
      `<circle cx="${px}" cy="${py}" r="4.5" fill="${topColor}" stroke="var(--card)" stroke-width="2"></circle>` +
      `<rect x="${boxX}" y="${boxY}" width="${boxW}" height="${boxH}" rx="0" fill="var(--bg)" stroke="var(--border)" stroke-width="2"></rect>` +
      `<text x="${boxX + 18}" y="${boxY + 32}" font-size="20" font-weight="800" fill="var(--text)">${escapeHtml(formatTokens(peakValue))} tokens</text>` +
      `<text x="${boxX + 18}" y="${boxY + 52}" font-size="12.5" font-weight="500" fill="var(--text-muted)">Single-day peak · ${escapeHtml(shortDate(dates[peakIndex]))}</text>` +
      chips;
  }

  return (
    `<svg viewBox="0 0 1344 504" class="tot-svg" xmlns="http://www.w3.org/2000/svg">` +
    `<defs>` +
    `<linearGradient id="totAllGrad" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${ALL_LINE}" stop-opacity="0.18"></stop><stop offset="1" stop-color="${ALL_LINE}" stop-opacity="0.02"></stop></linearGradient>` +
    `<linearGradient id="totTopGrad" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="${topColor}" stop-opacity="0.20"></stop><stop offset="1" stop-color="${topColor}" stop-opacity="0.02"></stop></linearGradient>` +
    `</defs>` +
    band +
    `<g>${grid}</g>` +
    `<g class="tot-axis" text-anchor="end">${yLabels}</g>` +
    `<g class="tot-axis" text-anchor="middle">${xLabels}</g>` +
    areas + lines + dots + trend + annot +
    `</svg>`
  );
}

// ── controller ───────────────────────────────────────────────────────────────

export interface TokensOverTime {
  el: HTMLElement;
  update(sessions: Session[], windowDays: number): void;
}

/**
 * Build the "tokens over time" card. `el` mounts once; feed it the current
 * session list via `update()` on every refresh / live event. Trend, Annotations
 * and the moving-average window are owned here and survive updates.
 */
export function createTokensOverTime(): TokensOverTime {
  const state: Controls = { showTrend: true, showAnnotations: true, smaWindow: 7 };
  let agg: Agg | null = null;

  const el = document.createElement("div");
  el.className = "tot";
  el.innerHTML = `
    <div class="tot-head">
      <div class="tot-head-titles">
        <h2 class="section-heading tot-title">tokens over time</h2>
        <div class="tot-sub">daily token usage by model</div>
        <!-- This card is fed by its own unfiltered fetch (see the getSessions(0)
             in sessions.ts) so it stays a global baseline to read the filtered
             panels against. That is deliberate, but it put "opus 100%" in the
             distribution card one scroll under "opus 66%" here, and 722.4k
             tokens in a stat card against 18.9B in this one, with nothing
             saying why. Say it. -->
        <div class="panel-scope">all projects · all time — not affected by the scope or the filters</div>
      </div>
      <div class="tot-legend"></div>
    </div>
    <div class="tot-controls">
      <button type="button" class="tot-toggle tot-on" data-tot="trend">Trend</button>
      <button type="button" class="tot-toggle tot-on" data-tot="annot">Annotations</button>
      <label class="tot-range">
        <span>Trend window</span>
        <input type="range" min="3" max="14" step="1" value="7" data-tot="window" />
        <span class="tot-range-val">7d</span>
      </label>
    </div>
    <div class="tot-kpis"></div>
    <div class="tot-chart"><div class="empty-state">loading…</div></div>
    <div class="tot-foot"></div>`;

  const subEl = el.querySelector<HTMLElement>(".tot-sub")!;
  const legendEl = el.querySelector<HTMLElement>(".tot-legend")!;
  const kpisEl = el.querySelector<HTMLElement>(".tot-kpis")!;
  const chartEl = el.querySelector<HTMLElement>(".tot-chart")!;
  const footEl = el.querySelector<HTMLElement>(".tot-foot")!;
  const trendBtn = el.querySelector<HTMLButtonElement>('[data-tot="trend"]')!;
  const annotBtn = el.querySelector<HTMLButtonElement>('[data-tot="annot"]')!;
  const windowInput = el.querySelector<HTMLInputElement>('[data-tot="window"]')!;
  const rangeVal = el.querySelector<HTMLElement>(".tot-range-val")!;

  trendBtn.addEventListener("click", () => {
    state.showTrend = !state.showTrend;
    trendBtn.classList.toggle("tot-on", state.showTrend);
    redraw();
  });
  annotBtn.addEventListener("click", () => {
    state.showAnnotations = !state.showAnnotations;
    annotBtn.classList.toggle("tot-on", state.showAnnotations);
    redraw();
  });
  windowInput.addEventListener("input", () => {
    state.smaWindow = Math.max(3, Math.min(14, parseInt(windowInput.value, 10) || 7));
    rangeVal.textContent = state.smaWindow + "d";
    redraw();
  });

  function clear(): void {
    subEl.textContent = "daily token usage by model";
    legendEl.innerHTML = "";
    kpisEl.innerHTML = "";
    footEl.innerHTML = "";
    chartEl.innerHTML = `<div class="empty-state">no token usage in this window</div>`;
  }

  function redraw(): void {
    if (!agg) {
      clear();
      return;
    }
    const a = agg;
    const N = a.dates.length;

    chartEl.innerHTML = buildSvg(a, state);

    const year = a.dates[N - 1].split("-")[0];
    subEl.textContent =
      `daily token usage by model · ${shortDate(a.dates[0])} – ${shortDate(a.dates[N - 1])}, ${year}`;

    // legend: All + per-family share
    let legend =
      `<span class="tot-leg-item"><span class="tot-leg-sw" style="background:${ALL_LINE}"></span>` +
      `<span class="tot-leg-name">All</span></span>`;
    a.families.forEach((m) => {
      legend +=
        `<span class="tot-leg-item"><span class="tot-leg-sw" style="background:${modelColor(m)}"></span>` +
        `<span class="tot-leg-name">${escapeHtml(m)}</span>` +
        `<span class="tot-leg-pct">${pct(a.byModel.get(m) ?? 0, a.total)}</span></span>`;
    });
    legendEl.innerHTML = legend;

    // KPI tiles
    const dayCount = a.dates.length;
    const kpi = (label: string, valHtml: string, note: string): string =>
      `<div class="tot-kpi"><div class="tot-kpi-label">${escapeHtml(label)}</div>` +
      `<div class="tot-kpi-val">${valHtml}</div>` +
      `<div class="tot-kpi-note">${escapeHtml(note)}</div></div>`;
    kpisEl.innerHTML =
      kpi(`total · ${dayCount} days`, compactUnitHtml(a.total), "across all models") +
      kpi("single-day peak", compactUnitHtml(a.peakValue), shortDate(a.dates[a.peakIndex])) +
      kpi("daily average", compactUnitHtml(a.total / N), "tokens / day") +
      kpi("opus share", pct(a.byModel.get("opus") ?? 0, a.total), "of all tokens");

    // footer note (only meaningful while the trend line shows)
    footEl.innerHTML = state.showTrend
      ? `<span class="tot-foot-dash"></span>` +
        `<span class="tot-foot-text">${state.smaWindow}-day moving average (trend line)</span>`
      : "";
  }

  function update(sessions: Session[], windowDays: number): void {
    agg = aggregate(sessions, windowDays);
    redraw();
  }

  return { el, update };
}
