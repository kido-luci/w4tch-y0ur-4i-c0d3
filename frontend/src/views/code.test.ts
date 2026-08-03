import { beforeEach, describe, expect, it, vi } from "vitest";

// The first test of a view module, and the reason the suite grew a jsdom
// environment. What it pins is the case multi-checkout introduced: a project can
// be bound to two clones of ONE repo, and both carry the same Claude `folder`.
// Keying the row on that folder — which is what the list did before — gives two
// rows with the same name and the same link, so one of them is unreachable and
// neither says which is which. checkoutKey (the last two path segments) is what
// separates them, and the list and the detail must derive it identically or a
// link opens the wrong checkout.

const getGit = vi.fn();
vi.mock("../api", () => ({ getGit: (...a: unknown[]) => getGit(...a) }));
vi.mock("../scope", () => ({ getScope: () => "some-scope" }));
vi.mock("../app/live", () => ({ showError: vi.fn() }));

const { renderCodeView } = await import("./code");

/** A row as /api/git reports it — only the fields the list reads. */
function repo(root: string, folder: string, branch = "main") {
  return {
    root,
    folder,
    isRepo: true,
    branch,
    detached: false,
    staged: 0,
    unstaged: 0,
    untracked: 0,
    hasUpstream: true,
    ahead: 0,
    behind: 0,
    commits: [],
  };
}

async function mount(repos: unknown[]): Promise<HTMLElement> {
  getGit.mockResolvedValue({ repos });
  const host = document.createElement("div");
  renderCodeView(host);
  await vi.waitFor(() => {
    if (!host.querySelector(".git-row") && !host.querySelector(".empty-state")) {
      throw new Error("not rendered yet");
    }
  });
  return host;
}

describe("code view rows", () => {
  beforeEach(() => getGit.mockReset());

  it("gives two clones of one repo distinct links and distinct names", async () => {
    // Both checkouts of the same repo, so both carry the same folder — this is
    // what the server sends once a project is bound to more than one root.
    const host = await mount([
      repo("/Users/me/dev/memoirme", "memoirme", "main"),
      repo("/Users/me/dev/copies/memoirme", "memoirme", "feature-x"),
    ]);

    const rows = [...host.querySelectorAll<HTMLAnchorElement>("a.git-row")];
    expect(rows).toHaveLength(2);

    const hrefs = rows.map((a) => a.getAttribute("href"));
    expect(hrefs).toEqual(["/project/code/dev%2Fmemoirme", "/project/code/copies%2Fmemoirme"]);
    expect(new Set(hrefs).size).toBe(2); // the whole point: not the same link twice

    const names = rows.map((a) => a.querySelector(".git-row-name")?.textContent);
    expect(names).toEqual(["dev/memoirme", "copies/memoirme"]);
  });

  it("titles a row with the full root, since the name is only two segments", async () => {
    const host = await mount([repo("/Users/me/dev/memoirme", "memoirme")]);
    const name = host.querySelector(".git-row-name");
    expect(name?.getAttribute("title")).toBe("/Users/me/dev/memoirme");
  });

  it("says the scope is empty rather than rendering nothing", async () => {
    const host = await mount([]);
    expect(host.querySelector(".empty-state")?.textContent).toMatch(/no repos in this scope/);
  });
});
