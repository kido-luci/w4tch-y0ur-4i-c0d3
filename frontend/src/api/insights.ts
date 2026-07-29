import { getJSON, buildQuery } from "./client";
import type { ToolCount } from "./sessions";

// --- insights: rework radar (churn) -----------------------------------------

// One session that edited a churned file: its footprint on that file, plus
// the session's own total cost — NOT a per-file share of it (see ChurnFile).
export interface ChurnSessionRef {
  id: string;
  title: string;
  project: string;
  endedAt: string;
  edits: number;
  linesAdded: number;
  linesRemoved: number;
  costUsd: number; // that session's total cost, not this file's share of it
}

// One file's rework footprint across sessions — a file rewritten across many
// sessions is where the loop went in circles.
export interface ChurnFile {
  path: string;
  sessions: number;
  edits: number;
  linesAdded: number;
  linesRemoved: number;
  churnedLines: number; // min(added, removed) — the writing that got unwritten; ranks the list
  lastTouched: string;
  refs: ChurnSessionRef[]; // newest first
}

// The churn view's payload: the ranked files (worst first, capped at `limit`)
// plus how many passed the `min` filter in total, so the UI can say "top 50 of
// 312" instead of silently rendering a capped list as if it were everything.
export interface ChurnResult {
  files: ChurnFile[];
  totalFiles: number;
}

export function getChurn(
  days: number,
  project?: string,
  min?: number,
  limit?: number,
): Promise<ChurnResult> {
  return getJSON<ChurnResult>(`/api/churn${buildQuery({ days, project, min, limit })}`);
}

// --- insights: friction ------------------------------------------------------

// One session that fought back: how often it was stopped or refused, next to
// what it cost. Ranked by interrupts — a session you kept hitting ESC in is a
// session whose prompt (or CLAUDE.md) was wrong.
export interface FrictionSession {
  id: string;
  title: string;
  project: string;
  endedAt: string;
  interrupts: number;
  denials: number;
  costUsd: number; // main + agent
  totalTokens: number; // main + agent
  durationMs: number;
}

// The friction view's payload: the worst sessions (worst-first, capped at
// `limit`) plus the window's totals across ALL sessions — not just the listed
// ones. denialTools is footnote-scale (a handful of denials in the whole
// corpus), not ranking material.
export interface FrictionResult {
  sessions: FrictionSession[];
  totalSessions: number; // sessions with any friction in the window
  interrupts: number;
  denials: number;
  denialTools?: ToolCount[];
}

export function getFriction(days: number, project?: string, limit?: number): Promise<FrictionResult> {
  return getJSON<FrictionResult>(`/api/friction${buildQuery({ days, project, limit })}`);
}

// --- insights: work sizing ---------------------------------------------------

// One session that outgrew its context: how often it compacted, next to what it
// cost and how long it ran.
export interface SizingSession {
  id: string;
  title: string;
  project: string;
  endedAt: string;
  compactions: number;
  costUsd: number; // main + agents
  totalTokens: number;
  durationMs: number;
}

// The sizing block's payload. The medians are the point — and they are a
// correlation, not a cause: compacting doesn't spend money, it marks work that
// was already too big for one sitting.
export interface SizingResult {
  sessions: SizingSession[]; // most compactions first, capped at `limit`
  totalSessions: number; // sessions that compacted at all
  scanned: number; // every session in the window
  medianCostClean: number; // median cost, sessions that never compacted
  medianCostHeavy: number; // median cost, sessions at/over heavyThreshold
  cleanCount: number;
  heavyCount: number;
  heavyThreshold: number;
}

export function getSizing(days: number, project?: string, limit?: number): Promise<SizingResult> {
  return getJSON<SizingResult>(`/api/sizing${buildQuery({ days, project, limit })}`);
}

// --- insights: cost per outcome ----------------------------------------------

// One week of spend measured against what came out of it. costPerPr is the
// week's WHOLE spend divided by the PRs it opened — not the price of a PR.
// Exploring, debugging and arguing about a plan all cost money and open
// nothing; dividing by outcomes rather than costing them individually is the
// point.
export interface LedgerWeek {
  week: string; // ISO, e.g. "2026-W29"
  startsOn: string;
  sessions: number;
  costUsd: number;
  lines: number; // added + removed
  prs: number;
  releases: number;
  costPerPr: number; // 0 when the week opened none
  costPer1kLines: number; // 0 when nothing moved
}

// Weeks run newest first; `total` is the window summed (its week/startsOn are
// empty). No $/ticket — the board has one done card, so it would average one.
export interface LedgerResult {
  weeks: LedgerWeek[];
  total: LedgerWeek;
}

export function getLedger(days: number, project?: string): Promise<LedgerResult> {
  return getJSON<LedgerResult>(`/api/ledger${buildQuery({ days, project })}`);
}
