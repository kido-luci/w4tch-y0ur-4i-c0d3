import { describe, expect, it } from "vitest";
import type { Milestone } from "../api";
import { createSummaryFetchGate, summariesSignature } from "./summaryGate";

// The detail view re-renders wholesale on every `session-updated` SSE event,
// and renderSessionMilestones used to fire GET /api/sessions/{id}/summaries
// unconditionally on each of those renders. On a RUNNING session the transcript
// is appended constantly, so this was one request per update, forever:
// measured at 13 requests in 91 seconds on a live session, still climbing. It
// read as "4 duplicate fetches on load" only because that is how long the page
// happened to be open.
//
// The response can only change when the milestones change — the server's
// freshness flag is `storedHash == sha256 over each milestone's "kind|label"`
// (internal/summarize). So the fetch belongs to the milestones, not to the
// repaint, and the signature below mirrors that hash's input so the gate is
// never less sensitive than the server's own test.

const m = (kind: string, label: string): Milestone =>
  ({ kind, label, ts: "2026-07-29T10:00:00Z" }) as Milestone;

describe("summariesSignature", () => {
  it("is stable for the same milestones", () => {
    const a = [m("commit", "fix the thing"), m("pr", "#12")];
    const b = [m("commit", "fix the thing"), m("pr", "#12")];
    expect(summariesSignature(a)).toBe(summariesSignature(b));
  });

  it("changes when a milestone is appended", () => {
    const before = [m("commit", "fix the thing")];
    const after = [...before, m("pr", "#12")];
    expect(summariesSignature(after)).not.toBe(summariesSignature(before));
  });

  it("changes when a label changes in place", () => {
    // A re-parse can relabel the tail (a plan's label firms up as it is
    // written), which flips the server's hash without changing the count.
    expect(summariesSignature([m("plan", "draft")])).not.toBe(
      summariesSignature([m("plan", "final")]),
    );
  });

  it("changes when a kind changes", () => {
    expect(summariesSignature([m("commit", "x")])).not.toBe(summariesSignature([m("pr", "x")]));
  });

  it("distinguishes a split label from its concatenation", () => {
    // Guards the separator: "a|b" as one label must not collide with two
    // milestones whose fields happen to join to the same string.
    expect(summariesSignature([m("commit", "a|b")])).not.toBe(
      summariesSignature([m("commit", "a"), m("b", "")]),
    );
  });
});

describe("summary fetch gate", () => {
  it("fetches on first sight of a session", () => {
    const gate = createSummaryFetchGate();
    expect(gate("s1", [m("commit", "one")])).toBe(true);
  });

  it("does NOT fetch again when the milestones are unchanged", () => {
    // This is the regression: a repaint with identical milestones must not
    // re-hit the endpoint, however many times the view re-renders.
    const gate = createSummaryFetchGate();
    const ms = [m("commit", "one")];
    expect(gate("s1", ms)).toBe(true);
    for (let i = 0; i < 20; i++) {
      expect(gate("s1", [...ms])).toBe(false);
    }
  });

  it("fetches again once the milestones change", () => {
    const gate = createSummaryFetchGate();
    const ms = [m("commit", "one")];
    expect(gate("s1", ms)).toBe(true);
    expect(gate("s1", ms)).toBe(false);
    expect(gate("s1", [...ms, m("pr", "#12")])).toBe(true);
    expect(gate("s1", [...ms, m("pr", "#12")])).toBe(false);
  });

  it("tracks each session independently", () => {
    const gate = createSummaryFetchGate();
    const ms = [m("commit", "one")];
    expect(gate("s1", ms)).toBe(true);
    expect(gate("s2", ms)).toBe(true); // same milestones, different session
    expect(gate("s1", ms)).toBe(false);
    expect(gate("s2", ms)).toBe(false);
  });

  it("treats an empty milestone list as its own signature", () => {
    const gate = createSummaryFetchGate();
    expect(gate("s1", [])).toBe(true);
    expect(gate("s1", [])).toBe(false);
    expect(gate("s1", [m("commit", "one")])).toBe(true);
  });
});
