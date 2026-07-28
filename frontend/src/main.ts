import "./style.css";
import "./pixel.css";
import { initNotifications } from "./notify";
import { buildPath, mountScopeRail, navigate, parseLocation, syncScopeToURL } from "./scope";
import { initTheme, mountThemeToggle } from "./theme";
import { renderBoardView } from "./views/board";
import { renderCodegraphView } from "./views/codegraph";
import { renderDesignEditorView, renderDesignView } from "./views/design";
import { renderSessionDetailView } from "./views/detail";
import { renderDocsView } from "./views/docs";
import { renderGitView } from "./views/git";
import { renderGitRepoView } from "./views/gitRepo";
import { renderInsightsView } from "./views/insights";
import { renderSearchView } from "./views/search";
import { renderSessionsView } from "./views/sessions";
import { renderShipsView } from "./views/ships";
import { renderWebView } from "./views/web";

initTheme();

// Excalidraw (lazy-loaded on /project/<scope>/design/<id>) resolves its canvas fonts from
// this base at runtime; the vite plugin ships them inside the bundle so the
// editor never reaches for a CDN (the app's no-egress rule).
(window as { EXCALIDRAW_ASSET_PATH?: string }).EXCALIDRAW_ASSET_PATH = "/excalidraw-assets/";

type Route =
  | { view: "list" }
  | { view: "detail"; id: string }
  | { view: "board"; cardId?: string }
  | { view: "design" }
  | { view: "designEditor"; id: string }
  | { view: "docs"; id?: string }
  | { view: "insights" }
  | { view: "search" }
  | { view: "ships" }
  | { view: "codegraph" }
  | { view: "git" }
  | { view: "gitRepo"; folder: string }
  | { view: "web"; section: "cloudflare" | "gsc" };

