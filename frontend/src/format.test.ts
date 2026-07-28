import { describe, expect, it } from "vitest";
import { formatDay } from "./format";

// formatDay exists because `iso.slice(0, 10)` was being used to render a
// cycle's start and end date. That reads the day off the UTC string, so a cycle
// the user started at local midnight east of Greenwich rendered as the day
// BEFORE the one they picked — and it looked correct in testing precisely
// because the seeded fixtures were stored at UTC midnight, where the two agree.
describe("formatDay", () => {
  it("returns the day in the viewer's zone, not the UTC day", () => {
    // 2026-07-27T17:00Z is 2026-07-28 00:00 at UTC+7 and still the 27th in UTC.
    const iso = "2026-07-27T17:00:00.000Z";
    const local = new Date(iso);
    const p = (n: number) => String(n).padStart(2, "0");
    const want = `${local.getFullYear()}-${p(local.getMonth() + 1)}-${p(local.getDate())}`;
    expect(formatDay(iso)).toBe(want);
  });

  it("round-trips a local midnight through toISOString", () => {
    // This is the exact path the cycles form takes: a date input's YYYY-MM-DD
    // becomes local midnight, is sent as an ISO instant, and must render back
    // as the same calendar day.
    const picked = "2026-03-09";
    const sent = new Date(`${picked}T00:00:00`).toISOString();
    expect(formatDay(sent)).toBe(picked);
  });

  it("round-trips an end-of-day instant too", () => {
    const picked = "2026-12-31";
    const sent = new Date(`${picked}T23:59:59`).toISOString();
    expect(formatDay(sent)).toBe(picked);
  });

  it("pads single-digit months and days", () => {
    expect(formatDay(new Date("2026-01-05T12:00:00").toISOString())).toBe("2026-01-05");
  });

  it("falls back to the leading 10 characters when the input is not a date", () => {
    expect(formatDay("not-a-date-at-all")).toBe("not-a-date");
  });
});
