import { getJSON, buildQuery } from "./client";

export type ModelFamily = "opus" | "sonnet" | "haiku" | "fable" | string;

export interface TokenBreakdown {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWrite5mTokens: number;
  cacheWrite1hTokens: number;
}

export interface ModelUsage {
  model: ModelFamily;
  tokens: number;
  costUsd: number;
}

export interface ToolCount {
  name: string;
  count: number;
}

export interface ActivitySlot {
  count: number;
  tools?: Record<string, number>;
}

// One tool use on the timeline — name + timestamp only, never inputs.
// Detail-only; long lanes arrive evenly downsampled (see *Dropped counts).
export interface ToolEvent {
  name: string;
  ts: string;
}

export interface FlowNode {
  kind: string; // explore | edit | run | delegate | other
  label: string;
  count: number;
  tools?: ToolCount[];
  startTs: string;
  endTs: string;
  agentId?: string;
}

export interface Milestone {
  kind: string; // plan | branch | commit | pr | release
  label: string;
  url?: string; // pr node -> the PR link
  ts: string;
}

// One branch-scoped unit of work: branch cut → … → the release that closed it.
export interface MilestoneGroup {
  title: string;
  milestones: Milestone[];
}

export interface Session {
  id: string;
  project: string;
  slug: string;
  title: string;
  gitBranch: string;
  prUrl?: string;
  startedAt: string;
  endedAt: string;
  durationMs: number;
  models: ModelFamily[];
  messageCount: number;
  compactCount: number;
  // How often the user stopped this session mid-flight, and how often they
  // refused a tool permission.
  interrupts: number;
  denials: number;
  tokens: TokenBreakdown;
  totalTokens: number;
  costUsd: number;
  linesAdded: number;
  linesRemoved: number;
  filesChanged: number;
  agentCount: number;
  agentTokens: number;
  agentCostUsd: number;
  contextTokens: number;
  contextWindow: number;
  modelBreakdown?: ModelUsage[];
  // Main-thread-only breakdowns (detail responses only).
  mainToolStats?: ToolStats | null;
  mainFilesChanged: number;
  mainTools?: ToolCount[];
  mainActivity?: ActivitySlot[];
  mainFlow?: FlowNode[];
  mainToolEvents?: ToolEvent[];
  mainToolEventsDropped?: number;
  // Detail-only: ISO timestamps of each interrupt, for the timeline's ticks.
  // No tool is attached on purpose — which tool was running isn't recoverable.
  interruptTimes?: string[];
  milestones?: Milestone[];
  milestoneGroups?: MilestoneGroup[];
  running: boolean;
  archived: boolean;
}

export interface ToolStats {
  readCount: number;
  searchCount: number;
  bashCount: number;
  editFileCount: number;
  linesAdded: number;
  linesRemoved: number;
  otherToolCount: number;
}

export interface AgentRun {
  id: string;
  sessionId: string;
  parentAgentId: string;
  agentType: string;
  description: string;
  model: ModelFamily;
  modelId: string;
  status: string;
  spawnDepth: number;
  startedAt: string;
  endedAt: string;
  durationMs: number;
  tokens: TokenBreakdown;
  totalTokens: number;
  messageCount: number;
  toolUseCount: number;
  toolStats: ToolStats | null;
  costUsd: number;
  linesAdded: number;
  linesRemoved: number;
  filesChanged: number;
  tools?: ToolCount[];
  toolEvents?: ToolEvent[];
  toolEventsDropped?: number;
  running: boolean;
}

export interface SessionDetail extends Session {
  agents: AgentRun[];
}

export interface Stats {
  sessions: number;
  totalTokens: number;
  totalCostUsd: number;
  agentSpawns: number;
  running: number;
}

export interface ActivityDay {
  date: string; // YYYY-MM-DD, local
  sessions: number;
  tokens: number;
  costUsd: number;
}

export interface Activity {
  weeks: number;
  days: ActivityDay[];
}

export function getSessions(days: number, project?: string, status?: string): Promise<Session[]> {
  return getJSON<Session[]>(`/api/sessions${buildQuery({ days, project, status })}`);
}

export async function getSession(id: string): Promise<SessionDetail> {
  const session = await getJSON<SessionDetail>(`/api/sessions/${encodeURIComponent(id)}`);
  // The API omits `agents` entirely for sessions without subagents.
  session.agents ??= [];
  return session;
}

export interface SummariesResponse {
  summaries: string[] | null; // aligned to milestoneGroups by index
  fresh: boolean; // false = nothing cached, or the session grew past the cache
}

/** Cached milestone-group summaries — a local disk read, never an AI call. */
export function getSummaries(id: string): Promise<SummariesResponse> {
  return getJSON<SummariesResponse>(`/api/sessions/${encodeURIComponent(id)}/summaries`);
}

/** Generate summaries via the local claude CLI (the one call that costs tokens). */
export async function postSummarize(id: string): Promise<string[]> {
  const res = await fetch(`/api/sessions/${encodeURIComponent(id)}/summarize`, { method: "POST" });
  const body = (await res.json().catch(() => null)) as {
    summaries?: string[];
    error?: string;
  } | null;
  if (!res.ok) {
    throw new Error(body?.error ?? `summarize -> ${res.status} ${res.statusText}`);
  }
  return body?.summaries ?? [];
}

export function getStats(days: number, project?: string, status?: string): Promise<Stats> {
  return getJSON<Stats>(`/api/stats${buildQuery({ days, project, status })}`);
}

export function getActivity(weeks: number, project?: string): Promise<Activity> {
  return getJSON<Activity>(`/api/activity${buildQuery({ weeks, project })}`);
}
