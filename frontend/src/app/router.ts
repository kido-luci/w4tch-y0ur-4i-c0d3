import { buildPath, parseLocation } from "../scope";

export type Route =
  | { view: "list" }
  | { view: "detail"; id: string }
  | { view: "board"; cardId?: string }
  | { view: "design" }
  | { view: "designEditor"; id: string }
  | { view: "ui" }
  | { view: "docs"; id?: string }
  | { view: "usage" }
  | { view: "insights" }
  | { view: "search" }
  | { view: "ships" }
  | { view: "codegraph" }
  | { view: "git" }
  | { view: "gitRepo"; folder: string }
  | { view: "cycles" };

// Routes are real paths, shaped family/scope/tab[/detail] (see scope/location.ts). The
// scope segment is orthogonal to the view, so it's parsed out here and the view
// is chosen from family + tab + detail. Detail segments arrive already decoded.
export function parseRoute(pathname: string): Route {
  const loc = parseLocation(pathname);
  if (loc.family === "claude") {
    if (loc.tab === "session" && loc.detail) {
      return { view: "detail", id: loc.detail };
    }
    if (loc.tab === "usage") return { view: "usage" };
    if (loc.tab === "insights") return { view: "insights" };
    if (loc.tab === "search") return { view: "search" };
    return { view: "list" }; // sessions — the default landing
  }
  if (loc.family === "project") {
    switch (loc.tab) {
      case "board":
        return loc.detail ? { view: "board", cardId: loc.detail } : { view: "board" };
      case "cycles":
        return { view: "cycles" };
      // `design` is the pre-split spelling of `wireframe`; parseLocation still
      // admits it so a pre-rename bookmark (or a drawing link in an old doc)
      // lands on the same views instead of the default.
      case "wireframe":
      case "design":
        return loc.detail ? { view: "designEditor", id: loc.detail } : { view: "design" };
      case "ui":
        return { view: "ui" };
      case "docs":
        return loc.detail ? { view: "docs", id: loc.detail } : { view: "docs" };
      case "ships":
        return { view: "ships" };
      case "codegraph":
        return { view: "codegraph" };
      case "git":
        return loc.detail ? { view: "gitRepo", folder: loc.detail } : { view: "git" };
    }
  }
  return { view: "list" };
}

// Old `#/…` hash URLs (bookmarks from before the path move, incl. `?scope=`)
// translate to the new path once at boot, so they don't dead-land on the default.
export function migrateLegacyHash(): void {
  const h = window.location.hash;
  if (!h.startsWith("#/")) return;
  const [rawPath = "", query = ""] = h.slice(1).split("?");
  const scopeM = /(?:^|&)scope=([^&]*)/.exec(query);
  const scope = scopeM?.[1] ? decodeURIComponent(scopeM[1]) : "";
  const segs = rawPath.split("/").filter(Boolean).map(decodeURIComponent);
  const tab = segs[0] ?? "";
  const detail = segs[1] ?? "";
  let path: string;
  switch (tab) {
    case "usage":
    case "insights":
    case "search":
    case "session":
      path = buildPath("claude", scope, tab, detail);
      break;
    case "design":
      // Hash-era URLs predate the design → wireframe rename; migrate straight
      // to the new spelling rather than minting a fresh legacy path.
      path = buildPath("project", scope, "wireframe", detail);
      break;
    case "board":
    case "docs":
    case "ships":
    case "codegraph":
    case "git":
      path = buildPath("project", scope, tab, detail);
      break;
    default:
      path = buildPath("claude", scope, "sessions", "");
  }
  window.history.replaceState(null, "", path);
}
