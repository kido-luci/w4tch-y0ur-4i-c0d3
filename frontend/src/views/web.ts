// View 8 — service analytics (routes `/service/cloudflare` and
// `/service/search-console`; legacy `#/web` = cloudflare): what the shipped
// sites are doing out there — one external service per tab. Cloudflare zone
// traffic/security/RUM, or Google Search Console performance, fetched through
// the local read proxies (/api/cloudflare/…, /api/gsc/…). Credentials live in
// webstats.json server-side; an unconfigured section renders its setup hint
// instead of data, so the tab is safe to open on a fresh install.

import { getCloudflareAnalytics, getGSCAnalytics, getShips, getWebSites } from "../api";
import type {
  CFResult,
  CFSecEvent,
  CFTimePoint,
  GSCQueryPage,
  GSCResult,
  GSCRow,
  NameCount,
  ShipRecord,
  WebSite,
} from "../api";
import { escapeHtml, formatRelativeTime, truncate } from "../format";
import { getScopeSet } from "../scope";

const CF_RANGE_KEY = "wyac.web.cfRange";
const GSC_RANGE_KEY = "wyac.web.gscRange";

const CF_RANGES = ["24h", "72h", "30d"];
const GSC_RANGES = ["7d", "28d", "90d"];

function loadRange(key: string, valid: string[], fallback: string): string {
  try {
    const v = localStorage.getItem(key);
    if (v && valid.includes(v)) return v;
  } catch {
    /* storage unavailable — session default */
  }
  return fallback;
}

function saveRange(key: string, v: string): void {
  try {
    localStorage.setItem(key, v);
  } catch {
    /* storage unavailable — range just won't persist */
  }
}

