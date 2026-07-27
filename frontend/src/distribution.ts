// Per-model token distribution: a 100% stacked bar split by model family with a
// legend (tokens · share · cost). Reuses the app's model colors so a family
// reads the same here as on its badges. Div-based so it fills its card width.

import type { ModelUsage } from "./api";
import { escapeHtml, formatCost, formatTokens, modelColor } from "./format";

/** Render a token-distribution bar + legend from per-model usage. */
export function renderModelDistribution(usage: ModelUsage[]): HTMLElement {
  const wrapper = document.createElement("div");
  wrapper.className = "dist";

  const rows = usage.filter((u) => u.tokens > 0).sort((a, b) => b.tokens - a.tokens);
  const total = rows.reduce((sum, u) => sum + u.tokens, 0);
  if (total === 0) {
    wrapper.innerHTML = `<div class="empty-state">no token usage yet</div>`;
    return wrapper;
  }

  const segs = rows
    .map((u) => {
      const pct = (u.tokens / total) * 100;
      const title = `${u.model} · ${formatTokens(u.tokens)} tok · ${pct.toFixed(1)}% · ${formatCost(u.costUsd)}`;
      return `<div class="dist-seg" style="width:${pct.toFixed(2)}%;background:${modelColor(u.model)}" title="${escapeHtml(title)}"></div>`;
    })
    .join("");

  const legend = rows
    .map((u) => {
      const pct = (u.tokens / total) * 100;
      return `
        <div class="dist-legend-row">
          <span class="dist-dot" style="background:${modelColor(u.model)}"></span>
          <span class="dist-name">${escapeHtml(u.model)}</span>
          <span class="dist-meta">${escapeHtml(formatTokens(u.tokens))} · ${pct.toFixed(0)}%</span>
          <span class="dist-cost">${escapeHtml(formatCost(u.costUsd))}</span>
        </div>`;
    })
    .join("");

  wrapper.innerHTML = `
    <div class="dist-bar">${segs}</div>
    <div class="dist-legend">${legend}</div>`;
  return wrapper;
}
