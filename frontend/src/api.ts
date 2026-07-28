// Typed fetch helpers + SSE subscription for the local API.

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

async function getJSON<T>(url: string): Promise<T> {
  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`${url} -> ${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

function buildQuery(params: Record<string, string | number | undefined>): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined || value === "") continue;
    search.set(key, String(value));
  }
  const qs = search.toString();
  return qs ? `?${qs}` : "";
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

// The project registry — user-owned projects, decoupled from the raw ~/.claude
// scan. Each owns the Claude folders (session cwd-basenames) it stands for.
export interface Project {
  name: string;
  folders: string[];
  hidden: boolean;
  ord: number;
  parent: string; // name of the project this nests under in the rail tree, "" = top-level
  logoVersion: number; // ms of the last logo write, 0 = no logo (also the cache-buster)
}

/** Visible project names (registry order, hidden excluded) — the label
    datalists' suggestions and the sessions filter's options. Sourced from the
    registry now, not the raw index scan. */
export async function getProjects(): Promise<string[]> {
  const reg = await getJSON<Project[]>("/api/projects/registry");
  return reg.filter((p) => !p.hidden).map((p) => p.name);
}

/** The full registry (including hidden) — the rail and its manager. */
export function getProjectRegistry(): Promise<Project[]> {
  return getJSON<Project[]>("/api/projects/registry");
}

/** Claude folders the index reports that no registry project owns yet. */
export function getUnmappedFolders(): Promise<string[]> {
  return getJSON<string[]>("/api/projects/unmapped");
}

/** Create or replace one project (PUT upsert): its owned folders, hidden flag
    and rail order. Claimed folders are stripped from other projects server-side. */
export function putProject(
  name: string,
  body: { folders: string[]; hidden: boolean; ord: number; parent: string },
): Promise<Project> {
  return sendJSON<Project>(`/api/projects/${encodeURIComponent(name)}`, "PUT", body);
}

export function deleteProject(name: string): Promise<void> {
  return sendJSON<void>(`/api/projects/${encodeURIComponent(name)}`, "DELETE");
}

/** Rename a project and cascade the new name across every label that carried
    the old one (cards, pages, drawings, group members). A user-data change. */
export function renameProject(from: string, to: string): Promise<{ name: string }> {
  return sendJSON<{ name: string }>(`/api/projects/${encodeURIComponent(from)}/rename`, "POST", { to });
}

/** A project's logo URL, cache-busted by its version (0 → no logo). */
export function projectLogoURL(name: string, version: number): string {
  return `/api/projects/${encodeURIComponent(name)}/logo?v=${version}`;
}

/** Upload a project's logo (raw image bytes; the Content-Type rides the blob). */
export async function putProjectLogo(name: string, blob: Blob): Promise<void> {
  const res = await fetch(`/api/projects/${encodeURIComponent(name)}/logo`, {
    method: "PUT",
    headers: { "Content-Type": blob.type || "image/png" },
    body: blob,
  });
  if (!res.ok) throw new Error(`logo upload -> ${res.status} ${res.statusText}`);
}

export function deleteProjectLogo(name: string): Promise<void> {
  return sendJSON<void>(`/api/projects/${encodeURIComponent(name)}/logo`, "DELETE");
}

// --- todo board -----------------------------------------------------------

export type TodoStatus = "backlog" | "doing" | "done";

export interface Todo {
  id: string;
  seq: number; // stable human-friendly card number (#12)
  title: string;
  note?: string;
  repo?: string; // project name, as /api/projects reports it
  labels?: string[];
  status: TodoStatus;
  order: number; // sort key within a column (midpoints on drag & drop)
  createdAt: string;
  linkedSessionIds?: string[]; // real sessions the ticket spans, link order
  linkedDrawingIds?: string[]; // wireframes in the design library, link order
  linkedDocIds?: string[]; // docs-wiki pages, link order
  snapshot?: TodoSnapshot; // frozen at done, cleared when the card leaves done
}

// The linked sessions' summed numbers at the moment the todo was done
// (server-taken).
export interface TodoSnapshot {
  tokens: number; // main + agents
  costUsd: number;
  agents: number;
  durationMs: number;
  sessions: number; // how many linked sessions the numbers cover
  takenAt: string;
}

/** Send a JSON body and decode a JSON reply, surfacing the API's error field. */
async function sendJSON<T>(url: string, method: string, body?: unknown): Promise<T> {
  const res = await fetch(url, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  const parsed = (await res.json().catch(() => null)) as ({ error?: string } & T) | null;
  if (!res.ok) {
    throw new Error(parsed?.error ?? `${url} -> ${res.status} ${res.statusText}`);
  }
  return parsed as T;
}

export function getTodos(): Promise<Todo[]> {
  return getJSON<Todo[]>("/api/todos");
}

export function createTodo(input: {
  title: string;
  note?: string;
  repo?: string;
  status?: TodoStatus;
}): Promise<Todo> {
  return sendJSON<Todo>("/api/todos", "POST", input);
}

export function patchTodo(
  id: string,
  patch: Partial<
    Pick<
      Todo,
      | "title"
      | "note"
      | "repo"
      | "labels"
      | "status"
      | "order"
      | "linkedSessionIds"
      | "linkedDrawingIds"
      | "linkedDocIds"
    >
  >,
): Promise<Todo> {
  return sendJSON<Todo>(`/api/todos/${encodeURIComponent(id)}`, "PATCH", patch);
}

export function deleteTodo(id: string): Promise<void> {
  return sendJSON<void>(`/api/todos/${encodeURIComponent(id)}`, "DELETE");
}

// --- design library ---------------------------------------------------------

// One wireframe's metadata; the scene itself is a standard .excalidraw JSON
// document fetched/saved separately (it can be large).
export interface Drawing {
  id: string;
  name: string;
  // The tab this drawing belongs to: a project name or a free-text custom
  // label; "" is the Ungrouped tab.
  group: string;
  // Free-text topic tags — many-to-many, unlike group's one tab: the grid
  // renders a section per topic, a drawing under each of its tags.
  topics: string[];
  createdAt: string;
  updatedAt: string;
  // The updatedAt the cached thumbnail was rendered from; the thumbnail is
  // fresh iff it equals updatedAt (see hasFreshThumbnail).
  thumbUpdatedAt: string;
  // The updatedAt the last publish sent to the review backend (zero time =
  // never published; fresh iff it equals updatedAt — same idiom as thumbs).
  publishedAt: string;
}

/** Whether the server's cached thumbnail matches the current scene version. */
export function hasFreshThumbnail(d: Drawing): boolean {
  return d.thumbUpdatedAt === d.updatedAt;
}

/** Whether the drawing has ever been pushed to the review backend. */
export function isPublished(d: Drawing): boolean {
  return !!d.publishedAt && !d.publishedAt.startsWith("0001-");
}

/** Whether the published copy matches the current scene version. */
export function isPublishFresh(d: Drawing): boolean {
  return isPublished(d) && d.publishedAt === d.updatedAt;
}

/** Pushes the drawing to the review backend; resolves to the review URL. */
export async function publishDrawing(id: string): Promise<string> {
  const res = await fetch(`/api/drawings/${encodeURIComponent(id)}/publish`, { method: "POST" });
  const parsed = (await res.json().catch(() => null)) as { error?: string; reviewUrl?: string } | null;
  if (!res.ok || !parsed?.reviewUrl) {
    throw new Error(parsed?.error ?? `publish failed (${res.status})`);
  }
  return parsed.reviewUrl;
}

/** Cache-busted URL of a drawing's cached thumbnail PNG. */
export function drawingThumbnailURL(d: Drawing): string {
  return `/api/drawings/${encodeURIComponent(d.id)}/thumbnail?v=${encodeURIComponent(d.thumbUpdatedAt)}`;
}

/** Upload a client-rendered thumbnail for the scene version baseUpdatedAt. */
export async function putDrawingThumbnail(id: string, png: Blob, baseUpdatedAt: string): Promise<void> {
  const res = await fetch(`/api/drawings/${encodeURIComponent(id)}/thumbnail`, {
    method: "PUT",
    headers: { "Content-Type": "image/png", "X-Base-Updated-At": baseUpdatedAt },
    body: png,
  });
  if (!res.ok) {
    throw new Error(`thumbnail -> ${res.status} ${res.statusText}`);
  }
}

export function getDrawings(): Promise<Drawing[]> {
  return getJSON<Drawing[]>("/api/drawings");
}

export function createDrawing(name: string, group = ""): Promise<Drawing> {
  return sendJSON<Drawing>("/api/drawings", "POST", { name, group });
}

/** Move a drawing to a group tab (project name or custom label; "" = Ungrouped). */
export function moveDrawing(id: string, group: string): Promise<Drawing> {
  return sendJSON<Drawing>(`/api/drawings/${encodeURIComponent(id)}`, "PATCH", { group });
}

/** Replaces a drawing's topic tags (the full new set; empty untags). */
export function setDrawingTopics(id: string, topics: string[]): Promise<Drawing> {
  return sendJSON<Drawing>(`/api/drawings/${encodeURIComponent(id)}`, "PATCH", { topics });
}

/** The raw .excalidraw scene document (parsed JSON). */
export function getDrawingContent(id: string): Promise<unknown> {
  return getJSON<unknown>(`/api/drawings/${encodeURIComponent(id)}`);
}

/** putDrawingContent rejection meaning the drawing was saved elsewhere since
 *  baseUpdatedAt (another tab, an MCP client) — resolve, don't retry. */
export class DrawingConflictError extends Error {}

/** Save an already-serialized .excalidraw document; returns fresh metadata.
 *  With baseUpdatedAt set, the save is conditional and throws
 *  DrawingConflictError instead of clobbering a newer save. */
export async function putDrawingContent(id: string, json: string, baseUpdatedAt?: string): Promise<Drawing> {
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  if (baseUpdatedAt) headers["X-Base-Updated-At"] = baseUpdatedAt;
  const res = await fetch(`/api/drawings/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers,
    body: json,
  });
  const parsed = (await res.json().catch(() => null)) as ({ error?: string } & Drawing) | null;
  if (res.status === 409) {
    throw new DrawingConflictError(parsed?.error ?? "drawing changed elsewhere");
  }
  if (!res.ok) {
    throw new Error(parsed?.error ?? `save -> ${res.status} ${res.statusText}`);
  }
  return parsed as Drawing;
}