/** Renders one service's analytics into `container`; returns a cleanup callback. */
export function renderWebView(container: HTMLElement, section: "cloudflare" | "gsc"): () => void {
  let cfRange = loadRange(CF_RANGE_KEY, CF_RANGES, "24h");
  let gscRange = loadRange(GSC_RANGE_KEY, GSC_RANGES, "28d");
  const expandedPages = new Set<string>(); // GSC pages showing their queries
  let gscData: GSCResult | null | undefined; // undefined = not loaded yet
  // Drop a slower earlier fetch when a newer range switch has gone out, so the
  // panel can't show 24h data under a lit 72h chip (the search.ts seq pattern).
  let cfSeq = 0;
  let gscSeq = 0;
  // The nav's global scope reaches this view through the repo↔site mapping in
  // webstats.json (a scope change re-renders the view, so reading it once is
  // enough). With no mapping configured the tab stays zone-wide as before.
  const scopeSet = getScopeSet();
  let sites: WebSite[] = [];
  let mappedHosts: string[] | null = null; // null = unscoped or nothing mapped
  let mappedProjects: string[] = []; // repos whose releases mark the chart

  // One card per tab now; the other section's elements simply don't exist,
  // and every wiring below guards on that.
  const cfCard = `
        <section class="card web-card">
          <div class="web-head">
            <h2 class="section-heading">cloudflare</h2>
            <div class="filter-row" id="cf-range"></div>
          </div>
          <p class="section-desc">The zone from the edge: requests and bandwidth, what the firewall
          ate, and what real browsers reported. Traffic counts every request — bots, crawlers and
          all; the RUM block is the human slice.</p>
          <div id="cf-wrap"><div class="empty-state">loading…</div></div>
        </section>`;
  const gscCard = `
        <section class="card web-card">
          <div class="web-head">
            <h2 class="section-heading">search console</h2>
            <div class="filter-row" id="gsc-range"></div>
          </div>
          <p class="section-desc">How the sites read from Google: clicks and impressions, the
          queries that surfaced them, and which pages answer which searches. Data trails real time
          by about two days — that's Google, not a bug.</p>
          <div id="gsc-wrap"><div class="empty-state">loading…</div></div>
        </section>`;
  container.innerHTML = `
    <div class="page">
      <div id="web-sections">${section === "gsc" ? gscCard : cfCard}</div>
    </div>
  `;

  const cfRangeEl = container.querySelector<HTMLElement>("#cf-range");
  const gscRangeEl = container.querySelector<HTMLElement>("#gsc-range");
  const cfWrapEl = container.querySelector<HTMLElement>("#cf-wrap");
  const gscWrapEl = container.querySelector<HTMLElement>("#gsc-wrap");

  function renderChips(el: HTMLElement, ranges: string[], active: string): void {
    el.innerHTML = ranges
      .map(
        (r) =>
          `<button type="button" class="filter-chip${r === active ? " filter-chip-on" : ""}" ` +
          `data-range="${r}">${escapeHtml(r)}</button>`,
      )
      .join("");
  }

  if (cfRangeEl) {
    renderChips(cfRangeEl, CF_RANGES, cfRange);
    cfRangeEl.addEventListener("click", (e) => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-range]");
      if (!btn) return;
      cfRange = btn.dataset["range"]!;
      saveRange(CF_RANGE_KEY, cfRange);
      renderChips(cfRangeEl, CF_RANGES, cfRange);
      void refreshCF();
    });
  }

  if (gscRangeEl) {
    renderChips(gscRangeEl, GSC_RANGES, gscRange);
    gscRangeEl.addEventListener("click", (e) => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-range]");
      if (!btn) return;
      gscRange = btn.dataset["range"]!;
      saveRange(GSC_RANGE_KEY, gscRange);
      renderChips(gscRangeEl, GSC_RANGES, gscRange);
      void refreshGSC();
    });
  }

  const unmappedScope = (): boolean => mappedHosts !== null && mappedHosts.length === 0;
  const noSiteHtml = `<div class="empty-state">no site maps to this scope — the zone belongs to other projects.</div>`;

  function cfScopeNoteHtml(): string {
    if (mappedHosts === null) return "";
    // CF's zone-traffic dataset has no host dimension — only the security and
    // RUM sections actually narrow. Say so instead of implying otherwise.
    if (mappedHosts.length === 1) {
      return `<div class="web-scope-note">scoped to ${escapeHtml(mappedHosts[0]!)} — security &amp; RUM narrowed; traffic stays zone-wide (CF has no per-host slice for it)</div>`;
    }
    return `<div class="web-scope-note">scope spans ${mappedHosts.length} hosts — whole zone shown</div>`;
  }

  async function refreshCF(): Promise<void> {
    if (!cfWrapEl) return; // the other tab is showing
    if (unmappedScope()) {
      cfWrapEl.innerHTML = noSiteHtml;
      return;
    }
    cfWrapEl.innerHTML = `<div class="empty-state">loading…</div>`;
    const mine = ++cfSeq;
    try {
      // The server's host filter fits one host; a wider scope shows the zone
      // (the union of its hosts, near enough) with a note saying so.
      const host = mappedHosts?.length === 1 ? mappedHosts[0] : undefined;
      const days = cfRange === "30d" ? 30 : cfRange === "72h" ? 3 : 1;
      const [res, releases] = await Promise.all([
        getCloudflareAnalytics(cfRange, host),
        mappedProjects.length
          ? getShips(days, mappedProjects.join(","), 200)
              .then((r) => r.ships.filter((s) => s.kind === "release" && s.exit === 0))
              .catch((): ShipRecord[] => []) // marks are an extra, never the section
          : Promise.resolve([] as ShipRecord[]),
      ]);
      if (mine !== cfSeq) return; // a newer range switch already went out
      cfWrapEl.innerHTML =
        res === null ? setupHintHtml("cloudflare") : cfScopeNoteHtml() + cfHtml(res, releases);
    } catch (err: unknown) {
      if (mine !== cfSeq) return;
      cfWrapEl.innerHTML = `<div class="empty-state">cloudflare fetch failed — see the server log.</div>`;
      console.error("failed to load cloudflare analytics", err);
    }
  }

  function renderGSC(): void {
    if (!gscWrapEl) return; // the other tab is showing
    if (gscData === undefined) return;
    if (unmappedScope()) {
      gscWrapEl.innerHTML = noSiteHtml;
      return;
    }
    gscWrapEl.innerHTML =
      gscData === null ? setupHintHtml("searchConsole") : gscHtml(gscData, expandedPages, mappedHosts);
    gscWrapEl.querySelectorAll<HTMLElement>(".web-page-row").forEach((row) => {
      row.addEventListener("click", () => {
        const page = row.dataset["page"]!;
        if (expandedPages.has(page)) expandedPages.delete(page);
        else expandedPages.add(page);
        renderGSC();
      });
    });
  }

  async function refreshGSC(): Promise<void> {
    if (!gscWrapEl) return; // the other tab is showing
    if (unmappedScope()) {
      gscWrapEl.innerHTML = noSiteHtml;
      return;
    }
    gscWrapEl.innerHTML = `<div class="empty-state">loading…</div>`;
    const mine = ++gscSeq;
    try {
      const data = await getGSCAnalytics(gscRange);
      if (mine !== gscSeq) return; // a newer range switch already went out
      gscData = data;
      // A fresh window reorders/drops pages — stale expansion would attach
      // queries to the wrong rows, so start collapsed again.
      expandedPages.clear();
      renderGSC();
    } catch (err: unknown) {
      if (mine !== gscSeq) return;
      gscWrapEl.innerHTML = `<div class="empty-state">search console fetch failed — see the server log.</div>`;
      console.error("failed to load search console analytics", err);
    }
  }

  // The mapping decides what the section fetches, so it resolves first; a
  // failed (or empty) mapping just means zone-wide, exactly as before. Both
  // refreshes are kicked; the absent tab's is a no-op via its guard.
  getWebSites()
    .catch((): WebSite[] => [])
    .then((list) => {
      sites = list;
      if (scopeSet && sites.length) {
        const mine = sites.filter((s) => scopeSet.has(s.project));
        mappedHosts = mine.map((s) => s.host);
        mappedProjects = [...new Set(mine.map((s) => s.project))];
      } else {
        mappedProjects = [...new Set(sites.map((s) => s.project))];
      }
      void refreshCF();
      void refreshGSC();
    });

  // No live subscription: both upstreams aggregate on their own delay (CF
  // minutes, GSC days) — a range click or a revisit is as fresh as it gets.
  return () => {};
}

