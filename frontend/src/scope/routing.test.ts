import { beforeEach, describe, expect, it } from "vitest";
import { buildPath, parseLocation } from "./location";
import { navigate, setScope, syncScopeToURL } from "./scope";

// Characterization tests for the routing half of the scope module, written
// before it was split apart — they are what made that split safe to do.
// Routing had no coverage at all, and CLAUDE.md records two rules here that
// each shipped as a bug once:
//
//   1. A scope change must DROP the detail segment. Carrying it over lands on a
//      detail belonging to the scope you just left — a dead "not in this scope"
//      screen reachable only by switching project while viewing one.
//   2. Every navigate path must canonicalise, not only the ones that re-render.
//      A replace fires no popstate on purpose, so render() — and with it
//      syncScopeToURL — never runs; the replace branch has to canonicalise
//      itself or the URL sits without its scope segment until some unrelated
//      render fixes it. A link copied in that window resolves against the
//      READER's remembered scope, not yours.
//
// These run without jsdom: the routing functions touch a small, known slice of
// the browser (location.pathname, history.push/replaceState, localStorage, one
// dispatched event), so a stub keeps `make check` free of a DOM dependency. The
// stub mirrors the one browser behaviour the invariants depend on — push and
// replace update location.pathname synchronously.

const SCOPE_KEY = "wyac-scope"; // the project family's
const CLAUDE_SCOPE_KEY = "wyac-scope-claude";

interface HistoryCall {
  kind: "push" | "replace";
  path: string;
}

let calls: HistoryCall[] = [];
let events: string[] = [];
let store: Record<string, string> = {};

const fakeWindow = {
  location: { pathname: "/" },
  history: {
    state: null as unknown,
    pushState(_state: unknown, _title: string, url: string): void {
      calls.push({ kind: "push", path: url });
      fakeWindow.location.pathname = url;
    },
    replaceState(_state: unknown, _title: string, url: string): void {
      calls.push({ kind: "replace", path: url });
      fakeWindow.location.pathname = url;
    },
  },
  dispatchEvent(e: { type: string }): boolean {
    events.push(e.type);
    return true;
  },
};

const fakeStorage = {
  getItem: (k: string): string | null => (k in store ? (store[k] ?? null) : null),
  setItem: (k: string, v: string): void => {
    store[k] = String(v);
  },
  removeItem: (k: string): void => {
    delete store[k];
  },
};

class FakePopStateEvent {
  type: string;
  constructor(type: string) {
    this.type = type;
  }
}

/** Put the stub browser at a path, as if the user had loaded it there. */
function at(pathname: string): void {
  fakeWindow.location.pathname = pathname;
  calls = [];
  events = [];
}

beforeEach(() => {
  calls = [];
  events = [];
  store = {};
  fakeWindow.location.pathname = "/";
  // Assigning through a plain record sidesteps the DOM lib's read-only
  // declarations for window/localStorage.
  const g = globalThis as unknown as Record<string, unknown>;
  g.window = fakeWindow;
  g.localStorage = fakeStorage;
  g.PopStateEvent = FakePopStateEvent;
});

describe("parseLocation", () => {
  it("reads family / scope / tab / detail from a fully-formed path", () => {
    expect(parseLocation("/project/myproj/git/some-repo")).toEqual({
      family: "project",
      scope: "myproj",
      tab: "git",
      detail: "some-repo",
    });
  });

  it("reports no scope for the transient scope-less form", () => {
    // Internal links are deliberately scope-LESS (href="/project/git"), and
    // syncScopeToURL splices the active scope in at render. Telling this apart
    // from a scoped path is what the tab sets are for.
    expect(parseLocation("/project/git")).toEqual({
      family: "project",
      scope: "",
      tab: "git",
      detail: "",
    });
  });

  it("treats a known tab name in the scope slot as the TAB, not a scope", () => {
    // Documented consequence of the disambiguation above: a project literally
    // named "board" is shadowed in the scope slot. Locked here as known
    // behaviour so a future reader sees it is a choice, not an oversight.
    expect(parseLocation("/project/board").scope).toBe("");
    expect(parseLocation("/project/board").tab).toBe("board");
  });

  it("decodes percent-encoded segments", () => {
    const loc = parseLocation("/project/my%20proj/git/a%2Fb");
    expect(loc.scope).toBe("my proj");
    expect(loc.detail).toBe("a/b");
  });

  it("still admits `design`, the pre-rename spelling of `wireframe`", () => {
    // The wireframe tab was spelled `design` before the OpenPencil `ui` tab
    // split off. Old bookmarks and drawing links must keep parsing as a TAB;
    // dropping the alias would re-read them as scope="design" and dead-land
    // every one of them.
    expect(parseLocation("/project/design").tab).toBe("design");
    expect(parseLocation("/project/x/design/abc")).toEqual({
      family: "project",
      scope: "x",
      tab: "design",
      detail: "abc",
    });
    expect(parseLocation("/project/wireframe").tab).toBe("wireframe");
    expect(parseLocation("/project/ui").tab).toBe("ui");
  });

  it("returns an empty family for the root and for unknown families", () => {
    expect(parseLocation("/")).toEqual({ family: "", scope: "", tab: "", detail: "" });
    expect(parseLocation("/nonsense/x").family).toBe("");
  });
});

