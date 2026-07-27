// View 8 — code graph (route `/project/codegraph`): the scoped repo's architecture as
// a dependency graph, read from the .codegraph/codegraph.db index the codegraph
// MCP maintains (wyac never writes it, never runs the indexer).
//
// The graph canvas is a React Flow island (see codegraphGraph.tsx): HTML card
// nodes and edges routed by ELK (orthogonal, channel-separated — so the layered
// graph reads instead of tangling). This vanilla shell owns everything else —
// the chips, the side panel, search, the data prep — and drives the island.
//
// Cards: a type icon, a quantity badge coloured by language (symbols for a file,
// files for a folder), the name, a status dot coloured by SUBSYSTEM (the folder's
// architectural layer — features / bloc / services / … — so the levels read
// apart), and a linked-edge count.
//
// Two altitudes: a big repo opens as a DIRECTORY overview (folders as cards,
// split adaptively, click to drill into files with the folders it talks to as
// ghost cards at the rim); a small repo (or inside a folder) shows files.
// Measured noise stays out: tests hide behind a chip, cross-language calls never
// leave the server, a dense flat folder hides weight-1 edges. Side panel:
// file → symbols → callers/callees; search rides the index's own FTS.

import {
  getCodegraph,
  getCodegraphFile,
  getCodegraphSymbol,
  searchCodegraphSymbols,
} from "../api";
import type { CGResponse, CGSymbol } from "../api";
import { mountCodegraphGraph } from "./codegraphGraph";
import type { GraphHandle, GVEdge, GVNode } from "./codegraphGraph";
import { escapeHtml, formatRelativeTime, formatTokens } from "../format";
import { getScope, getScopeParam } from "../scope";

// Which subsystems (components) are shown, persisted per scope. Default is
// NONE — the graph opens empty and you switch on the layers you want to see;
// your picks survive a reload.
const CG_SUBSYS_KEY = "wyac-cg-subsys";
function loadSubsysOn(scopeKey: string): Set<string> {
  try {
    const raw = localStorage.getItem(CG_SUBSYS_KEY);
    if (raw) {
      const m = JSON.parse(raw) as Record<string, string[]>;
      if (Array.isArray(m[scopeKey])) return new Set(m[scopeKey]);
    }
  } catch {
    /* corrupt/unavailable storage — start empty */
  }
  return new Set();
}
function saveSubsysOn(scopeKey: string, on: Set<string>): void {
  try {
    const raw = localStorage.getItem(CG_SUBSYS_KEY);
    const m = (raw ? JSON.parse(raw) : {}) as Record<string, string[]>;
    m[scopeKey] = [...on];
    localStorage.setItem(CG_SUBSYS_KEY, JSON.stringify(m));
  } catch {
    /* storage unavailable — selection just won't persist */
  }
}

// Subsystem hues for the folder/file status dot — mid-tones that hold on both
// themes. One colour per architectural layer, so features / bloc / services /
// models / … read apart in the overview.
const CLUSTER_COLORS = [
  "#4a8fd4",
  "#2aa17c",
  "#8b7fe0",
  "#d9a03f",
  "#cf6b96",
  "#3fa9bf",
  "#97a13f",
  "#c47a52",
  "#7f77dd",
  "#d4537e",
];

interface Chip {
  bg: string;
  fg: string;
  dot: string;
}
const LANG: Record<string, Chip> = {
  typescript: { bg: "#E6F1FB", fg: "#042C53", dot: "#378ADD" },
  javascript: { bg: "#FAEEDA", fg: "#412402", dot: "#EF9F27" },
  go: { bg: "#E1F5EE", fg: "#04342C", dot: "#1D9E75" },
  dart: { bg: "#E1F5EE", fg: "#04342C", dot: "#3fa9bf" },
  python: { bg: "#EAF3DE", fg: "#173404", dot: "#639922" },
  yaml: { bg: "#F1EFE8", fg: "#2C2C2A", dot: "#888780" },
};
const CHIP_DEFAULT: Chip = { bg: "#F1EFE8", fg: "#2C2C2A", dot: "#888780" };
const CHIP_FOLDER: Chip = { bg: "#F1EFE8", fg: "#444441", dot: "#888780" };
const CHIP_GHOST: Chip = { bg: "transparent", fg: "#8a8a8a", dot: "#8a8a8a" };