// --- shared formatting -------------------------------------------------------

function n(v: number): string {
  return v.toLocaleString("en-US");
}

function formatBytes(b: number): string {
  if (b >= 1e12) return `${(b / 1e12).toFixed(1)} TB`;
  if (b >= 1e9) return `${(b / 1e9).toFixed(1)} GB`;
  if (b >= 1e6) return `${(b / 1e6).toFixed(1)} MB`;
  if (b >= 1e3) return `${(b / 1e3).toFixed(1)} kB`;
  return `${b} B`;
}

function fmtCtr(ctr: number): string {
  return `${(ctr * 100).toFixed(1)}%`;
}

function fmtPos(p: number): string {
  return p.toFixed(1);
}

function pct(part: number, total: number): string {
  return total > 0 ? `${((part / total) * 100).toFixed(0)}%` : "—";
}

// "Jul 13" / "Jul 13 14:00" — series bucket labels for hover titles.
function bucketLabel(ts: string): string {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const day = d.toLocaleDateString("en-US", { month: "short", day: "numeric" });
  return ts.length > 10 ? `${day} ${String(d.getHours()).padStart(2, "0")}:00` : day;
}

function statHtml(label: string, value: string, title = ""): string {
  return `
    <div class="web-stat"${title ? ` title="${escapeHtml(title)}"` : ""}>
      <div class="stat-label">${escapeHtml(label)}</div>
      <div class="stat-value">${escapeHtml(value)}</div>
    </div>`;
}

// One bar per bucket, scaled to the window max; the whole story lives in the
// hover titles, the bars are the shape. `variant` picks the fill (see CSS).
// `marks` overlays full-height ticks on the named buckets (release markers).
function barsSvg(
  values: number[],
  titles: string[],
  variant = "",
  marks: { i: number; title: string }[] = [],
): string {
  const max = Math.max(...values, 1);
  const bw = 6;
  const gap = 2;
  const h = 44;
  const w = values.length * (bw + gap);
  const bars = values
    .map((v, i) => {
      const bh = Math.max(v > 0 ? 2 : 0, Math.round((v / max) * h));
      return (
        `<rect x="${i * (bw + gap)}" y="${h - bh}" width="${bw}" height="${bh}">` +
        `<title>${escapeHtml(titles[i] ?? "")}</title></rect>`
      );
    })
    .join("");
  const ticks = marks
    .filter((m) => m.i >= 0 && m.i < values.length)
    .map(
      (m) =>
        `<rect class="web-bars-markline" x="${m.i * (bw + gap) - 1}" y="0" width="${bw + 2}" height="${h}">` +
        `<title>${escapeHtml(m.title)}</title></rect>`,
    )
    .join("");
  return `<svg class="web-bars${variant ? ` web-bars-${variant}` : ""}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" role="img">${bars}${ticks}</svg>`;
}

