// Global project scope: always one project name or one project-GROUP name.
// Every view but web reads it at render time (web's data is per-site, and no
// repo↔site mapping exists). There is no "all projects" entry anymore — ""
// is only the transient pre-boot value before the rail picks a default. Its
// UI is the project rail (#proj-rail): the leftmost column on board / design
// / docs, mounted once outside #view so it persists across visits (routes
// without it just hide it). Picking a project re-renders the view the same
// path as a hashchange, so the old view's cleanup runs normally — and the
// scope is app-wide: sessions, insights and the rest follow it too.
//
// The taxonomy is now the durable PROJECT REGISTRY (/api/projects/registry),
// decoupled from the raw ~/.claude scan: a project OWNS the Claude folders
// (session cwd-basenames) it stands for, so you can merge several folders under
// one name or hide the junk. Two resolutions of a scope, for two kinds of view:
//   - getScopeSet(): the user-project NAMES it covers — client-side label
//     matching (board / docs / design / ships, whose items carry a name).
//   - getScopeParam(): the Claude FOLDERS it covers — the server-side session
//     filter (sessions / insights / search), which matches s.Project.
// A group is a named set of project names, managed from "+ groups…"; it covers
// its name plus its members. Names resolve group-first on collision.

import { getClaudeScopes, getScopeIndex } from "../api";
import type { ClaudeScope, Project, ProjectGroup } from "../api";
import { escapeHtml } from "../domain/format";
import { buildPath, parseLocation } from "./location";

// Two families, two taxonomies, two scopes — they share the path GRAMMAR
// (family/scope/tab) and nothing else. /project scopes by the registry you
// curate; /claude scopes by the repos its sessions actually ran in, derived
// from the transcripts (see /api/claude/scopes). Neither is the other's source
// of truth, so each remembers its own scope: switching family used to carry a
// project name into the session views, where it meant something else entirely.
const SCOPE_KEY = "wyac-scope"; // project family
const CLAUDE_SCOPE_KEY = "wyac-scope-claude";

type Family = "claude" | "project";

/** The family a path belongs to. The root is the sessions list, so "" reads as
    claude — same rule syncScopeToURL applies when it canonicalises. */
function familyOf(pathname: string): Family {
  return parseLocation(pathname).family === "project" ? "project" : "claude";
}

function storageKey(fam: Family): string {
  return fam === "claude" ? CLAUDE_SCOPE_KEY : SCOPE_KEY;
}

function remembered(fam: Family): string {
  try {
    return localStorage.getItem(storageKey(fam)) ?? "";
  } catch {
    return "";
  }
}

/** The claude family's scope list, keyed by its URL segment. Empty until the
    boot fetch lands; "" as a scope means every folder, which is the default and
    a legitimate answer here (unlike the project rail, which always selects one). */
let claudeScopes: ClaudeScope[] = [];

export function getClaudeScopeList(): ClaudeScope[] {
  return claudeScopes;
}

export function loadClaudeScopeIndex(): Promise<void> {
  return getClaudeScopes()
    .then((list) => {
      claudeScopes = list;
    })
    .catch(() => {
      /* keep the last good list; an empty one just means "all" everywhere */
    });
}

// Module-level caches so views can resolve the scope synchronously at render
// time; mountScopeRail fills them (and re-renders once, if that changes what an
// already-rendered scope resolves to).
let knownGroups: ProjectGroup[] = [];
let knownProjects: Project[] = [];

// The rail is the only writer (via setKnownTaxonomy); this accessor lets it
// live in the UI layer without the caches moving with it.
export function getKnownProjects(): Project[] {
  return knownProjects;
}

// The rail is the only writer (via setKnownTaxonomy); this accessor lets it
// live in the UI layer without the caches moving with it.
export function getKnownGroups(): ProjectGroup[] {
  return knownGroups;
}

// The rail is the only writer; this setter lets it live in the UI layer
// without the caches themselves moving with it.
export function setKnownTaxonomy(projects: Project[], groups: ProjectGroup[]): void {
  knownProjects = projects;
  knownGroups = groups;
}

/** The active scope label. The URL path is the source of truth when it carries a
    scope segment (so a bookmarked/shared link opens that project); localStorage is
    the fallback that remembers the last scope across loads and seeds a bare path.
    "" is the transient pre-boot value — the rail picks a default at boot. */
export function getScope(): string {
  const loc = parseLocation(window.location.pathname);
  if (loc.scope) return loc.scope;
  return remembered(familyOf(window.location.pathname));
}