// Routes are real paths, shaped family/scope/tab[/detail] (see scope.ts). The
// scope segment is orthogonal to the view, so it's parsed out here and the view
// is chosen from family + tab + detail. Detail segments arrive already decoded.
function parseRoute(pathname: string): Route {
  const loc = parseLocation(pathname);
  if (loc.family === "service") {
    return { view: "web", section: loc.tab === "search-console" ? "gsc" : "cloudflare" };
  }
  if (loc.family === "claude") {
    if (loc.tab === "session" && loc.detail) {
      return { view: "detail", id: loc.detail };
    }
    if (loc.tab === "insights") return { view: "insights" };
    if (loc.tab === "search") return { view: "search" };
    return { view: "list" }; // sessions — the default landing
  }
  if (loc.family === "project") {
    switch (loc.tab) {
      case "board":
        return loc.detail ? { view: "board", cardId: loc.detail } : { view: "board" };
      case "design":
        return loc.detail ? { view: "designEditor", id: loc.detail } : { view: "design" };
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
function migrateLegacyHash(): void {
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
    case "insights":
    case "search":
    case "session":
      path = buildPath("claude", scope, tab, detail);
      break;
    case "board":
    case "design":
    case "docs":
    case "ships":
    case "codegraph":
    case "git":
      path = buildPath("project", scope, tab, detail);
      break;
    case "service":
      path = "/service/" + (detail || "cloudflare");
      break;
    case "web":
      path = "/service/cloudflare";
      break;
    default:
      path = buildPath("claude", scope, "sessions", "");
  }
  window.history.replaceState(null, "", path);
}

const appRoot = document.querySelector<HTMLDivElement>("#app");
if (!appRoot) {
  throw new Error("#app root element not found");
}
const app: HTMLDivElement = appRoot;

/* The nav is the one piece of chrome outside the views: it renders once and
   survives route changes, so only #view is torn down on navigation. Two-level
   IA: the top bar carries the three families — claude (views over the Claude
   transcripts: sessions / insights / search), project (views over YOUR
   content: board / design / docs / ships) and service (one tab per external
   service: cloudflare / search console) — and the sub-row lists the active
   family's views. The theme toggle is the one other app-wide control.

   Both rows sit in one .px-chrome wrapper so a single `position: sticky`
   pins them together — as siblings each would need its own top offset, and
   the second one's offset is the first one's height, which is not a
   constant (the sub-row wraps to two lines on a phone). */
app.innerHTML = `
  <div class="px-chrome">
  <nav class="px-nav">
    <div class="px-nav__mark" aria-hidden="true"><i></i><i></i><i></i><i></i></div>
    <span class="px-nav__word">W4tch y0ur 4I c0d3</span>
    <div class="px-nav__links">
      <a class="px-nav-link" href="/" data-fam="claude">claude</a>
      <a class="px-nav-link" href="/project/board" data-fam="project">project</a>
      <a class="px-nav-link" href="/service/cloudflare" data-fam="service">service</a>
    </div>
    <div class="px-nav__end">
      <button type="button" class="theme-btn" id="theme-btn" aria-label="toggle light/dark theme"></button>
    </div>
  </nav>
  <nav class="px-subnav">
    <a class="px-subnav-link" href="/" data-nav="list" data-fam="claude">sessions</a>
    <a class="px-subnav-link" href="/claude/insights" data-nav="insights" data-fam="claude">insights</a>
    <a class="px-subnav-link" href="/claude/search" data-nav="search" data-fam="claude">search</a>
    <a class="px-subnav-link" href="/project/board" data-nav="board" data-fam="project">board</a>
    <a class="px-subnav-link" href="/project/design" data-nav="design" data-fam="project">design</a>
    <a class="px-subnav-link" href="/project/docs" data-nav="docs" data-fam="project">docs</a>
    <a class="px-subnav-link" href="/project/ships" data-nav="ships" data-fam="project">ships</a>
    <a class="px-subnav-link" href="/project/codegraph" data-nav="codegraph" data-fam="project">code graph</a>
    <a class="px-subnav-link" href="/project/git" data-nav="git" data-fam="project">git</a>
    <a class="px-subnav-link" href="/service/cloudflare" data-nav="cloudflare" data-fam="service">cloudflare</a>
    <a class="px-subnav-link" href="/service/search-console" data-nav="gsc" data-fam="service">search console</a>
  </nav>
  </div>
  <div class="app-shell">
    <nav class="proj-rail" id="proj-rail" hidden></nav>
    <div id="view"></div>
  </div>
`;

const viewRoot = app.querySelector<HTMLDivElement>("#view");
const railRoot = app.querySelector<HTMLElement>("#proj-rail");
if (!viewRoot || !railRoot) {
  throw new Error("#view / #proj-rail root elements not found");
}
const view: HTMLDivElement = viewRoot;
const railEl: HTMLElement = railRoot;

// Publish the sticky chrome's real height as --chrome-h. Everything else that
// pins to the top reads it: the project rail, and the five in-view sticky
// panels (session rail, inspector, board panel, doc index, doc TOC), which
// would otherwise pin at 16px — underneath the chrome. It is measured rather
// than written down because it is not one number: 86px on a desktop, 100px at
// 375px where the sub-row wraps.
const chromeEl = app.querySelector<HTMLElement>(".px-chrome")!;
new ResizeObserver(() => {
  document.documentElement.style.setProperty(
    "--chrome-h",
    `${Math.round(chromeEl.getBoundingClientRect().height)}px`,
  );
}).observe(chromeEl);

mountThemeToggle(app.querySelector<HTMLButtonElement>("#theme-btn")!);
const famLinks = [...app.querySelectorAll<HTMLAnchorElement>("[data-fam]:not([data-nav])")];
const navLinks = [...app.querySelectorAll<HTMLAnchorElement>("[data-nav]")];

let cleanup: (() => void) | null = null;

function render(): void {
  cleanup?.();
  cleanup = null;
  view.innerHTML = "";

  // Keep the path canonical: a bare/scope-less path (an existing link that omits
  // the scope, or the root) re-gains its scope segment here. replaceState only, so
  // it never re-enters render().
  syncScopeToURL();

  const route = parseRoute(window.location.pathname);
  // A session detail is reached from the list, so it keeps `sessions` lit;
  // a drawing's editor keeps `design` lit, a page detail `docs`.
  const active =
    route.view === "board"
      ? "board"
      : route.view === "design" || route.view === "designEditor"
        ? "design"
        : route.view === "docs"
          ? "docs"
          : route.view === "insights"
            ? "insights"
            : route.view === "search"
              ? "search"
              : route.view === "ships"
                ? "ships"
                : route.view === "codegraph"
                  ? "codegraph"
                  : route.view === "git" || route.view === "gitRepo"
                  ? "git"
                  : route.view === "web"
                  ? route.section === "gsc"
                    ? "gsc"
                    : "cloudflare"
                  : "list";
  // The family follows the active view; the sub-row shows only that family's
  // tabs and lights the active one.
  const family =
    active === "list" || active === "insights" || active === "search"
      ? "claude"
      : active === "cloudflare" || active === "gsc"
        ? "service"
        : "project";
  for (const link of famLinks) {
    link.classList.toggle("px-nav-link--active", link.dataset.fam === family);
  }
  for (const link of navLinks) {
    link.hidden = link.dataset.fam !== family;
    link.classList.toggle("px-subnav-link--active", link.dataset.nav === active);
  }

  // The project rail (scope switcher + wiki index, one constant block —
  // independent of which view is open) flanks board / design / docs; the
  // drawing editor stays immersive, and every other view fills the shell.
  railEl.hidden = !(
    route.view === "board" ||
    route.view === "design" ||
    route.view === "docs" ||
    route.view === "codegraph" ||
    route.view === "git" ||
    route.view === "gitRepo"
  );

  cleanup =
    route.view === "detail"
      ? renderSessionDetailView(view, route.id)
      : route.view === "board"
        ? renderBoardView(view, route.cardId)
        : route.view === "design"
          ? renderDesignView(view)
          : route.view === "designEditor"
            ? renderDesignEditorView(view, route.id)
            : route.view === "docs"
              ? renderDocsView(view, route.id)
              : route.view === "insights"
                ? renderInsightsView(view)
                : route.view === "search"
                  ? renderSearchView(view)
                  : route.view === "ships"
                    ? renderShipsView(view)
                    : route.view === "codegraph"
                      ? renderCodegraphView(view)
                      : route.view === "git"
                        ? renderGitView(view)
                        : route.view === "gitRepo"
                        ? renderGitRepoView(view, route.folder)
                        : route.view === "web"
                          ? renderWebView(view, route.section)
                          : renderSessionsView(view);
}

// Back/Forward, and every programmatic navigate()/setScope() (which dispatch a
// popstate), re-render through here.
window.addEventListener("popstate", render);

// Intercept clicks on internal path links so they route in-app (pushState) instead
// of triggering a full page load. External links (github PRs/issues, target=_blank),
// downloads, and modified clicks (new tab, etc.) are left to the browser.
document.addEventListener("click", (e) => {
  if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) {
    return;
  }
  const a = (e.target as HTMLElement).closest("a");
  const href = a?.getAttribute("href");
  if (!a || href === null || href === undefined || a.target === "_blank" || a.hasAttribute("download")) {
    return;
  }
  // Same-origin path links only ("/…", not "//host" and not "http(s)://…").
  if (!href.startsWith("/") || href.startsWith("//")) {
    return;
  }
  e.preventDefault();
  navigate(href);
});

// Old #/… bookmarks → the new path, once, before anything reads the location.
migrateLegacyHash();

// A scope change re-renders through popstate, so the outgoing view's cleanup runs
// normally. The rail mounts once (outside #view) and just hides on routes without it.
mountScopeRail(railEl, render);
initNotifications();
render();