function topListHtml(title: string, rows: NameCount[] | null, empty: string, total?: number): string {
  const list = rows ?? [];
  // Each row's percentage is its share of the TRUE total (all requests /
  // security events / pageviews), passed in by the caller — not of the shown
  // top-N. Dividing by the visible rows made them sum to 100% and read as the
  // whole, hiding the long tail. Fall back to the shown sum only if no total.
  const denom = total ?? list.reduce((s, r) => s + r.count, 0);
  const items =
    list.length === 0
      ? `<div class="empty-state">${escapeHtml(empty)}</div>`
      : list
          .map(
            (r) => `
      <div class="web-toplist-row" title="${escapeHtml(r.name)}">
        <span class="web-toplist-name">${escapeHtml(truncate(r.name, 42))}</span>
        <span class="web-toplist-count">${n(r.count)}</span>
        <span class="web-toplist-pct">${pct(r.count, denom)}</span>
      </div>`,
          )
          .join("");
  return `
    <div class="web-toplist">
      <h3 class="web-subheading">${escapeHtml(title)}</h3>
      ${items}
    </div>`;
}

// Sections the upstream refused ride along under `errors` — surface them as a
// footnote, or a half-empty page reads as "quiet" instead of "broken".
function errorsFootnoteHtml(errors: Record<string, string> | undefined): string {
  const entries = Object.entries(errors ?? {});
  if (entries.length === 0) return "";
  const list = entries.map(([k, v]) => `${escapeHtml(k)}: ${escapeHtml(truncate(v, 120))}`).join(" · ");
  return `<div class="web-errors">unavailable — ${list}</div>`;
}

function setupHintHtml(section: "cloudflare" | "searchConsole"): string {
  const fields =
    section === "cloudflare"
      ? `"cloudflare": { "zoneId": "…", "accountId": "…", "analyticsToken": "…", "rumSiteTag": "…" }`
      : `"searchConsole": { "property": "sc-domain:…", "saKeyFile": "/path/to/service-account.json" }`;
  return `
    <div class="empty-state">not configured — add <code>${escapeHtml(fields)}</code>
    to <code>webstats.json</code> in the server's config dir and restart. Read-only
    credentials only; the browser never sees them.</div>`;
}

// --- cloudflare --------------------------------------------------------------

function cfHtml(res: CFResult, releases: ShipRecord[] = []): string {
  return (
    cfTrafficHtml(res, releases) +
    cfSecurityHtml(res) +
    cfRumHtml(res) +
    errorsFootnoteHtml(res.errors)
  );
}

// A release lands in the bucket whose start last precedes it — the tick says
// "a release shipped in this hour/day", never an invented exact position.
// Index search is order-independent: the series' sort is upstream's business.
function releaseMarks(
  series: CFTimePoint[],
  releases: ShipRecord[],
): { i: number; title: string }[] {
  if (series.length === 0 || releases.length === 0) return [];
  const starts = series.map((p) => Date.parse(p.ts));
  const byBucket = new Map<number, string[]>();
  for (const r of releases) {
    const ts = Date.parse(r.ts);
    let best = -1;
    for (let k = 0; k < starts.length; k++) {
      const s = starts[k]!;
      if (s <= ts && (best === -1 || s > starts[best]!)) best = k;
    }
    if (best < 0) continue;
    const list = byBucket.get(best) ?? [];
    list.push(`${r.project} ${r.version || "release"} · ${formatRelativeTime(r.ts)}`);
    byBucket.set(best, list);
  }
  return [...byBucket.entries()].map(([i, list]) => ({ i, title: `release: ${list.join(", ")}` }));
}

function cfSeriesTitle(p: CFTimePoint): string {
  return (
    `${bucketLabel(p.ts)} — ${n(p.requests)} requests, ${n(p.cached)} cached, ` +
    `${formatBytes(p.bytes)}, ${n(p.err4xx)}×4xx ${n(p.err5xx)}×5xx, ${n(p.uniques)} uniques`
  );
}