/** Fork a drawing: new entry named "<name> (copy)" with the same scene. */
export function duplicateDrawing(id: string): Promise<Drawing> {
  return sendJSON<Drawing>(`/api/drawings/${encodeURIComponent(id)}/duplicate`, "POST");
}

export function renameDrawing(id: string, name: string): Promise<Drawing> {
  return sendJSON<Drawing>(`/api/drawings/${encodeURIComponent(id)}`, "PATCH", { name });
}

export function deleteDrawing(id: string): Promise<void> {
  return sendJSON<void>(`/api/drawings/${encodeURIComponent(id)}`, "DELETE");
}

// One wiki page's metadata; the markdown body is fetched/saved separately (via
// getDoc / putDocBody) so the tree stays light. parentId "" is a top-level page.
export interface Doc {
  id: string;
  title: string;
  parentId: string;
  group: string; // project scope; "" inherits from the parent tree (unscoped on a root). A child's own group overrides.
  order: number;
  createdAt: string;
  updatedAt: string;
}

/** A page plus its markdown body (the GET /api/docs/{id} payload). */
export interface DocWithBody extends Doc {
  body: string;
}

export function getDocs(): Promise<Doc[]> {
  return getJSON<Doc[]>("/api/docs");
}

export function getDoc(id: string): Promise<DocWithBody> {
  return getJSON<DocWithBody>(`/api/docs/${encodeURIComponent(id)}`);
}