const DIR_MODE_AT = 80;
const SPLIT_AT = 40;
const MAX_GROUPS = 20;
const DENSE_AT = 150;
const MAX_NODES = 400;

type NodeClass = "file" | "dir" | "ghost";

interface VNode {
  id: string;
  label: string;
  info: string;
  kindClass: NodeClass;
  cluster: string;
  degree: number;
  lang?: string;
  isTest?: boolean;
  symbols?: number;
  files?: number;
}

interface VEdge {
  a: string;
  b: string;
  kind: string;
  weight: number;
}

const LAYOUTS: { key: string; label: string }[] = [
  { key: "layered", label: "layered" },
  { key: "tree", label: "tree" },
  { key: "force", label: "force" },
  { key: "concentric", label: "concentric" },
];

function basename(p: string): string {
  return p.split("/").pop() ?? p;
}

// Source-root segments stripped when naming a subsystem, so the layer below
// them is what groups: lib/features/x → features, internal/usecase → usecase.
const SRC_ROOTS = new Set(["lib", "src", "internal", "pkg"]);

/** The architectural subsystem a path belongs to — its first segment, but with
    a leading source root (`lib` / `src` / `internal` / `pkg`) stripped so
    lib/features/x and lib/bloc don't both collapse to "lib". This is the
    status-dot colour key, so the overview's folders group by layer. */
function subsystem(p: string): string {
  const segs = p.split("/");
  const i = segs[0] !== undefined && SRC_ROOTS.has(segs[0]) ? 1 : 0;
  const seg = segs[i];
  if (seg === undefined) return "(root)";
  // A lone filename (a root-level file — the last segment, carrying an
  // extension) is its own "component" of one, which is noise; group those
  // under (root). A lone directory (no dot) keeps its own name.
  if (i === segs.length - 1 && seg.includes(".")) return "(root)";
  return seg;
}

// Secondary (auxiliary) subsystems — infra / vendor / generated / platform /
// utility layers that are noise when you're reading the app's own structure.
// They render as a separate "aux" group in the chip row and are hidden by
// default (one click brings any back). `(root)` is deliberately NOT here: on a
// flat repo (wyac's Go backend) it IS the primary code.
const AUX_SUBSYS = new Set([
  "packages", "gen", "generated", "utils", "util", "constants", "const",
  "debug", "config", "configs", "service_locator", "di", "assets",
  "l10n", "i18n", "intl", "theme", "themes", "styles", "helpers", "helper",
  "route", "routes", "model", "models",
  "android", "ios", "web", "macos", "windows", "linux",
]);
function isAux(s: string): boolean {
  return AUX_SUBSYS.has(s.toLowerCase());
}

function dirLabel(key: string): string {
  return key.split("/").slice(-2).join("/");
}

function kindGroup(kind: string): string {
  return kind === "imports" || kind === "references" ? kind : "calls";
}

function groupDirs(paths: string[]): Map<string, string> {
  const dirAt = (p: string, depth: number): string => {
    const segs = p.split("/");
    segs.pop();
    if (!segs.length) return "(root)";
    return segs.slice(0, Math.min(depth, segs.length)).join("/");
  };
  const buckets = new Map<string, { depth: number; paths: string[] }>();
  for (const p of paths) {
    const k = dirAt(p, 1);
    const b = buckets.get(k);
    if (b) b.paths.push(p);
    else buckets.set(k, { depth: 1, paths: [p] });
  }
  const frozen = new Set<string>();
  for (let guard = 0; guard < 64 && buckets.size < MAX_GROUPS; guard++) {
    let bigKey = "";
    let big: { depth: number; paths: string[] } | null = null;
    for (const [k, b] of buckets) {
      if (!frozen.has(k) && b.paths.length > Math.max(SPLIT_AT, big?.paths.length ?? 0)) {
        bigKey = k;
        big = b;
      }
    }
    if (!big) break;
    const sub = new Map<string, string[]>();
    for (const p of big.paths) {
      const k = dirAt(p, big.depth + 1);
      const arr = sub.get(k);
      if (arr) arr.push(p);
      else sub.set(k, [p]);
    }
    if (sub.size <= 1) {
      frozen.add(bigKey);
      continue;
    }
    buckets.delete(bigKey);
    for (const [k, ps] of sub) buckets.set(k, { depth: big.depth + 1, paths: ps });
  }
  const out = new Map<string, string>();
  for (const [k, b] of buckets) for (const p of b.paths) out.set(p, k);
  return out;
}

