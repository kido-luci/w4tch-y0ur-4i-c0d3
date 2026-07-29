// Thumbnail generation for the design library. The browser is the only place
// the Excalidraw renderer exists, so the grid (not the server) renders each
// changed scene to a small PNG and uploads it to the server cache — that way
// editor saves AND MCP writes both self-heal on the next grid view. The
// Excalidraw bundle is heavy: it is only reached via dynamic import, and only
// when at least one thumbnail is missing or stale.

import { getDrawingContent, putDrawingThumbnail } from "../api";

const THUMB_MAX_EDGE = 640;

// One attempt per scene version per session: a scene that cannot render
// (e.g. empty) must not be retried in a loop on every SSE re-render.
const attempted = new Set<string>();

interface SceneDoc {
  elements?: unknown[];
  appState?: Record<string, unknown>;
  files?: Record<string, unknown>;
}

/** Render + upload one drawing's thumbnail; resolves false when skipped. */
export async function generateThumbnail(id: string, updatedAt: string): Promise<boolean> {
  const key = `${id}@${updatedAt}`;
  if (attempted.has(key)) return false;
  attempted.add(key);
  const [{ exportToBlob }, scene] = await Promise.all([
    import("@excalidraw/excalidraw"),
    getDrawingContent(id),
  ]);
  const doc = (scene ?? {}) as SceneDoc;
  const elements = (doc.elements ?? []) as never[];
  if (elements.length === 0) return false; // nothing to render — keep the placeholder
  const appState: Record<string, unknown> = { ...(doc.appState ?? {}) };
  delete appState.collaborators; // session state, not document state (see excalidrawIsland)
  appState.exportBackground = true;
  const blob = await exportToBlob({
    elements,
    appState: appState as never,
    files: (doc.files ?? {}) as never,
    mimeType: "image/png",
    maxWidthOrHeight: THUMB_MAX_EDGE,
  });
  await putDrawingThumbnail(id, blob, updatedAt);
  return true;
}