function cfTrafficHtml(res: CFResult, releases: ShipRecord[]): string {
  const t = res.traffic;
  if (!t) return "";
  const series = t.series ?? [];
  const marks = releaseMarks(series, releases);
  const errRows = (t.statusCodes ?? [])
    .map(
      (c) => `
      <tr>
        <td class="cell-title">${c.code}</td>
        <td>${n(c.requests)}</td>
      </tr>`,
    )
    .join("");
  return `
    <div class="web-block">
      <div class="web-stats">
        ${statHtml("requests", n(t.totals.requests))}
        ${statHtml("cached", pct(t.totals.cached, t.totals.requests), "share of requests answered from cache")}
        ${statHtml("bandwidth", formatBytes(t.totals.bytes))}
        ${statHtml("uniques", n(t.totals.uniques), "sum of per-bucket uniques — an approximation")}
        ${statHtml("threats", n(t.totals.threats))}
      </div>
      ${series.length > 0 ? barsSvg(series.map((p) => p.requests), series.map(cfSeriesTitle), "", marks) : ""}
      <div class="web-cols">
        ${topListHtml("top countries", t.topCountries, "no traffic in this window", t.totals.requests)}
        ${
          errRows
            ? `<div class="web-toplist"><h3 class="web-subheading">error status codes</h3>
               <table class="sessions-table web-table"><tbody>${errRows}</tbody></table></div>`
            : ""
        }
      </div>
    </div>`;
}

function cfSecurityHtml(res: CFResult): string {
  const s = res.security;
  if (!s) return "";
  const windowNote = `last ${s.windowHours}h`;
  if (s.total === 0) {
    return `
      <div class="web-block">
        <h3 class="web-subheading">security events · ${escapeHtml(windowNote)}</h3>
        <div class="empty-state">the firewall ate nothing in this window.</div>
      </div>`;
  }
  const recent = (s.recent ?? [])
    .map(
      (e: CFSecEvent) => `
      <tr>
        <td class="cell-muted">${escapeHtml(bucketLabel(e.datetime))}</td>
        <td class="cell-title">${escapeHtml(e.action)}</td>
        <td class="cell-muted">${escapeHtml(e.country)}</td>
        <td class="cell-muted" title="${escapeHtml(e.host)}">${escapeHtml(truncate(e.host, 28))}</td>
        <td class="cell-muted" title="${escapeHtml(e.path)}">${escapeHtml(truncate(e.path, 36))}</td>
        <td class="cell-muted" title="${escapeHtml(e.ruleId)}">${escapeHtml(truncate(e.source || e.ruleId, 20))}</td>
      </tr>`,
    )
    .join("");
  return `
    <div class="web-block">
      <h3 class="web-subheading">security events · ${n(s.total)} · ${escapeHtml(windowNote)}</h3>
      <div class="web-cols">
        ${topListHtml("by action", s.byAction, "—", s.total)}
        ${topListHtml("by country", s.byCountry, "—", s.total)}
        ${topListHtml("by rule", s.byRule, "—", s.total)}
      </div>
      ${
        recent
          ? `<table class="sessions-table web-table">
              <thead><tr><th>when</th><th>action</th><th>country</th><th>host</th><th>path</th><th>rule</th></tr></thead>
              <tbody>${recent}</tbody>
            </table>
            <div class="table-caption">most recent ${(s.recent ?? []).length} events</div>`
          : ""
      }
    </div>`;
}

function cfRumHtml(res: CFResult): string {
  const r = res.rum;
  if (!r) return "";
  const series = r.series ?? [];
  return `
    <div class="web-block">
      <h3 class="web-subheading">web analytics (RUM)</h3>
      <div class="web-stats">
        ${statHtml("pageviews", n(r.pageviews))}
        ${statHtml("visits", n(r.visits))}
      </div>
      ${
        series.length > 0
          ? barsSvg(
              series.map((p) => p.pageviews),
              series.map((p) => `${bucketLabel(p.ts)} — ${n(p.pageviews)} pageviews, ${n(p.visits)} visits`),
              "muted",
            )
          : ""
      }
      <div class="web-cols">
        ${topListHtml("top paths", r.topPaths, "no pageviews in this window", r.pageviews)}
        ${topListHtml("top referers", r.topReferers, "nothing referred", r.pageviews)}
        ${topListHtml("top countries", r.topCountries, "—", r.pageviews)}
      </div>
    </div>`;
}

