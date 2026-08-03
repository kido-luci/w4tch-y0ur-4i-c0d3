import { getJSON, sendJSON } from "./client";

// The project registry — user-owned projects, decoupled from the raw ~/.claude
// scan. Each owns the Claude folders (session cwd-basenames) it stands for.
export interface Project {
  name: string;
  folders: string[];
  hidden: boolean;
  // Mirrors the GitHub repo's visibility (derived server-side; private repo,
  // no GitHub remote, or no repo → true). Presentation mode hides these.
  private: boolean;
  // repoRoot is the repo this project IS — YOUR binding, set in the manager,
  // never inferred from the Claude folders. linkKind is what the server could
  // confirm about it: "" = never resolved, "none" = deliberately unbound,
  // "missing" = bound to a path that is gone, "local" = a repo with no GitHub
  // remote, "linked" = repoSlug (owner/name) is filled and visibility known.
  repoRoot?: string;
  // repoRoots is every local checkout of that one repo. repoRoot above is the
  // first of them; the server keeps the two in step, so a caller that only needs
  // "a" root can keep reading repoRoot.
  repoRoots?: string[];
  repoSlug?: string;
  linkKind: "" | "none" | "missing" | "local" | "linked";
  ord: number;
  parent: string; // name of the project this nests under in the rail tree, "" = top-level
  logoVersion: number; // ms of the last logo write, 0 = no logo (also the cache-buster)
}

/** Visible project names (registry order, hidden excluded) — the label
    datalists' suggestions and the sessions filter's options. Sourced from the
    registry now, not the raw index scan. */
export async function getProjects(): Promise<string[]> {
  const reg = await getJSON<Project[]>("/api/projects/registry");
  return reg.filter((p) => !p.hidden).map((p) => p.name);
}

/** The full registry (including hidden) — the rail and its manager. */
export function getProjectRegistry(): Promise<Project[]> {
  return getJSON<Project[]>("/api/projects/registry");
}

/** Claude folders the index reports that no registry project owns yet. */
export function getUnmappedFolders(): Promise<string[]> {
  return getJSON<string[]>("/api/projects/unmapped");
}

/** Create or replace one project (PUT upsert): its owned folders, hidden flag
    and rail order. Claimed folders are stripped from other projects server-side. */
export function putProject(
  name: string,
  body: { folders: string[]; hidden: boolean; ord: number; parent: string; repoRoots?: string[] },
): Promise<Project> {
  return sendJSON<Project>(`/api/projects/${encodeURIComponent(name)}`, "PUT", body);
}

/** One repo this machine knows about — the manager's binding suggestions.
    Purely a convenience list: the binding you pick is stored on the project,
    and a repo with no sessions can still be bound by typing its path. */
export interface KnownRepo {
  root: string;
  name: string;
  slug?: string;
  boundBy?: string; // the project already bound to it, if any
  byName?: boolean; // only found by matching a folder's name — worth confirming
}

export function getKnownRepos(): Promise<KnownRepo[]> {
  return getJSON<KnownRepo[]>("/api/repos");
}

/** Presentation mode — the server-side switch that hides private projects
    app-wide (rail, every endpoint family, MCP) while demoing or taking
    screenshots. The PUT's SSE echo (`presentation-updated`) is what makes
    every open tab follow. */
export function getPresentation(): Promise<{ hidden: boolean }> {
  return getJSON<{ hidden: boolean }>("/api/presentation");
}

export function putPresentation(hidden: boolean): Promise<{ hidden: boolean }> {
  return sendJSON<{ hidden: boolean }>("/api/presentation", "PUT", { hidden });
}

export function deleteProject(name: string): Promise<void> {
  return sendJSON<void>(`/api/projects/${encodeURIComponent(name)}`, "DELETE");
}

/** Rename a project and cascade the new name across every label that carried
    the old one (cards, pages, drawings, group members). A user-data change. */
export function renameProject(from: string, to: string): Promise<{ name: string }> {
  return sendJSON<{ name: string }>(`/api/projects/${encodeURIComponent(from)}/rename`, "POST", { to });
}

/** A project's logo URL, cache-busted by its version (0 → no logo). */
export function projectLogoURL(name: string, version: number): string {
  return `/api/projects/${encodeURIComponent(name)}/logo?v=${version}`;
}

/** Upload a project's logo (raw image bytes; the Content-Type rides the blob). */
export async function putProjectLogo(name: string, blob: Blob): Promise<void> {
  const res = await fetch(`/api/projects/${encodeURIComponent(name)}/logo`, {
    method: "PUT",
    headers: { "Content-Type": blob.type || "image/png" },
    body: blob,
  });
  if (!res.ok) throw new Error(`logo upload -> ${res.status} ${res.statusText}`);
}

export function deleteProjectLogo(name: string): Promise<void> {
  return sendJSON<void>(`/api/projects/${encodeURIComponent(name)}/logo`, "DELETE");
}

/** label -> the project names whose cards that scope covers, resolved by the
 *  server. The client used to compute this itself; see /api/scopes. */
export function getScopeIndex(): Promise<Record<string, string[]>> {
  return getJSON<Record<string, string[]>>("/api/scopes");
}

// One named set of project names — the nav's global scope can cover several
// repos at once through one of these.
export interface ProjectGroup {
  name: string;
  projects: string[];
}

export function getGroups(): Promise<ProjectGroup[]> {
  return getJSON<ProjectGroup[]>("/api/groups");
}

/** Create or replace one group's member set (PUT upsert). */
export function putGroup(name: string, projects: string[]): Promise<ProjectGroup> {
  return sendJSON<ProjectGroup>(`/api/groups/${encodeURIComponent(name)}`, "PUT", { projects });
}

export function deleteGroup(name: string): Promise<void> {
  return sendJSON<void>(`/api/groups/${encodeURIComponent(name)}`, "DELETE");
}
