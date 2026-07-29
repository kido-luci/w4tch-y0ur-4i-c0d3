import { getJSON, sendJSON } from "./client";

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
