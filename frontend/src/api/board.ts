import { getJSON, sendJSON } from "./client";

// --- todo board -----------------------------------------------------------

// A workflow column's id. Was a three-value union until data.db v12 made the
// columns user-defined — "backlog" | "doing" | "done" still exist, they are
// just no longer the whole set, so the type is the id and the live list comes
// from getBoardStates().
export type TodoStatus = string;

// What a column means to the server. `done` is the one that matters: landing
// in a done-category column freezes the card's cost snapshot, whatever the
// column is called.
export type StateCategory = "todo" | "started" | "done";

export interface TodoState {
  id: string;
  name: string;
  category: StateCategory;
  order: number;
  wipLimit?: number; // 0 = uncapped; a signal the board renders, not a lock
  repo?: string; // "" = shared across every scope
  builtin?: boolean; // backlog/doing/done — renamable, not deletable
}

export type TodoKind = "epic" | "story" | "task" | "bug";

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
  parentId?: string; // nests under another card; the board goes two deep
  kind?: TodoKind;
  priority?: number; // 0 none … 4 urgent
  estimate?: number; // story points, not hours; 0 = unestimated
  cycleId?: string;
  rollup?: TodoRollup; // server-computed, present only on a card with children
}

// A parent card's children, counted server-side so every client agrees.
export interface TodoRollup {
  children: number;
  done: number;
  estimate: number;
  estimateDone: number;
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

export function getTodos(): Promise<Todo[]> {
  return getJSON<Todo[]>("/api/todos");
}

export function createTodo(input: {
  title: string;
  note?: string;
  repo?: string;
  status?: TodoStatus;
  kind?: TodoKind;
  parentId?: string;
  priority?: number;
  estimate?: number;
  cycleId?: string;
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
      | "parentId"
      | "kind"
      | "priority"
      | "estimate"
      | "cycleId"
    >
  >,
): Promise<Todo> {
  return sendJSON<Todo>(`/api/todos/${encodeURIComponent(id)}`, "PATCH", patch);
}

export function deleteTodo(id: string): Promise<void> {
  return sendJSON<void>(`/api/todos/${encodeURIComponent(id)}`, "DELETE");
}

// --- workflow columns -------------------------------------------------------

/** `repo` narrows to what one scope sees: the shared columns plus its own. */
export function getBoardStates(repo?: string): Promise<TodoState[]> {
  const q = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  return getJSON<TodoState[]>(`/api/board/states${q}`);
}

export function createBoardState(input: {
  name: string;
  category?: StateCategory;
  repo?: string;
  wipLimit?: number;
}): Promise<TodoState> {
  return sendJSON<TodoState>("/api/board/states", "POST", input);
}

export function patchBoardState(
  id: string,
  patch: Partial<Pick<TodoState, "name" | "category" | "order" | "wipLimit">>,
): Promise<TodoState> {
  return sendJSON<TodoState>(`/api/board/states/${encodeURIComponent(id)}`, "PATCH", patch);
}

/** Rejected with 409 while cards still sit in the column. */
export function deleteBoardState(id: string): Promise<void> {
  return sendJSON<void>(`/api/board/states/${encodeURIComponent(id)}`, "DELETE");
}

// --- cycles (sprints) -------------------------------------------------------

export interface Cycle {
  id: string;
  name: string;
  repo?: string;
  goal?: string;
  startsAt: string;
  endsAt: string;
  closedAt?: string; // absent while the cycle is open
}

/** One row of the velocity table: committed vs landed. */
export interface CycleReport {
  cycle: Cycle;
  cards: number;
  cardsDone: number;
  points: number;
  pointsDone: number;
  unestimated: number;
}

export interface BurndownPoint {
  date: string; // YYYY-MM-DD
  total: number;
  done: number;
  remaining: number;
  ideal: number;
}

export interface Burndown {
  cycleId: string;
  points: BurndownPoint[];
  cards: number;
  cardsDone: number;
  unestimated: number; // cards the chart is blind to
}

export function getCycles(repo?: string): Promise<Cycle[]> {
  const q = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  return getJSON<Cycle[]>(`/api/cycles${q}`);
}

export function createCycle(input: {
  name: string;
  repo?: string;
  goal?: string;
  startsAt: string;
  endsAt: string;
}): Promise<Cycle> {
  return sendJSON<Cycle>("/api/cycles", "POST", input);
}

export function patchCycle(
  id: string,
  patch: Partial<Pick<Cycle, "name" | "goal" | "repo" | "startsAt" | "endsAt">> & {
    closed?: boolean;
  },
): Promise<Cycle> {
  return sendJSON<Cycle>(`/api/cycles/${encodeURIComponent(id)}`, "PATCH", patch);
}

export function deleteCycle(id: string): Promise<void> {
  return sendJSON<void>(`/api/cycles/${encodeURIComponent(id)}`, "DELETE");
}

export function getBurndown(id: string): Promise<Burndown> {
  return getJSON<Burndown>(`/api/cycles/${encodeURIComponent(id)}/burndown`);
}

export function getVelocity(repo?: string): Promise<CycleReport[]> {
  const q = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  return getJSON<CycleReport[]>(`/api/cycles/velocity${q}`);
}

// --- board history ----------------------------------------------------------

export interface TodoEvent {
  id: number;
  todoId: string;
  ts: string;
  kind: "created" | "status" | "estimate" | "cycle" | "parent" | "priority";
  from?: string;
  to?: string;
}

export function getTodoEvents(id: string): Promise<TodoEvent[]> {
  return getJSON<TodoEvent[]>(`/api/todos/${encodeURIComponent(id)}/events`);
}

export function getBoardEvents(limit?: number): Promise<TodoEvent[]> {
  const q = limit ? `?limit=${limit}` : "";
  return getJSON<TodoEvent[]>(`/api/board/events${q}`);
}

// --- saved views ------------------------------------------------------------

export type ViewKind = "board" | "table" | "timeline";

/** A saved filter plus the shape it draws. `query` is opaque to the server. */
export interface BoardView {
  id: string;
  name: string;
  repo?: string;
  kind: ViewKind;
  query: BoardQuery;
  order: number;
}

/** The filter vocabulary the board understands. Every field is optional and
 *  an absent one filters nothing. */
export interface BoardQuery {
  text?: string;
  kinds?: TodoKind[];
  statuses?: string[];
  labels?: string[];
  cycleId?: string;
  minPriority?: number;
  unestimatedOnly?: boolean;
}

export function getBoardViews(repo?: string): Promise<BoardView[]> {
  const q = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  return getJSON<BoardView[]>(`/api/board/views${q}`);
}

export function createBoardView(input: {
  name: string;
  repo?: string;
  kind?: ViewKind;
  query?: BoardQuery;
}): Promise<BoardView> {
  return sendJSON<BoardView>("/api/board/views", "POST", input);
}

export function patchBoardView(
  id: string,
  patch: Partial<Pick<BoardView, "name" | "kind" | "query" | "order">>,
): Promise<BoardView> {
  return sendJSON<BoardView>(`/api/board/views/${encodeURIComponent(id)}`, "PATCH", patch);
}

export function deleteBoardView(id: string): Promise<void> {
  return sendJSON<void>(`/api/board/views/${encodeURIComponent(id)}`, "DELETE");
}