export function createDoc(input: { title: string; parentId?: string }): Promise<Doc> {
  return sendJSON<Doc>("/api/docs", "POST", input);
}

/** putDocBody rejection meaning the page was saved elsewhere since
 *  baseUpdatedAt (another tab, an MCP client) — resolve, don't retry. */
export class DocConflictError extends Error {}

/** Save a page's markdown body; returns fresh metadata. With baseUpdatedAt set,
 *  the save is conditional and throws DocConflictError instead of clobbering a
 *  newer save. */
export async function putDocBody(id: string, body: string, baseUpdatedAt?: string): Promise<Doc> {
  const headers: Record<string, string> = { "Content-Type": "text/markdown" };
  if (baseUpdatedAt) headers["X-Base-Updated-At"] = baseUpdatedAt;
  const res = await fetch(`/api/docs/${encodeURIComponent(id)}`, {
    method: "PUT",
    headers,
    body,
  });
  const parsed = (await res.json().catch(() => null)) as ({ error?: string } & Doc) | null;
  if (res.status === 409) {
    throw new DocConflictError(parsed?.error ?? "doc changed elsewhere");
  }
  if (!res.ok) {
    throw new Error(parsed?.error ?? `save -> ${res.status} ${res.statusText}`);
  }
  return parsed as Doc;
}

