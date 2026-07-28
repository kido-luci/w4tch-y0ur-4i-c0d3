import { describe, expect, it } from "vitest";
import { matchesQuery } from "./boardQuery";
import type { BoardQuery, Todo } from "./api";

// matchesQuery is the ONE place a saved view's stored query is interpreted, and
// a saved view's `query` is opaque JSON the server never validates. So the two
// properties worth pinning are: an absent field filters NOTHING (or an old saved
// view starts hiding cards after an unrelated change), and a present one filters
// exactly what it says.
const card = (over: Partial<Todo> = {}): Todo => ({
  id: "t1",
  seq: 7,
  title: "Fix the login redirect",
  status: "backlog",
  order: 1,
  createdAt: "2026-07-01T00:00:00Z",
  ...over,
});

const q = (over: Partial<BoardQuery> = {}): BoardQuery => ({ ...over });

describe("an empty query", () => {
  it("matches every card", () => {
    expect(matchesQuery(card(), {})).toBe(true);
    expect(matchesQuery(card({ kind: "bug", priority: 4, estimate: 8 }), {})).toBe(true);
  });

  it("treats an empty list as absent rather than as 'match nothing'", () => {
    // A saved view round-tripped through JSON can come back with [] where the
    // UI meant "no filter"; that must not blank the board.
    expect(matchesQuery(card(), q({ kinds: [], statuses: [], labels: [] }))).toBe(true);
  });
});

describe("text", () => {
  it("matches the title case-insensitively", () => {
    expect(matchesQuery(card(), q({ text: "LOGIN" }))).toBe(true);
    expect(matchesQuery(card(), q({ text: "logout" }))).toBe(false);
  });

  it("searches the note too", () => {
    expect(matchesQuery(card({ note: "see RFC-42" }), q({ text: "rfc-42" }))).toBe(true);
  });

  it("matches the card number, with or without the hash", () => {
    expect(matchesQuery(card(), q({ text: "#7" }))).toBe(true);
    expect(matchesQuery(card(), q({ text: "7" }))).toBe(true);
    expect(matchesQuery(card(), q({ text: "#8" }))).toBe(false);
  });
});

describe("kind, status and labels are OR within a group", () => {
  it("keeps a card whose kind is any of the listed ones", () => {
    expect(matchesQuery(card({ kind: "bug" }), q({ kinds: ["bug", "epic"] }))).toBe(true);
    expect(matchesQuery(card({ kind: "story" }), q({ kinds: ["bug", "epic"] }))).toBe(false);
  });

  it("reads a card with no kind as a task", () => {
    expect(matchesQuery(card(), q({ kinds: ["task"] }))).toBe(true);
    expect(matchesQuery(card(), q({ kinds: ["epic"] }))).toBe(false);
  });

  it("filters by status", () => {
    expect(matchesQuery(card({ status: "in-review" }), q({ statuses: ["in-review"] }))).toBe(true);
    expect(matchesQuery(card({ status: "backlog" }), q({ statuses: ["in-review"] }))).toBe(false);
  });

  it("keeps a card sharing at least one label", () => {
    const t = card({ labels: ["infra", "self-host"] });
    expect(matchesQuery(t, q({ labels: ["self-host"] }))).toBe(true);
    expect(matchesQuery(t, q({ labels: ["docs", "infra"] }))).toBe(true);
    expect(matchesQuery(t, q({ labels: ["docs"] }))).toBe(false);
    expect(matchesQuery(card(), q({ labels: ["infra"] }))).toBe(false);
  });
});

describe("cycle and priority", () => {
  it("filters to one cycle", () => {
    expect(matchesQuery(card({ cycleId: "c1" }), q({ cycleId: "c1" }))).toBe(true);
    expect(matchesQuery(card({ cycleId: "c2" }), q({ cycleId: "c1" }))).toBe(false);
    expect(matchesQuery(card(), q({ cycleId: "c1" }))).toBe(false);
  });

  it("treats minPriority as a floor, and 0 as no filter", () => {
    expect(matchesQuery(card({ priority: 3 }), q({ minPriority: 3 }))).toBe(true);
    expect(matchesQuery(card({ priority: 4 }), q({ minPriority: 3 }))).toBe(true);
    expect(matchesQuery(card({ priority: 2 }), q({ minPriority: 3 }))).toBe(false);
    // A card with no priority reads as 0, and minPriority 0 filters nothing.
    expect(matchesQuery(card(), q({ minPriority: 0 }))).toBe(true);
    expect(matchesQuery(card(), q({ minPriority: 1 }))).toBe(false);
  });
});

describe("unestimatedOnly", () => {
  it("keeps only cards the burndown cannot see", () => {
    expect(matchesQuery(card(), q({ unestimatedOnly: true }))).toBe(true);
    expect(matchesQuery(card({ estimate: 0 }), q({ unestimatedOnly: true }))).toBe(true);
    expect(matchesQuery(card({ estimate: 3 }), q({ unestimatedOnly: true }))).toBe(false);
  });

  it("filters nothing when false", () => {
    expect(matchesQuery(card({ estimate: 3 }), q({ unestimatedOnly: false }))).toBe(true);
  });
});

describe("groups are AND across each other", () => {
  it("requires every present filter to pass", () => {
    const t = card({ kind: "bug", labels: ["infra"], estimate: 0, priority: 4, cycleId: "c1" });
    expect(
      matchesQuery(t, q({ kinds: ["bug"], labels: ["infra"], unestimatedOnly: true, cycleId: "c1" })),
    ).toBe(true);
    // One failing group is enough to drop it.
    expect(
      matchesQuery(t, q({ kinds: ["bug"], labels: ["docs"], unestimatedOnly: true, cycleId: "c1" })),
    ).toBe(false);
  });
});