/** The PROJECT family's scope no matter where you are standing. The rail mounts
    on every route and has to render, default and compare against its own
    family's scope — getScope() would hand it the claude one on a session route.
    The path wins only when the path IS the project family's. */
export function getProjectScope(): string {
  const loc = parseLocation(window.location.pathname);
  if (loc.family === "project" && loc.scope) return loc.scope;
  return remembered("project");
}

/** label -> the project names that scope covers, as RESOLVED BY THE SERVER
    (/api/scopes). Empty until the boot fetch lands, exactly like knownProjects. */
let scopeIndex: Record<string, string[]> = {};

/** The set of user-project NAMES the active scope covers, or null when
    unscoped. This is the client-side label filter for the board / design /
    docs / ships / cycles.

    It is a LOOKUP, not a computation. Expanding a group into its members and
    walking the rail's parent tree used to happen here as well as in the Go
    resolver — one rule, two implementations, in two languages, agreeing only
    by inspection. They did not agree: the server's copy compared labels
    instead of expanding them, and a workflow column created under a group
    vanished when the rail narrowed to a member. One rule, one place.

    The fallback before the index loads is the label alone, which is what the
    old code also returned while knownGroups/knownProjects were still empty —
    a degenerate answer, not a second copy of the rule. The boot fetch
    re-renders the views when it lands. */
export function getScopeSet(): Set<string> | null {
  const scope = getScope();
  if (!scope) return null;
  // The claude family answers in FOLDERS — that is what a session carries
  // (s.project) and what its callers compare against. The project family
  // answers in project NAMES, which is what a card carries. One question, two
  // vocabularies, because there are now two taxonomies.
  if (familyOf(window.location.pathname) === "claude") {
    const cs = claudeScopes.find((c) => c.key === scope);
    return new Set(cs ? cs.folders : [scope]);
  }
  return new Set(scopeIndex[scope] ?? [scope]);
}

/** Reload the resolved index. Called at boot and whenever the groups or the
    project registry change, since either can change what a label covers. */
export function loadScopeIndex(): Promise<void> {
  return getScopeIndex()
    .then((idx) => {
      scopeIndex = idx ?? {};
    })
    .catch(() => {
      /* keep the last good index; the label-alone fallback covers a cold start */
    });
}

/** The active scope as an API `project` param for the SESSION-derived endpoints
    (they filter s.Project, a Claude folder). The claude scope IS a repo, so
    this expands it to the folders whose sessions ran there — no registry
    lookup, which is the whole point of the split. undefined = every folder. */
export function getScopeParam(): string | undefined {
  const scope = getScope();
  if (!scope) return undefined; // "" is every folder — the claude family's default
  const cs = claudeScopes.find((c) => c.key === scope);
  // A scope the list does not know (a stale link, or the list not loaded yet)
  // falls back to itself as a folder name: the pre-registry behaviour, and it
  // keeps a bookmarked folder reachable instead of silently widening to ALL.
  const folders = cs ? cs.folders : [scope];
  return folders.length ? folders.join(",") : undefined;
}

/** The scope control for views without the rail. Two different things, because
    the two families now have two different taxonomies:

    - claude (sessions / insights / search) gets a real SWITCHER — its scopes
      are derived, so there is no manager page to send you to, and a select is
      the whole UI. main.ts listens for its change event.
    - project (cycles / ships) keeps the read-only chip linking to the board,
      where the registry's rail is.

    Deliberately scope-LESS href, per the routing rule — syncScopeToURL splices
    the active scope in at render. */
export function scopeChipHtml(): string {
  const scope = getScope();
  if (familyOf(window.location.pathname) === "claude") {
    const opts = [`<option value=""${scope ? "" : " selected"}>all repos</option>`]
      .concat(
        claudeScopes.map(
          (c) =>
            `<option value="${escapeHtml(c.key)}"${c.key === scope ? " selected" : ""}>` +
            `${escapeHtml(c.name)} (${c.sessions})</option>`,
        ),
      )
      .join("");
    return (
      `<label class="scope-chip scope-chip--select" title="the repos your sessions ran in — derived from the transcripts, not from the project registry">` +
      `<span class="scope-chip-key">scope</span>` +
      `<select id="claude-scope-select">${opts}</select></label>`
    );
  }
  const label = scope || "all projects";
  return (
    `<a class="scope-chip" href="/project/board" ` +
    `title="the views with the project rail — board, design, docs, code graph, git — are where the scope is changed">` +
    `<span class="scope-chip-key">scope</span>${escapeHtml(label)}</a>`
  );
}