// --- search console ----------------------------------------------------------

function gscHtml(raw: GSCResult, expanded: Set<string>, hosts: string[] | null = null): string {
  // Under a mapped scope the per-URL tables narrow to the scope's hosts; the
  // summary, series and query tables have no host dimension in the API, so
  // they stay domain-wide with a note saying so rather than pretending.
  const hostSet = hosts && hosts.length > 0 ? new Set(hosts) : null;
  const inScope = (u: string): boolean => {
    if (!hostSet) return true;
    try {
      return hostSet.has(new URL(u).host);
    } catch {
      return true; // an unparsable URL is kept, never silently dropped
    }
  };
  const res: GSCResult = hostSet
    ? {
        ...raw,
        byHost: (raw.byHost ?? []).filter((r) => hostSet.has(r.name)),
        topPages: (raw.topPages ?? []).filter((p) => inScope(p.name)),
        queryPages: (raw.queryPages ?? []).filter((q) => inScope(q.page)),
      }
    : raw;
  const scopeNote = hostSet
    ? `<div class="web-scope-note">pages narrowed to ${hostSet.size === 1 ? escapeHtml(hosts![0]!) : `${hostSet.size} hosts`} — totals, series and queries stay domain-wide (GSC has no per-site slice)</div>`
    : "";
  const s = res.summary;
  const series = res.series ?? [];
  const summary = s
    ? `
    <div class="web-stats">
      ${statHtml("clicks", n(s.clicks))}
      ${statHtml("impressions", n(s.impressions))}
      ${statHtml("CTR", fmtCtr(s.ctr))}
      ${statHtml("position", fmtPos(s.position), "impression-weighted average")}
    </div>`
    : "";
  const charts =
    series.length > 0
      ? `
    <div class="web-chart-label">clicks</div>
    ${barsSvg(series.map((p) => p.clicks), series.map((p) => `${bucketLabel(p.date)} — ${n(p.clicks)} clicks, CTR ${fmtCtr(p.ctr)}`))}
    <div class="web-chart-label">impressions</div>
    ${barsSvg(series.map((p) => p.impressions), series.map((p) => `${bucketLabel(p.date)} — ${n(p.impressions)} impressions, position ${fmtPos(p.position)}`), "muted")}`
      : "";
  const window = `${res.startDate} → ${res.endDate}`;
  return `
    ${scopeNote}
    <div class="web-block">
      <div class="table-caption">${escapeHtml(res.property)} · ${escapeHtml(window)}</div>
      ${summary}
      ${charts}
    </div>
    ${gscRowTableHtml("top queries", res.topQueries)}
    ${gscPagesHtml(res, expanded)}
    <div class="web-cols">
      ${gscMiniTableHtml("devices", res.devices)}
      ${gscMiniTableHtml("countries", res.countries)}
      ${gscMiniTableHtml("by host", res.byHost)}
    </div>
    ${gscSitemapsHtml(res)}
    ${errorsFootnoteHtml(res.errors)}`;
}

function gscRowCells(r: GSCRow): string {
  return `
    <td>${n(r.clicks)}</td>
    <td class="cell-muted">${n(r.impressions)}</td>
    <td class="cell-muted">${fmtCtr(r.ctr)}</td>
    <td class="cell-muted">${fmtPos(r.position)}</td>`;
}

function gscRowTableHtml(title: string, rows: GSCRow[] | null): string {
  const list = rows ?? [];
  if (list.length === 0) return "";
  const body = list
    .map(
      (r) => `
      <tr>
        <td class="cell-title" title="${escapeHtml(r.name)}">${escapeHtml(truncate(r.name, 64))}</td>
        ${gscRowCells(r)}
      </tr>`,
    )
    .join("");
  return `
    <div class="web-block">
      <h3 class="web-subheading">${escapeHtml(title)}</h3>
      <table class="sessions-table web-table">
        <thead><tr><th>query</th><th>clicks</th><th>impressions</th><th>CTR</th><th>position</th></tr></thead>
        <tbody>${body}</tbody>
      </table>
    </div>`;
}

