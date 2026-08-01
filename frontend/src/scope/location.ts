// The route lives in the real path (History API), shaped family/scope/tab[/detail]:
//   /project/<scope>/git , /claude/<scope>/sessions.
// The tab sets let parseLocation tell a scope-less transient path (/project/git,
// before syncScopeToURL injects the scope) apart from a scoped one (/project/x/git):
// if the segment after the family names a known tab, there's no scope segment.
// `design` is the legacy spelling of `wireframe` (renamed when the OpenPencil
// `ui` tab split off) — still parsed so old bookmarks and drawing deep links
// resolve; the router maps it to the same views and nothing links to it.
const PROJECT_TABS = new Set([
  "board",
  "cycles",
  "wireframe",
  "design",
  "ui",
  "docs",
  "ships",
  "codegraph",
  "git",
]);
const CLAUDE_TABS = new Set(["sessions", "usage", "insights", "search", "session"]);

export interface Loc {
  family: "claude" | "project" | "";
  scope: string; // "" when it's the transient scope-less form
  tab: string;
  detail: string;
}

/** Split a pathname into family / scope / tab / detail. */
export function parseLocation(pathname: string): Loc {
  const segs = pathname.split("/").filter(Boolean).map(decodeURIComponent);
  const family = segs[0] ?? "";
  if (family === "project" || family === "claude") {
    const tabs = family === "project" ? PROJECT_TABS : CLAUDE_TABS;
    if (segs[1] && tabs.has(segs[1])) {
      return { family, scope: "", tab: segs[1], detail: segs[2] ?? "" };
    }
    return { family, scope: segs[1] ?? "", tab: segs[2] ?? "", detail: segs[3] ?? "" };
  }
  return { family: "", scope: "", tab: "", detail: "" };
}

/** Build a canonical path from its parts; the scope segment is dropped when
    there's no scope yet. */
export function buildPath(family: string, scope: string, tab: string, detail: string): string {
  const segs = [family];
  if ((family === "project" || family === "claude") && scope) segs.push(encodeURIComponent(scope));
  if (tab) segs.push(tab);
  if (detail) segs.push(encodeURIComponent(detail));
  return "/" + segs.join("/");
}