/** Known group names — extra labels the docs group input can offer. */
export function getKnownGroupNames(): string[] {
  return knownGroups.map((g) => g.name);
}

/** The display label for a Claude folder (an s.Project value): the name of the
    REPO its sessions ran in, or the folder itself when they ran in none.
    Session-derived views (grouping, per-project tables, live cards) show this,
    so two folders of one repo read as one name — and a folder nobody has
    claimed still reads as something, which is what it did before any of this. */
export function labelForFolder(folder: string): string {
  const cs = claudeScopes.find((c) => c.folders.includes(folder));
  return cs ? cs.name : folder;
}

function saveScope(p: string, fam: Family = familyOf(window.location.pathname)): void {
  try {
    if (p === "") {
      localStorage.removeItem(storageKey(fam));
    } else {
      localStorage.setItem(storageKey(fam), p);
    }
  } catch {
    /* storage unavailable — the scope just won't persist */
  }
}

/** Go to a path via the History API. A push (the default) adds a Back entry and
    dispatches a synthetic popstate — the single event both render() and the rail's
    re-highlight listen for, so programmatic and Back/Forward navigation share one
    path. replace=true is a SILENT in-place URL update (no entry, no event): its
    callers (boot default, rename) re-render through their own path, so dispatching
    here would just double-render. */
export function navigate(path: string, replace = false): void {
  if (path === window.location.pathname) {
    return;
  }
  if (replace) {
    window.history.replaceState(window.history.state, "", path);
    // A replace fires no popstate, so render() — and with it syncScopeToURL —
    // never runs. Canonicalise here instead, or a scope-less replace (the docs
    // view auto-opening its first page) leaves the URL without its scope segment
    // until some unrelated render happens to fix it. Copying the link in that
    // window would resolve against the READER's remembered scope, not yours.
    syncScopeToURL();
    return;
  }
  window.history.pushState(window.history.state, "", path);
  window.dispatchEvent(new PopStateEvent("popstate"));
}

/** Set the active scope: persist it AND reflect it in the path, keeping the
    current tab/detail. A rail pick pushes a history entry (Back returns to the
    previous scope); a boot default or rename passes replace=true to swap it in
    place. Service routes have no scope segment, so a scope change there is a
    persist-only no-op on the URL. */
export function setScope(label: string, replace = false, fam?: Family): void {
  const loc = parseLocation(window.location.pathname);
  const target = fam ?? familyOf(window.location.pathname);
  saveScope(label, target);
  let { family, tab, detail } = loc;
  // Only the family you are LOOKING at owns the URL. The project rail mounts on
  // every route and applies its default at boot, so without this it splices a
  // project name into a claude path — where it names nothing, and the session
  // list goes empty. A family-less path (the root) has nothing to rewrite
  // either; the next scoped navigation carries the persisted scope.
  if (family === "" || family !== target) {
    return;
  }
  if (family === "claude" && tab === "") tab = "sessions";
  // Picking a DIFFERENT scope drops the detail segment. A repo / page / card /
  // drawing belongs to the scope you were in; carrying it across lands on a dead
  // "not in this scope" screen (a repo path under a scope that doesn't hold it).
  // Switching project means "show me THIS project's list", so land on the tab.
  // A replace is the other case — the boot default, or a rename of the scope
  // you're already in — where it's the same scope and the detail is still valid.
  if (!replace) detail = "";
  navigate(buildPath(family, label, tab, detail), replace);
}

/** Canonicalise the path to the effective scope without adding history — render()
    calls this so a bare/scope-less path (an existing link that omits the scope, or
    the root) re-gains its scope segment, keeping every URL shareable. Also mirrors
    the effective scope into localStorage, so opening a shared link makes it the
    remembered scope. No-op once the path is already canonical, so it never loops. */
export function syncScopeToURL(): void {
  const loc = parseLocation(window.location.pathname);
  let { family, tab, detail } = loc;
  if (family === "") {
    // Root → the default landing (sessions), scoped.
    family = "claude";
    tab = "sessions";
  }
  if (family === "claude" && tab === "") tab = "sessions";
  const label = getScope();
  saveScope(label);
  const want = buildPath(family, label, tab, detail);
  if ((window.location.pathname || "/") !== want) {
    window.history.replaceState(window.history.state, "", want);
  }
}
