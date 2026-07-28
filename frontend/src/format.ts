// Number / duration / date formatting helpers shared by both views.

/** Strip a trailing ".0" from a fixed-decimal string, e.g. "1.0k" -> "1k". */
function trimTrailingZero(value: string): string {
  return value.replace(/\.0([a-zA-Z%]*)$/, "$1");
}

/** Compact token/count formatting: 1.2k, 3.4M, 1.2B, or the raw number under 1000. */
export function formatTokens(n: number): string {
  const abs = Math.abs(n);
  if (abs < 1000) return String(Math.round(n));
  if (abs < 1_000_000) return trimTrailingZero((n / 1_000).toFixed(1)) + "k";
  if (abs < 1_000_000_000) return trimTrailingZero((n / 1_000_000).toFixed(1)) + "M";
  return trimTrailingZero((n / 1_000_000_000).toFixed(1)) + "B";
}

/** Compact cost formatting: $0.42, $12.50, $1,204 (>=100 drops decimals). */
export function formatCost(usd: number): string {
  const abs = Math.abs(usd);
  if (abs >= 100) {
    return "$" + Math.round(usd).toLocaleString("en-US");
  }
  return "$" + usd.toFixed(2);
}

/** Duration formatting from milliseconds: 42s, 12m 05s, 1h 12m, 2d 4h. */
export function formatDuration(ms: number): string {
  if (ms < 0) ms = 0;
  const totalSeconds = Math.floor(ms / 1000);
  const days = Math.floor(totalSeconds / 86400);
  const hours = Math.floor((totalSeconds % 86400) / 3600);
  const minutes = Math.floor((totalSeconds % 3600) / 60);
  const seconds = totalSeconds % 60;

  if (days > 0) return `${days}d ${hours}h`;
  if (hours > 0) return `${hours}h ${minutes}m`;
  if (minutes > 0) return `${minutes}m ${String(seconds).padStart(2, "0")}s`;
  return `${seconds}s`;
}

/** Relative time from an ISO timestamp: 12s ago, 5m ago, 3h ago, 2d ago, else YYYY-MM-DD. */
export function formatRelativeTime(iso: string): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return ""; // empty/unparseable date — never throw on the toISOString fallback
  const now = Date.now();
  const diffMs = now - then;
  const diffSeconds = Math.floor(diffMs / 1000);

  if (diffSeconds < 60) return `${Math.max(diffSeconds, 0)}s ago`;
  const diffMinutes = Math.floor(diffSeconds / 60);
  if (diffMinutes < 60) return `${diffMinutes}m ago`;
  const diffHours = Math.floor(diffMinutes / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  const diffDays = Math.floor(diffHours / 24);
  if (diffDays < 7) return `${diffDays}d ago`;

  const d = new Date(iso);
  return d.toISOString().slice(0, 10);
}

/** Absolute local time formatting: "Jul 14, 15:42". */
/**
 * A timestamp's calendar day in the VIEWER's zone, YYYY-MM-DD.
 *
 * Not `iso.slice(0, 10)`: that reads the day off the UTC string, so a cycle
 * starting at local midnight east of Greenwich renders as the day before the
 * one the user picked. Only correct by accident for instants already stored at
 * UTC midnight.
 */
export function formatDay(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso.slice(0, 10);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
}

export function formatAbsoluteTime(iso: string): string {
  const d = new Date(iso);
  const month = d.toLocaleString("en-US", { month: "short" });
  const day = d.getDate();
  const hh = String(d.getHours()).padStart(2, "0");
  const mm = String(d.getMinutes()).padStart(2, "0");
  return `${month} ${day}, ${hh}:${mm}`;
}

/** Truncate a string to `max` chars, appending an ellipsis if truncated. */
export function truncate(text: string, max: number): string {
  if (text.length <= max) return text;
  return text.slice(0, Math.max(0, max - 1)).trimEnd() + "…";
}

/** Escape a string for safe insertion into innerHTML. */
export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#39;");
}

const MODEL_COLORS: Record<string, string> = {
  opus: "#7c5cff",
  sonnet: "#2dd4bf",
  haiku: "#f59e0b",
  fable: "#ec4899",
};

const DEFAULT_MODEL_COLOR = "#9ca3af";

/** Exact model-family accent color; unknown families fall back to gray. */
export function modelColor(family: string): string {
  return MODEL_COLORS[family.toLowerCase()] ?? DEFAULT_MODEL_COLOR;
}

/** Small colored pill markup for a model family badge (15% opacity bg, full-color text + dot). */
export function modelBadgeHtml(family: string): string {
  const color = modelColor(family);
  const label = escapeHtml(family);
  return (
    `<span class="model-badge" style="--model-color:${color}">` +
    `<span class="model-dot"></span>${label}</span>`
  );
}

/** "+120 −8" markup for changed lines (green adds, red dels); "—" when none. */
export function linesBadgeHtml(added: number, removed: number): string {
  if (!added && !removed) return "—";
  return (
    `<span class="lines-add">+${added.toLocaleString("en-US")}</span> ` +
    `<span class="lines-del">−${removed.toLocaleString("en-US")}</span>`
  );
}

/** Compact tool-usage summary, e.g. "18R 16B 2E"; "—" when there's no data. */
export function formatToolStats(stats: {
  readCount: number;
  searchCount: number;
  bashCount: number;
  editFileCount: number;
  otherToolCount: number;
} | null): string {
  if (!stats) return "—";
  const parts: string[] = [];
  if (stats.readCount) parts.push(`${stats.readCount}R`);
  if (stats.searchCount) parts.push(`${stats.searchCount}S`);
  if (stats.bashCount) parts.push(`${stats.bashCount}B`);
  if (stats.editFileCount) parts.push(`${stats.editFileCount}E`);
  if (stats.otherToolCount) parts.push(`${stats.otherToolCount}O`);
  return parts.length ? parts.join(" ") : "—";
}