/** Update a page's metadata: rename (title), re-nest (parentId; "" = top level),
 *  re-scope (group; "" = unscoped), or reorder (order). Only the fields present
 *  are touched. */
export function patchDoc(
  id: string,
  patch: { title?: string; parentId?: string; group?: string; order?: number },
): Promise<Doc> {
  return sendJSON<Doc>(`/api/docs/${encodeURIComponent(id)}`, "PATCH", patch);
}

export function deleteDoc(id: string): Promise<void> {
  return sendJSON<void>(`/api/docs/${encodeURIComponent(id)}`, "DELETE");
}

// One named set of project names — the nav's global scope can cover several
// repos at once through one of these.
export interface ProjectGroup {
  name: string;
  projects: string[];
}

export function getGroups(): Promise<ProjectGroup[]> {
  return getJSON<ProjectGroup[]>("/api/groups");
}

/** Create or replace one group's member set (PUT upsert). */
export function putGroup(name: string, projects: string[]): Promise<ProjectGroup> {
  return sendJSON<ProjectGroup>(`/api/groups/${encodeURIComponent(name)}`, "PUT", { projects });
}

export function deleteGroup(name: string): Promise<void> {
  return sendJSON<void>(`/api/groups/${encodeURIComponent(name)}`, "DELETE");
}

export function getStats(days: number, project?: string, status?: string): Promise<Stats> {
  return getJSON<Stats>(`/api/stats${buildQuery({ days, project, status })}`);
}

export function getActivity(weeks: number, project?: string): Promise<Activity> {
  return getJSON<Activity>(`/api/activity${buildQuery({ weeks, project })}`);
}

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

// --- transcript search -------------------------------------------------------

// One match inside one session's transcript. Snippets are cut server-side to a
// fixed width — enough to recognise the moment, never the whole message.
export interface SearchHit {
  sessionId: string;
  title: string;
  project: string;
  ts: string;
  role: string; // user | assistant
  snippet: string;
}

// A query-time grep — nothing is indexed. `matched` counts every hit found;
// `hits` carries at most `limit` of them, and `truncated` says so.
export interface SearchResult {
  hits: SearchHit[];
  matched: number;
  files: number; // transcripts opened
  truncated: boolean;
  tookMs: number;
}

export function search(
  q: string,
  days: number,
  project?: string,
  limit?: number,
): Promise<SearchResult> {
  return getJSON<SearchResult>(`/api/search${buildQuery({ q, days, project, limit })}`);
}

// --- ship history ------------------------------------------------------------

// One recorded `make check` / `make release` run, dropped into ~/.wyac/ships
// by scripts/wyac-ship and indexed by the server (see ships.go). `log` rides
// along only when the query asks (withLog) — it is the payload's whole weight.
export interface ShipRecord {
  file: string;
  project: string;
  kind: string; // check | release
  version?: string;
  sha?: string;
  exit: number;
  durationMs: number;
  ts: string;
  log?: string;
  // The session running the project when the run happened, if the server
  // could match one — absent when none did.
  sessionId?: string;
  sessionTitle?: string;
}

// A capped list that never reads as the whole answer: `total` counts
// everything the filter matched, `ships` carries at most `limit`.
export interface ShipsResult {
  ships: ShipRecord[];
  total: number;
}

export function getShips(
  days: number,
  project?: string,
  limit?: number,
  withLog?: boolean,
): Promise<ShipsResult> {
  return getJSON<ShipsResult>(
    `/api/ships${buildQuery({ days, project, limit, log: withLog ? 1 : undefined })}`,
  );
}

// --- web analytics (/service/cloudflare) --------------------------------------
// Read proxies over Cloudflare + Search Console (see webstats.go server-side).
// Shapes mirror the Go Result structs; nil Go slices arrive as null, so the
// view guards every list with `?? []`.

export interface NameCount {
  name: string;
  count: number;
}

export interface CFTimePoint {
  ts: string; // RFC3339 hour or YYYY-MM-DD day
  requests: number;
  cached: number;
  bytes: number;
  err4xx: number;
  err5xx: number;
  uniques: number;
  threats: number;
}

export interface CFCodeCount {
  code: number;
  requests: number;
}