// Top pages, each expandable to the queries that surface it (the page×query
// drill-down) — the input for title/meta work, one click deep.
function gscPagesHtml(res: GSCResult, expanded: Set<string>): string {
  const pages = res.topPages ?? [];
  if (pages.length === 0) return "";
  const byPage = new Map<string, GSCQueryPage[]>();
  for (const qp of res.queryPages ?? []) {
    const list = byPage.get(qp.page) ?? [];
    list.push(qp);
    byPage.set(qp.page, list);
  }
  const body = pages
    .map((p) => {
      const isOpen = expanded.has(p.name);
      const queries = byPage.get(p.name) ?? [];
      const main = `
      <tr class="web-page-row${isOpen ? " web-page-row-open" : ""}" data-page="${escapeHtml(p.name)}">
        <td class="churn-chevron">${queries.length > 0 ? (isOpen ? "▾" : "▸") : ""}</td>
        <td class="cell-title" title="${escapeHtml(p.name)}">${escapeHtml(truncate(shortenURL(p.name), 56))}</td>
        ${gscRowCells(p)}
      </tr>`;
      if (!isOpen || queries.length === 0) return main;
      const qRows = queries
        .map(
          (q) => `
        <div class="web-toplist-row" title="${escapeHtml(q.query)}">
          <span class="web-toplist-name">${escapeHtml(truncate(q.query, 56))}</span>
          <span class="web-toplist-count">${n(q.clicks)} clicks · ${n(q.impressions)} impr · ${fmtCtr(q.ctr)} · pos ${fmtPos(q.position)}</span>
        </div>`,
        )
        .join("");
      return main + `<tr class="web-page-detail"><td colspan="6"><div class="web-toplist">${qRows}</div></td></tr>`;
    })
    .join("");
  return `
    <div class="web-block">
      <h3 class="web-subheading">top pages</h3>
      <table class="sessions-table web-table">
        <thead><tr><th></th><th>page</th><th>clicks</th><th>impressions</th><th>CTR</th><th>position</th></tr></thead>
        <tbody>${body}</tbody>
      </table>
      <div class="table-caption">click a page for the queries that surface it</div>
    </div>`;
}

function gscMiniTableHtml(title: string, rows: GSCRow[] | null): string {
  const list = rows ?? [];
  if (list.length === 0) return "";
  const body = list
    .map(
      (r) => `
      <div class="web-toplist-row" title="${escapeHtml(r.name)}">
        <span class="web-toplist-name">${escapeHtml(truncate(r.name, 32))}</span>
        <span class="web-toplist-count">${n(r.clicks)} clicks</span>
        <span class="web-toplist-pct">${fmtCtr(r.ctr)}</span>
      </div>`,
    )
    .join("");
  return `
    <div class="web-toplist">
      <h3 class="web-subheading">${escapeHtml(title)}</h3>
      ${body}
    </div>`;
}

function gscSitemapsHtml(res: GSCResult): string {
  const maps = res.sitemaps ?? [];
  if (maps.length === 0) return "";
  const body = maps
    .map(
      (m) => `
      <tr>
        <td class="cell-title" title="${escapeHtml(m.path)}">${escapeHtml(truncate(shortenURL(m.path), 48))}</td>
        <td class="cell-muted">${escapeHtml(m.lastSubmitted ? bucketLabel(m.lastSubmitted) : "—")}</td>
        <td class="cell-muted">${escapeHtml(m.lastDownloaded ? bucketLabel(m.lastDownloaded) : "—")}</td>
        <td>${m.isPending ? "pending" : "processed"}</td>
        <td class="${m.errors > 0 ? "web-bad" : "cell-muted"}">${n(m.errors)}</td>
        <td class="cell-muted">${n(m.warnings)}</td>
      </tr>`,
    )
    .join("");
  return `
    <div class="web-block">
      <h3 class="web-subheading">sitemaps</h3>
      <table class="sessions-table web-table">
        <thead><tr><th>sitemap</th><th>submitted</th><th>downloaded</th><th>status</th><th>errors</th><th>warnings</th></tr></thead>
        <tbody>${body}</tbody>
      </table>
    </div>`;
}

/** Strip the scheme+host for display; the full URL stays in `title`. */
function shortenURL(u: string): string {
  return u.replace(/^https?:\/\/[^/]+/, "") || u;
}
