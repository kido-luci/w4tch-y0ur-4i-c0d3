import { buildQuery, getJSON, sendJSON } from "./client";

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

// --- design files -----------------------------------------------------------

/**
 * One `.fig`/`.pen` document under a repo's `design/` folder. Unlike a Drawing
 * these are plain files on disk, not library entries: the server lists them and
 * hands one to the OpenPencil desktop app, and owns nothing about their content.
 */
export interface DesignFile {
  root: string;
  folder: string;
  name: string;
  path: string;
  size: number;
  modifiedAt: string;
}

/**
 * The scope's design documents, newest first. `project` is the Claude FOLDER
 * list from getScopeParam() — the server matches it against session folders,
 * so a rail label like `memoirme-app` finds nothing while its folder
 * `memoirme copy` does. Undefined = unscoped, every repo.
 */
export async function getDesignFiles(project: string | undefined): Promise<DesignFile[]> {
  const res = await getJSON<{ files: DesignFile[] }>(`/api/design-files${buildQuery({ project })}`);
  return res.files ?? [];
}

/**
 * Opens the file in OpenPencil. `path` must come from getDesignFiles — the
 * server re-resolves the scope and rejects anything else — and `project` must
 * be the same folder param the listing used, or the re-resolve sees a
 * different repo set than the one that produced the path.
 */
export function openDesignFile(project: string | undefined, path: string): Promise<void> {
  return sendJSON<void>(`/api/design-files/open${buildQuery({ project, path })}`, "POST");
}