export interface CFTraffic {
  totals: {
    requests: number;
    cached: number;
    bytes: number;
    cachedBytes: number;
    uniques: number;
    threats: number;
  };
  series: CFTimePoint[] | null;
  statusCodes: CFCodeCount[] | null;
  topCountries: NameCount[] | null;
}

export interface CFSecEvent {
  datetime: string;
  action: string;
  clientIP: string;
  country: string;
  host: string;
  path: string;
  ruleId: string;
  source: string;
}

export interface CFSecurity {
  windowHours: number;
  total: number;
  byAction: NameCount[] | null;
  byCountry: NameCount[] | null;
  byHost: NameCount[] | null;
  byRule: NameCount[] | null;
  recent: CFSecEvent[] | null;
}

export interface CFRUMPoint {
  ts: string;
  pageviews: number;
  visits: number;
}

export interface CFRUM {
  pageviews: number;
  visits: number;
  series: CFRUMPoint[] | null;
  topPaths: NameCount[] | null;
  topHosts: NameCount[] | null;
  topCountries: NameCount[] | null;
  topReferers: NameCount[] | null;
}

// Sections are independent server-side: a failed one is null with its reason
// under `errors`, so one unavailable dataset never blanks the whole view.
export interface CFResult {
  range: string;
  host?: string;
  traffic: CFTraffic | null;
  security: CFSecurity | null;
  rum: CFRUM | null;
  errors?: Record<string, string>;
}

export interface GSCRow {
  name: string;
  clicks: number;
  impressions: number;
  ctr: number;
  position: number;
}

export interface GSCDayPoint {
  date: string;
  clicks: number;
  impressions: number;
  ctr: number;
  position: number;
}

export interface GSCSummary {
  clicks: number;
  impressions: number;
  ctr: number;
  position: number; // impression-weighted
}

export interface GSCQueryPage {
  page: string;
  query: string;
  clicks: number;
  impressions: number;
  ctr: number;
  position: number;
}

export interface GSCSitemap {
  path: string;
  lastSubmitted: string;
  lastDownloaded: string;
  isPending: boolean;
  errors: number;
  warnings: number;
}

export interface GSCResult {
  range: string;
  property: string;
  startDate: string;
  endDate: string;
  summary: GSCSummary | null;
  series: GSCDayPoint[] | null;
  topQueries: GSCRow[] | null;
  topPages: GSCRow[] | null;
  queryPages: GSCQueryPage[] | null;
  byHost: GSCRow[] | null;
  devices: GSCRow[] | null;
  countries: GSCRow[] | null;
  sitemaps: GSCSitemap[] | null;
  errors?: Record<string, string>;
}

/** null = the server has no webstats.json for this section (503) — the view
 *  renders setup hints instead of an error. Other failures still throw. */