describe("buildPath", () => {
  it("omits the scope segment when there is no scope yet", () => {
    expect(buildPath("project", "", "git", "")).toBe("/project/git");
  });

  it("encodes the scope and the detail", () => {
    expect(buildPath("project", "my proj", "git", "a/b")).toBe("/project/my%20proj/git/a%2Fb");
  });

  it("round-trips through parseLocation", () => {
    const path = buildPath("claude", "my proj", "session", "abc-123");
    expect(parseLocation(path)).toEqual({
      family: "claude",
      scope: "my proj",
      tab: "session",
      detail: "abc-123",
    });
  });
});

describe("setScope — a scope change drops the detail segment", () => {
  it("lands on the tab, not on the previous scope's detail", () => {
    at("/project/old/git/repo-1");
    setScope("new");
    expect(fakeWindow.location.pathname).toBe("/project/new/git");
    expect(calls).toEqual([{ kind: "push", path: "/project/new/git" }]);
  });

  it("drops the detail for every family, not just git", () => {
    at("/claude/old/session/abc-123");
    setScope("new");
    expect(fakeWindow.location.pathname).toBe("/claude/new/session");
  });

  it("KEEPS the detail on a replace — the boot default or a rename", () => {
    // The one case where the detail is still valid: same scope, new name.
    at("/project/old/git/repo-1");
    setScope("new", true);
    expect(fakeWindow.location.pathname).toBe("/project/new/git/repo-1");
    expect(events).toEqual([]); // a replace must not re-render
  });

  it("persists the scope but leaves the URL alone off a scoped family", () => {
    at("/");
    setScope("myrepo");
    expect(calls).toEqual([]);
    // The root belongs to the claude family, so that is whose scope it is.
    expect(store[CLAUDE_SCOPE_KEY]).toBe("myrepo");
    expect(store[SCOPE_KEY]).toBeUndefined();
  });

  it("defaults a bare claude path to the sessions tab", () => {
    at("/claude/old");
    setScope("new");
    expect(fakeWindow.location.pathname).toBe("/claude/new/sessions");
  });
});

describe("navigate — every path canonicalises, including the replace branch", () => {
  it("splices the remembered scope into a scope-less REPLACE", () => {
    // The docs view auto-opening its first page took exactly this path, and
    // left the URL scope-less until an unrelated render fixed it.
    store[SCOPE_KEY] = "myproj";
    at("/project/myproj/docs");
    navigate("/project/docs/page-1", true);
    expect(fakeWindow.location.pathname).toBe("/project/myproj/docs/page-1");
    expect(events).toEqual([]);
  });

  it("dispatches exactly one popstate on a push", () => {
    at("/claude/myproj/sessions");
    navigate("/claude/myproj/insights");
    expect(events).toEqual(["popstate"]);
    expect(calls).toEqual([{ kind: "push", path: "/claude/myproj/insights" }]);
  });

  it("is a no-op when the path is already current", () => {
    at("/claude/myproj/sessions");
    navigate("/claude/myproj/sessions");
    expect(calls).toEqual([]);
    expect(events).toEqual([]);
  });
});

describe("syncScopeToURL", () => {
  it("expands the root to the default scoped landing", () => {
    store[CLAUDE_SCOPE_KEY] = "myrepo";
    at("/");
    syncScopeToURL();
    expect(fakeWindow.location.pathname).toBe("/claude/myrepo/sessions");
  });

  it("leaves the root scope-less when claude has no remembered scope", () => {
    // "" is a legitimate claude scope — every folder — so the root does not
    // borrow the project family's, which would name something that means
    // nothing over there.
    store[SCOPE_KEY] = "myproj";
    at("/");
    syncScopeToURL();
    expect(fakeWindow.location.pathname).toBe("/claude/sessions");
  });

  it("re-gains the scope segment on a scope-less path", () => {
    store[SCOPE_KEY] = "myproj";
    at("/project/board");
    syncScopeToURL();
    expect(fakeWindow.location.pathname).toBe("/project/myproj/board");
  });

  it("does not touch an already-canonical path, so it never loops", () => {
    store[SCOPE_KEY] = "myproj";
    at("/project/myproj/board");
    syncScopeToURL();
    expect(calls).toEqual([]);
  });

  it("prefers the path's scope over the remembered one, and remembers it", () => {
    // Opening a shared link makes its scope the remembered scope.
    store[SCOPE_KEY] = "myproj";
    at("/project/theirs/board");
    syncScopeToURL();
    expect(calls).toEqual([]);
    expect(store[SCOPE_KEY]).toBe("theirs");
  });
});

// The two families carry two taxonomies — project names on one side, the repos
// the sessions ran in on the other — so a scope means different things in each
// and must be remembered separately. They shared one key until the split, which
// is how switching family carried a project name into the session views.
describe("the families remember their scopes separately", () => {
  it("writes each family's scope to its own key", () => {
    at("/claude/repo-a/sessions");
    setScope("repo-b");
    at("/project/proj-a/board");
    setScope("proj-b");
    expect(store[CLAUDE_SCOPE_KEY]).toBe("repo-b");
    expect(store[SCOPE_KEY]).toBe("proj-b");
  });

  it("does not carry one family's scope into the other's URL", () => {
    store[CLAUDE_SCOPE_KEY] = "repo-a";
    store[SCOPE_KEY] = "proj-a";
    at("/project/board");
    syncScopeToURL();
    expect(fakeWindow.location.pathname).toBe("/project/proj-a/board");
    at("/claude/sessions");
    syncScopeToURL();
    expect(fakeWindow.location.pathname).toBe("/claude/repo-a/sessions");
  });

  it("keeps the other family's scope untouched when one changes", () => {
    store[SCOPE_KEY] = "proj-a";
    at("/claude/repo-a/sessions");
    setScope("repo-b");
    expect(store[SCOPE_KEY]).toBe("proj-a");
  });
});