/** Renders the code-graph view into `container`; returns a cleanup callback. */
export function renderCodegraphView(container: HTMLElement): () => void {
  const scope = getScopeParam();
  let resp: CGResponse | null = null;
  let repoOverride: string | undefined;
  let showTests = false;
  let showWeak = false;
  let focusDir: string | null = null;
  let layoutName = "layered";
  const kindOn: Record<string, boolean> = { calls: true, imports: true, references: false };
  let dead = false;
  let searchTimer: number | undefined;
  // Subsystems (code components / layers) switched ON — only these show. Loaded
  // from localStorage per scope, so a reload keeps your picks; empty by default
  // (nothing shown until you switch a layer on).
  const scopeKey = getScope();
  const subsysOn = loadSubsysOn(scopeKey);

  let graph: GraphHandle | null = null;
  let denseRelevant = false;
  let selectedId: string | null = null;
  // The last laid-out graph data, so a selection change re-pushes the same node
  // and edge arrays (stable refs) without re-running the ELK layout.
  let lastNodes: GVNode[] = [];
  let lastEdges: GVEdge[] = [];

  let panelStack: (() => void)[] = [];

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          <select class="project-select" id="cg-repo" hidden></select>
          <div class="filter-row" id="cg-kinds"></div>
          <div class="filter-row" id="cg-layouts"></div>
          <div class="filter-row cg-subsys" id="cg-subsys" hidden></div>
          <div class="cg-crumb" id="cg-crumb" hidden></div>
          <input class="search-input cg-search" id="cg-search" type="search"
            placeholder="search symbols…" autocomplete="off" disabled>
        </div>
        <div class="cg-meta" id="cg-meta"></div>
      </header>
      <section class="card cg-card">
        <div class="cg-canvas" id="cg-canvas">
          <div class="cg-cy" id="cg-cy"></div>
          <div class="empty-state cg-empty" id="cg-empty" hidden></div>
          <div class="cg-tip" id="cg-tip" hidden></div>
        </div>
        <aside class="cg-side" id="cg-side" hidden></aside>
      </section>
    </div>
  `;

  const repoEl = container.querySelector<HTMLSelectElement>("#cg-repo")!;
  const kindsEl = container.querySelector<HTMLElement>("#cg-kinds")!;
  const layoutsEl = container.querySelector<HTMLElement>("#cg-layouts")!;
  const subsysEl = container.querySelector<HTMLElement>("#cg-subsys")!;
  const crumbEl = container.querySelector<HTMLElement>("#cg-crumb")!;
  const searchEl = container.querySelector<HTMLInputElement>("#cg-search")!;
  const metaEl = container.querySelector<HTMLElement>("#cg-meta")!;
  const canvasEl = container.querySelector<HTMLElement>("#cg-canvas")!;
  const cyEl = container.querySelector<HTMLElement>("#cg-cy")!;
  const emptyEl = container.querySelector<HTMLElement>("#cg-empty")!;
  const tipEl = container.querySelector<HTMLElement>("#cg-tip")!;
  const sideEl = container.querySelector<HTMLElement>("#cg-side")!;

  graph = mountCodegraphGraph(cyEl);

  // --- callbacks the island drives -------------------------------------------

  function onDrill(id: string): void {
    focusDir = id;
    renderGraph();
  }
  function onHover(id: string | null, clientX?: number, clientY?: number, info?: string): void {
    if (id === null || clientX === undefined || clientY === undefined) {
      tipEl.hidden = true;
      return;
    }
    tipEl.innerHTML = `<span class="cg-tip-path">${escapeHtml(id)}</span><br>${escapeHtml(info ?? "")}`;
    tipEl.hidden = false;
    const rect = canvasEl.getBoundingClientRect();
    tipEl.style.left = `${Math.min(clientX - rect.left + 14, rect.width - 300)}px`;
    tipEl.style.top = `${clientY - rect.top + 14}px`;
  }

  function pushGraph(): void {
    graph?.update({
      vnodes: lastNodes,
      vedges: lastEdges,
      layout: layoutName,
      selectedId,
      onDrill,
      onSelectFile: selectFileNode,
      onHover,
    });
  }

  // --- header chrome ---------------------------------------------------------

  function renderKindChips(): void {
    const chips = ["calls", "imports", "references"]
      .map(
        (k) =>
          `<button type="button" class="filter-chip${kindOn[k] ? " filter-chip-on" : ""}" data-kind="${k}">${k}</button>`,
      )
      .join("");
    kindsEl.innerHTML =
      chips +
      `<button type="button" class="filter-chip${showTests ? " filter-chip-on" : ""}" data-kind="tests">tests</button>` +
      (denseRelevant
        ? `<button type="button" class="filter-chip${showWeak ? " filter-chip-on" : ""}" data-kind="weak">weak edges</button>`
        : "");
  }

  kindsEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-kind]");
    if (!btn) return;
    const k = btn.dataset["kind"]!;
    if (k === "tests") showTests = !showTests;
    else if (k === "weak") showWeak = !showWeak;
    else kindOn[k] = !kindOn[k];
    renderGraph();
  });

  function renderLayoutChips(): void {
    layoutsEl.innerHTML = LAYOUTS.map(
      (l) =>
        `<button type="button" class="filter-chip${l.key === layoutName ? " filter-chip-on" : ""}" data-lay="${l.key}">${l.label}</button>`,
    ).join("");
  }
  renderLayoutChips();

  layoutsEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-lay]");
    if (!btn || btn.dataset["lay"] === layoutName) return;
    layoutName = btn.dataset["lay"]!;
    renderLayoutChips();
    pushGraph(); // re-runs ELK for the new algorithm — no data recompute
  });

  // One toggle chip per subsystem (code component) present in the current view,
  // coloured to match the card dot; unchecking drops that layer's nodes/edges.
  function renderSubsysChips(
    subs: string[],
    counts: Map<string, number>,
    palette: Map<string, string>,
  ): void {
    subsysEl.hidden = subs.length < 2;
    if (subsysEl.hidden) {
      subsysEl.innerHTML = "";
      return;
    }
    const chip = (s: string): string =>
      `<button type="button" class="filter-chip cg-sub-chip${subsysOn.has(s) ? " filter-chip-on" : ""}" data-sub="${escapeHtml(s)}">` +
      `<span class="cg-sub-dot" style="background:${palette.get(s)}"></span>${escapeHtml(s)}` +
      `<span class="cg-sub-n">${counts.get(s) ?? 0}</span></button>`;
    // Primary layers first, then the auxiliary ones behind an "aux" divider.
    const primary = subs.filter((s) => !isAux(s));
    const aux = subs.filter((s) => isAux(s));
    subsysEl.innerHTML =
      primary.map(chip).join("") +
      (aux.length ? `<span class="cg-sub-sep">aux</span>` + aux.map(chip).join("") : "");
  }

  subsysEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>("[data-sub]");
    if (!btn) return;
    const s = btn.dataset["sub"]!;
    if (subsysOn.has(s)) subsysOn.delete(s);
    else subsysOn.add(s);
    saveSubsysOn(scopeKey, subsysOn);
    renderGraph();
  });

  function renderCrumb(): void {
    crumbEl.hidden = focusDir === null;
    crumbEl.innerHTML =
      focusDir === null
        ? ""
        : `<button type="button" class="filter-chip" data-up="1">◂ overview</button>` +
          `<span class="cg-crumb-path">${escapeHtml(focusDir)}</span>`;
  }

  crumbEl.addEventListener("click", (e) => {
    if (!(e.target as HTMLElement).closest("[data-up]")) return;
    focusDir = null;
    renderGraph();
  });

  function renderRepoSelect(): void {
    if (!resp) return;
    repoEl.hidden = resp.repos.length <= 1;
    repoEl.innerHTML = resp.repos
      .map(
        (r) =>
          `<option value="${escapeHtml(r.root)}"${r.root === resp!.active ? " selected" : ""}>` +
          `${escapeHtml(basename(r.root))}${r.hasIndex ? "" : " (no index)"}</option>`,
      )
      .join("");
  }

  repoEl.addEventListener("change", () => {
    repoOverride = repoEl.value;
    // Cancel any pending search and clear the box, so an in-flight/debounced
    // search (its guard checks searchEl.value) can't run the old query against
    // the newly-selected repo and reopen the panel this switch just closed.
    window.clearTimeout(searchTimer);
    searchEl.value = "";
    void load();
  });

  function renderMeta(extra = ""): void {
    const r = resp?.repos.find((x) => x.root === resp!.active);
    if (!r) {
      metaEl.innerHTML = "";
      return;
    }
    const stale =
      r.commitsSince > 0
        ? ` · <span class="cg-stale" title="the index is behind the repo — re-run the codegraph indexer">+${r.commitsSince} commits since</span>`
        : "";
    metaEl.innerHTML =
      `${r.files} files · ${formatTokens(r.nodes)} symbols · ${formatTokens(r.edges)} edges` +
      ` · indexed ${escapeHtml(formatRelativeTime(r.indexedAt))}${stale}${extra}`;
  }

  // --- graph build ------------------------------------------------------------

  function showEmpty(html: string | null): void {
    if (html) {
      emptyEl.innerHTML = html;
      emptyEl.hidden = false;
      cyEl.style.visibility = "hidden";
    } else {
      emptyEl.hidden = true;
      cyEl.style.visibility = "visible";
    }
  }

  /** Card display fields for one node, from its data + the subsystem palette. */
  function card(n: VNode, palette: Map<string, string>): GVNode {
    let chip: Chip;
    let badge: string;
    let sub: string;
    let count: string;
    let icon: string;
    if (n.kindClass === "ghost") {
      chip = CHIP_GHOST;
      badge = "ext";
      sub = "ghost — click to jump";
      count = "↗";
      icon = "ghost";
    } else if (n.kindClass === "dir") {
      chip = { ...CHIP_FOLDER, dot: palette.get(n.cluster) ?? CHIP_FOLDER.dot };
      badge = `${n.files ?? 0} files`;
      sub = subsystem(n.id);
      count = String(n.degree);
      icon = "folder";
    } else {
      const lc = LANG[n.lang ?? ""] ?? CHIP_DEFAULT;
      chip = { bg: lc.bg, fg: lc.fg, dot: palette.get(n.cluster) ?? lc.dot };
      badge = `${n.symbols ?? 0} sym`;
      sub = `${n.lang ?? "?"}${n.isTest ? " · test" : ""}`;
      count = String(n.degree);
      icon = n.isTest ? "test" : "file";
    }
    return {
      id: n.id,
      label: n.label,
      info: n.info,
      cls: n.kindClass,
      badge,
      sub,
      count,
      icon,
      bg: chip.bg,
      fg: chip.fg,
      dot: chip.dot,
      sel: false,
      dim: false,
    };
  }

  function renderGraph(): void {
    tipEl.hidden = true;
    selectedId = null;

    if (!resp || !resp.graph) {
      const msg = !resp
        ? "could not load"
        : resp.repos.length === 0
          ? "no repo resolved for this scope yet — open a Claude session in it once, then come back."
          : `no .codegraph index in ${escapeHtml(
              resp.repos.map((r) => basename(r.root)).join(", "),
            )}.<br>run the codegraph indexer in the repo once, then reload.`;
      renderCrumb();
      lastNodes = [];
      lastEdges = [];
      pushGraph();
      showEmpty(msg);
      return;
    }

    const files = new Map(
      resp.graph.files.filter((f) => showTests || !f.isTest).map((f) => [f.path, f]),
    );

    const raw: { from: string; to: string; kind: string; weight: number }[] = [];
    const degree = new Map<string, number>();
    for (const e of resp.graph.edges) {
      if (!files.has(e.from) || !files.has(e.to)) continue;
      const kg = kindGroup(e.kind);
      if (!kindOn[kg]) continue;
      raw.push({ from: e.from, to: e.to, kind: kg, weight: e.weight });
      degree.set(e.from, (degree.get(e.from) ?? 0) + e.weight);
      degree.set(e.to, (degree.get(e.to) ?? 0) + e.weight);
    }
    const linked = [...files.values()].filter((f) => (degree.get(f.path) ?? 0) > 0);
    const groupOf = groupDirs(linked.map((f) => f.path));

    const aggregate = (
      edges: typeof raw,
      idOf: (p: string) => string | null,
    ): VEdge[] => {
      const m = new Map<string, VEdge>();
      for (const e of edges) {
        const a = idOf(e.from);
        const b = idOf(e.to);
        if (a === null || b === null || a === b) continue;
        const key = `${a}\n${b}\n${e.kind}`; // \n can't occur in a path/kind; a space can, and would collide
        const cur = m.get(key);
        if (cur) cur.weight += e.weight;
        else m.set(key, { a, b, kind: e.kind, weight: e.weight });
      }
      return [...m.values()];
    };

    const fileNode = (
      f: { path: string; symbols: number; isTest: boolean; language: string },
      cluster: string,
    ): VNode => ({
      id: f.path,
      label: basename(f.path),
      info: `${f.symbols} symbols · ${degree.get(f.path) ?? 0} linked symbol edges${f.isTest ? " · test" : ""}`,
      kindClass: "file",
      cluster,
      degree: degree.get(f.path) ?? 0,
      lang: f.language,
      isTest: f.isTest,
      symbols: f.symbols,
    });

    let vnodes: VNode[] = [];
    let vedges: VEdge[] = [];
    let mode: "files" | "dirs" | "focus" = "files";
    let note = "";

    if (focusDir !== null && ![...groupOf.values()].includes(focusDir)) {
      focusDir = null;
    }

    if (focusDir !== null) {
      mode = "focus";
      const innerSet = new Set(
        linked.filter((f) => groupOf.get(f.path) === focusDir).map((f) => f.path),
      );
      for (const f of linked) {
        if (innerSet.has(f.path)) vnodes.push(fileNode(f, subsystem(f.path)));
      }
      const ghostWeight = new Map<string, number>();
      for (const e of raw) {
        const inA = innerSet.has(e.from);
        const inB = innerSet.has(e.to);
        if (inA === inB) continue;
        const ext = groupOf.get(inA ? e.to : e.from)!;
        ghostWeight.set(ext, (ghostWeight.get(ext) ?? 0) + e.weight);
      }
      for (const [key, w] of ghostWeight) {
        vnodes.push({
          id: key,
          label: dirLabel(key),
          info: `outside this folder · ${w} symbol edges here · click to jump`,
          kindClass: "ghost",
          cluster: "ext",
          degree: w,
        });
      }
      vedges = aggregate(
        raw.filter((e) => innerSet.has(e.from) || innerSet.has(e.to)),
        (p) => (innerSet.has(p) ? p : (groupOf.get(p) ?? null)),
      );
      note = ` · ${innerSet.size} files in ${escapeHtml(focusDir)}`;
    } else if (linked.length > DIR_MODE_AT) {
      mode = "dirs";
      const perDir = new Map<string, number>();
      for (const f of linked) perDir.set(groupOf.get(f.path)!, (perDir.get(groupOf.get(f.path)!) ?? 0) + 1);
      vedges = aggregate(raw, (p) => groupOf.get(p) ?? null);
      const dirDegree = new Map<string, number>();
      for (const e of vedges) {
        dirDegree.set(e.a, (dirDegree.get(e.a) ?? 0) + e.weight);
        dirDegree.set(e.b, (dirDegree.get(e.b) ?? 0) + e.weight);
      }
      for (const [key, n] of perDir) {
        vnodes.push({
          id: key,
          label: dirLabel(key),
          info: `${n} files · ${dirDegree.get(key) ?? 0} cross-folder symbol edges · click to open`,
          kindClass: "dir",
          cluster: subsystem(key),
          degree: dirDegree.get(key) ?? 0,
          files: n,
        });
      }
      note = ` · ${vnodes.length} folders over ${formatTokens(linked.length)} files — click a folder to open it`;
    } else {
      for (const f of linked) vnodes.push(fileNode(f, subsystem(f.path)));
      vedges = aggregate(raw, (p) => p);
    }

    // Component (subsystem) filter. In the overview / file view only the
    // switched-on layers show (default none — you pick what to look at, and it
    // persists). The focus view (inside a folder) ignores it — you drilled in
    // to see the files — and drops the chip row. The palette is stable so a
    // subsystem keeps its colour; ghosts (cluster "ext") carry no chip.
    const subCount = new Map<string, number>();
    for (const n of vnodes) if (n.cluster !== "ext") subCount.set(n.cluster, (subCount.get(n.cluster) ?? 0) + 1);
    const presentSubsys = [...subCount.keys()].sort();
    const palette = new Map<string, string>();
    presentSubsys.forEach((c, i) => palette.set(c, CLUSTER_COLORS[i % CLUSTER_COLORS.length]!));
    let noneSelected = false;
    if (mode === "focus" || presentSubsys.length < 2) {
      // Focus view (you drilled in for the files), or a single-component repo —
      // nothing to choose between, so show everything and drop the chip row.
      subsysEl.hidden = true;
      subsysEl.innerHTML = "";
    } else {
      renderSubsysChips(presentSubsys, subCount, palette);
      vnodes = vnodes.filter((n) => n.cluster === "ext" || subsysOn.has(n.cluster));
      const kept = new Set(vnodes.map((n) => n.id));
      vedges = vedges.filter((e) => kept.has(e.a) && kept.has(e.b));
      noneSelected = vnodes.length === 0;
    }

    denseRelevant = mode !== "dirs" && vnodes.length > DENSE_AT;
    if (denseRelevant && !showWeak) {
      vedges = vedges.filter((e) => e.weight > 1);
      const still = new Set<string>();
      for (const e of vedges) {
        still.add(e.a);
        still.add(e.b);
      }
      const before = vnodes.length;
      vnodes = vnodes.filter((n) => still.has(n.id));
      note += ` · weight-1 edges hidden (${before - vnodes.length} files with them)`;
    }
    if (vnodes.length > MAX_NODES) {
      vnodes = vnodes.sort((a, b) => b.degree - a.degree).slice(0, MAX_NODES);
      note += ` · showing top ${MAX_NODES}`;
    }

    const ids = new Set(vnodes.map((n) => n.id));
    vedges = vedges.filter((e) => ids.has(e.a) && ids.has(e.b));

    renderKindChips();
    renderCrumb();
    renderMeta(note);

    if (!vnodes.length) {
      lastNodes = [];
      lastEdges = [];
      pushGraph();
      showEmpty(
        noneSelected
          ? "no component selected — switch a layer on above to show it."
          : "nothing to draw with the current filters.",
      );
      return;
    }

    lastNodes = vnodes.map((n) => card(n, palette));
    lastEdges = vedges.map((e) => ({ a: e.a, b: e.b, kind: e.kind, weight: e.weight }));
    showEmpty(null);
    pushGraph();
  }

  // --- selection + side panel ------------------------------------------------

  function selectFileNode(id: string): void {
    selectedId = id;
    pushGraph();
    panelStack = [];
    pushPanel(() => void openFilePanel(id));
  }

  function clearSelection(): void {
    selectedId = null;
    pushGraph();
  }

  function closeSide(): void {
    sideEl.hidden = true;
    sideEl.innerHTML = "";
    panelStack = [];
  }

  function pushPanel(render: () => void): void {
    panelStack.push(render);
    render();
  }

  function panelChrome(title: string, body: string): void {
    sideEl.hidden = false;
    sideEl.innerHTML =
      `<div class="cg-side-head">` +
      (panelStack.length > 1
        ? `<button type="button" class="nav-link" id="cg-back">← back</button>`
        : "") +
      `<button type="button" class="nav-link cg-side-close" id="cg-close">✕</button></div>` +
      `<div class="cg-side-title">${title}</div>` +
      body;
    sideEl.querySelector("#cg-close")?.addEventListener("click", () => {
      clearSelection();
      closeSide();
    });
    sideEl.querySelector("#cg-back")?.addEventListener("click", () => {
      panelStack.pop();
      panelStack[panelStack.length - 1]?.();
    });
  }

  function symbolRows(list: CGSymbol[], withFile: boolean): string {
    if (!list.length) return `<div class="cg-side-empty">none</div>`;
    return list
      .map(
        (s) =>
          `<button type="button" class="cg-sym" data-sym="${escapeHtml(s.id)}">` +
          `<span class="cg-kind">${escapeHtml(s.via ?? s.kind)}</span>` +
          `<span class="cg-sym-name">${escapeHtml(s.name)}${(s.count ?? 0) > 1 ? ` ×${s.count}` : ""}</span>` +
          `<span class="cg-sym-loc">${withFile && s.file ? `${escapeHtml(basename(s.file))}:` : ""}${s.line}</span>` +
          `</button>`,
      )
      .join("");
  }

  function wireSymbolRows(): void {
    for (const btn of sideEl.querySelectorAll<HTMLButtonElement>("[data-sym]")) {
      btn.addEventListener("click", () => void openSymbol(btn.dataset["sym"]!));
    }
  }

  async function openFilePanel(path: string): Promise<void> {
    if (!resp?.active) return;
    panelChrome(`<span class="cg-tip-path">${escapeHtml(path)}</span>`, `<div class="cg-side-empty">loading…</div>`);
    try {
      const syms = await getCodegraphFile(resp.active, path, scope);
      if (dead) return;
      panelChrome(`<span class="cg-tip-path">${escapeHtml(path)}</span>`, symbolRows(syms, false));
      wireSymbolRows();
    } catch {
      if (!dead) panelChrome(escapeHtml(basename(path)), `<div class="cg-side-empty">could not load symbols</div>`);
    }
  }

  async function openSymbol(id: string): Promise<void> {
    if (!resp?.active) return;
    const render = async (): Promise<void> => {
      try {
        const d = await getCodegraphSymbol(resp!.active, id, scope);
        if (dead) return;
        const file = d.node.file ?? "";
        // Land the graph selection on the symbol's file so panel and graph agree.
        if (file && lastNodes.some((n) => n.id === file && n.cls === "file") && selectedId !== file) {
          selectedId = file;
          pushGraph();
        }
        panelChrome(
          `<span class="cg-kind">${escapeHtml(d.node.kind)}</span> ${escapeHtml(d.node.name)}`,
          (d.node.signature ? `<pre class="cg-sig">${escapeHtml(d.node.signature)}</pre>` : "") +
            `<div class="cg-side-sub">${escapeHtml(file)}:${d.node.line}</div>` +
            `<div class="cg-side-sub cg-side-heading">callers (${d.callers.length})</div>` +
            symbolRows(d.callers, true) +
            `<div class="cg-side-sub cg-side-heading">callees (${d.callees.length})</div>` +
            symbolRows(d.callees, true),
        );
        wireSymbolRows();
      } catch {
        if (!dead) panelChrome("symbol", `<div class="cg-side-empty">could not load</div>`);
      }
    };
    pushPanel(() => void render());
  }

  searchEl.addEventListener("input", () => {
    window.clearTimeout(searchTimer);
    const q = searchEl.value.trim();
    if (q.length < 2 || !resp?.active) return;
    searchTimer = window.setTimeout(() => {
      void (async () => {
        try {
          const hits = await searchCodegraphSymbols(resp!.active, q, scope);
          if (dead || searchEl.value.trim() !== q) return;
          panelStack = [];
          pushPanel(() => {
            panelChrome(`search "${escapeHtml(q)}"`, symbolRows(hits, true));
            wireSymbolRows();
          });
        } catch {
          /* stale repo / typing race — the next keystroke retries */
        }
      })();
    }, 250);
  });

  // --- boot ------------------------------------------------------------------

  async function load(): Promise<void> {
    focusDir = null;
    showWeak = false;
    try {
      const r = await getCodegraph(scope, repoOverride);
      if (dead) return;
      resp = r;
    } catch {
      if (dead) return;
      resp = null;
    }
    searchEl.disabled = !resp?.active;
    renderRepoSelect();
    renderMeta();
    closeSide();
    renderGraph();
  }
  void load();

  return () => {
    dead = true;
    window.clearTimeout(searchTimer);
    graph?.destroy();
    graph = null;
  };
}