async function getAnalytics<T>(url: string): Promise<T | null> {
  const res = await fetch(url);
  if (res.status === 503) return null;
  if (!res.ok) {
    throw new Error(`${url} -> ${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

export function getCloudflareAnalytics(range: string, host?: string): Promise<CFResult | null> {
  return getAnalytics<CFResult>(`/api/cloudflare/analytics${buildQuery({ range, host })}`);
}

export function getGSCAnalytics(range: string): Promise<GSCResult | null> {
  return getAnalytics<GSCResult>(`/api/gsc/analytics${buildQuery({ range })}`);
}

// One zone-hostname↔repo mapping entry from webstats.json — non-secret, so
// the endpoint answers (possibly an empty list) even with no credentials.
export interface WebSite {
  host: string;
  project: string;
}

export function getWebSites(): Promise<WebSite[]> {
  return getJSON<WebSite[]>(`/api/webstats/sites`);
}

// --- code graph (/project/codegraph) ------------------------------------------
// Read-only views over each repo's .codegraph/codegraph.db (codegraph.go).

export interface CGRepo {
  root: string;
  folder: string; // the Claude folder that resolved here
  hasIndex: boolean;
  indexedAt: string;
  files: number;
  nodes: number;
  edges: number;
  commitsSince: number; // commits landed after indexing; -1 = git couldn't say
}

export interface CGFile {
  path: string;
  language: string;
  symbols: number;
  isTest: boolean;
}

export interface CGEdge {
  from: string;
  to: string;
  kind: string; // calls | imports | references | instantiates | extends
  weight: number; // symbol edges collapsed into this file edge
}

export interface CGGraph {
  files: CGFile[];
  edges: CGEdge[];
}

export interface CGResponse {
  repos: CGRepo[];
  active: string; // root whose graph came back; "" when none has an index
  graph: CGGraph | null;
}

export interface CGSymbol {
  id: string;
  name: string;
  kind: string;
  file?: string;
  line: number;
  endLine?: number;
  signature?: string;
  via?: string; // connecting edge kind, on callers/callees
  count?: number; // >1 = that many parallel edges collapsed into the row
}

export interface CGSymbolDetail {
  node: CGSymbol;
  callers: CGSymbol[];
  callees: CGSymbol[];
}

export function getCodegraph(project?: string, repo?: string): Promise<CGResponse> {
  return getJSON<CGResponse>(`/api/codegraph${buildQuery({ project, repo })}`);
}

export function getCodegraphFile(repo: string, path: string, project?: string): Promise<CGSymbol[]> {
  return getJSON<CGSymbol[]>(`/api/codegraph/file${buildQuery({ repo, path, project })}`);
}

export function searchCodegraphSymbols(repo: string, q: string, project?: string): Promise<CGSymbol[]> {
  return getJSON<CGSymbol[]>(`/api/codegraph/symbols${buildQuery({ repo, q, project })}`);
}

export function getCodegraphSymbol(repo: string, id: string, project?: string): Promise<CGSymbolDetail> {
  return getJSON<CGSymbolDetail>(`/api/codegraph/symbols${buildQuery({ repo, id, project })}`);
}

// --- git ----------------------------------------------------------------------

export interface GitCommit {
  hash: string;
  subject: string;
  author: string;
  when: string; // ISO-8601; "" / zero when git's date was unparseable
}

export interface GitRepo {
  root: string;
  folder: string;
  isRepo: boolean;
  branch: string;
  detached: boolean; // branch holds a short hash, not a branch name
  staged: number;
  unstaged: number;
  untracked: number;
  hasUpstream: boolean;
  ahead: number;
  behind: number;
  commits: GitCommit[] | null; // null when a repo has no commits / log failed
}

export interface GitResponse {
  repos: GitRepo[];
}

export function getGit(project?: string): Promise<GitResponse> {
  return getJSON<GitResponse>(`/api/git${buildQuery({ project })}`);
}

// --- git drill-down: branches / commit diff / working-tree diff ---------------

export interface GitFileChange {
  path: string;
  add: number; // -1 = binary
  del: number;
}

export interface GitBranch {
  name: string;
  isRemote: boolean;
  isCurrent: boolean;
  subject: string;
  when: string;
  ahead: number;
  behind: number;
  merged: boolean;
}

export interface GitCommitDetail {
  hash: string;
  subject: string;
  body: string;
  author: string;
  email: string;
  when: string;
  files: GitFileChange[] | null;
  diff: string;
  truncated: boolean;
}

export interface GitDiff {
  files: GitFileChange[] | null;
  untracked: string[] | null;
  diff: string;
  truncated: boolean;
}

export function getGitBranches(repo: string, project?: string): Promise<{ branches: GitBranch[] | null }> {
  return getJSON(`/api/git/branches${buildQuery({ repo, project })}`);
}

/** Commit-list filters. Applied server-side (`git log` flags) rather than in the
    browser, because the list is paged: filtering only the loaded rows would read
    as "no results" when the match is simply further back. */
export interface GitLogFilter {
  nomerges?: boolean;
  q?: string;
  author?: string;
}

/** One page of history — the detail view's list and its "load more". `authors`
    comes back on the first page only, to fill the author picker. */
export function getGitCommits(
  repo: string,
  skip: number,
  limit: number,
  project?: string,
  filter?: GitLogFilter,
): Promise<{ commits: GitCommit[] | null; authors?: string[] | null }> {
  return getJSON(
    `/api/git/commits${buildQuery({
      repo,
      skip,
      limit,
      project,
      nomerges: filter?.nomerges ? 1 : undefined,
      q: filter?.q || undefined,
      author: filter?.author || undefined,
    })}`,
  );
}

export function getGitCommit(repo: string, hash: string, project?: string): Promise<GitCommitDetail> {
  return getJSON<GitCommitDetail>(`/api/git/commit${buildQuery({ repo, hash, project })}`);
}

export function getGitDiff(repo: string, project?: string): Promise<GitDiff> {
  return getJSON<GitDiff>(`/api/git/diff${buildQuery({ repo, project })}`);
}

// --- git drill-down: GitHub (PRs / issues / CI runs) --------------------------

export interface GitPR {
  number: number;
  title: string;
  author: string;
  state: string; // OPEN / MERGED / CLOSED
  draft: boolean;
  branch: string;
  review: string; // approved / changes_requested / review_required / ""
  checks: string; // success / failure / pending / ""
  url: string;
  createdAt: string;
  updatedAt: string;
}

export interface GitIssue {
  number: number;
  title: string;
  author: string;
  labels: string[] | null;
  url: string;
  updatedAt: string;
}

export interface GitRun {
  title: string;
  workflow: string;
  status: string; // completed / in_progress / queued
  conclusion: string; // success / failure / cancelled / ""
  branch: string;
  url: string;
  createdAt: string;
}

export function getGitPRs(repo: string, project?: string): Promise<{ supported: boolean; prs: GitPR[] | null }> {
  return getJSON(`/api/git/prs${buildQuery({ repo, project })}`);
}

export function getGitActivity(
  repo: string,
  project?: string,
): Promise<{ supported: boolean; issues: GitIssue[] | null; runs: GitRun[] | null }> {
  return getJSON(`/api/git/activity${buildQuery({ repo, project })}`);
}

// --- shared SSE transport -----------------------------------------------------
// Browsers cap HTTP/1.1 connections at ~6 per host, and an EventSource holds
// one forever — a handful of viewer tabs (each previously opening TWO streams:
// view + notifications) starved the pool and new tabs hung blank. Now exactly
// ONE tab per browser holds the real /api/events connection: tabs race for a
// Web Lock, the winner ("leader") opens the EventSource and re-publishes every
// event on a BroadcastChannel; the rest just listen. When the leader tab dies
// the lock auto-releases and the next waiter takes over. Browsers without
// those APIs fall back to one connection per tab (the old behavior).

type RawHandler = (type: string, data: unknown) => void;

const SSE_TYPES = [
  "session-updated",
  "session-attention",
  "session-stopped",
  "todos-updated",
  "drawings-updated",
  "docs-updated",
  "groups-updated",
  "projects-updated",
  "ship-recorded",
];

const rawHandlers = new Set<RawHandler>();
let transportStarted = false;
const bc = typeof BroadcastChannel !== "undefined" ? new BroadcastChannel("wyac-events") : null;

function dispatch(type: string, data: unknown): void {
  for (const h of [...rawHandlers]) h(type, data);
}

// Open the real SSE connection (leader tab only). EventSource auto-reconnects
// on its own; errors are just logged.
function openSource(): void {
  const src = new EventSource("/api/events");
  for (const t of SSE_TYPES) {
    src.addEventListener(t, (evt) => {
      let data: unknown;
      try {
        data = JSON.parse((evt as MessageEvent<string>).data);
      } catch {
        return;
      }
      dispatch(t, data); // BroadcastChannel skips the sender, so dispatch locally
      bc?.postMessage({ type: t, data });
    });
  }
  src.onerror = (err) => {
    console.warn("SSE connection error (will auto-retry)", err);
  };
}

function startTransport(): void {
  if (transportStarted) return;
  transportStarted = true;
  if (bc && typeof navigator !== "undefined" && "locks" in navigator) {
    bc.onmessage = (evt) => {
      const msg = evt.data as { type?: string; data?: unknown } | null;
      if (msg?.type) dispatch(msg.type, msg.data);
    };
    void navigator.locks.request("wyac-sse-leader", () => {
      openSource();
      return new Promise<never>(() => {}); // hold leadership until the tab dies
    });
  } else {
    openSource();
  }
}

/**
 * Subscribe to the shared /api/events stream (one real connection per browser,
 * see above). The handler gets every event type raw; returns an unsubscribe.
 */
export function subscribeRawEvents(handler: RawHandler): () => void {
  rawHandlers.add(handler);
  startTransport();
  return () => {
    rawHandlers.delete(handler);
  };
}

/**
 * Subscribe to `session-updated` events with the full SessionDetail payload.
 * Returns an unsubscribe function.
 */
export function subscribeSessionEvents(
  onUpdate: (session: SessionDetail) => void,
): () => void {
  return subscribeRawEvents((type, data) => {
    if (type !== "session-updated") return;
    const session = data as SessionDetail;
    session.agents ??= [];
    onUpdate(session);
  });
}
