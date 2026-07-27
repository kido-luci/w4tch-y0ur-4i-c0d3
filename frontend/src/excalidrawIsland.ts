// React island for the Excalidraw canvas — the only React in the app. It is
// reached exclusively via dynamic import from the design editor view, so
// React + Excalidraw live in their own lazy chunk and the main bundle stays
// vanilla TS. No JSX either (tsconfig runs erasableSyntaxOnly): the island
// renders exactly one component, so createElement is enough.

import { createElement } from "react";
import { createRoot } from "react-dom/client";
import { Excalidraw, serializeAsJSON } from "@excalidraw/excalidraw";
import type { ExcalidrawImperativeAPI } from "@excalidraw/excalidraw/types";
import "@excalidraw/excalidraw/index.css";

export type IslandTheme = "light" | "dark";

export interface ExcalidrawIsland {
  /** Current scene as a standard .excalidraw JSON string ("" until ready). */
  serialize(): string;
  /** Swap in a scene saved elsewhere; keeps the local viewport untouched. */
  replaceScene(scene: unknown): void;
  setTheme(theme: IslandTheme): void;
  unmount(): void;
}

export interface MountOptions {
  /** Parsed .excalidraw document to open (shape validated server-side only). */
  scene: unknown;
  theme: IslandTheme;
  /** Fires on every scene change; the caller debounces + serializes. */
  onDirty: () => void;
}

interface SceneDoc {
  elements?: unknown[];
  appState?: Record<string, unknown>;
  files?: Record<string, unknown>;
}

export function mountExcalidraw(host: HTMLElement, opts: MountOptions): ExcalidrawIsland {
  const doc = (opts.scene ?? {}) as SceneDoc;
  // `collaborators` deserializes from JSON as a plain object but Excalidraw
  // expects a Map — dropping it is the standard fix (it's session state, not
  // document state, and serializeAsJSON strips it again on save).
  const appState = { ...(doc.appState ?? {}) };
  delete appState.collaborators;
  const initialData = {
    elements: (doc.elements ?? []) as never[],
    appState: appState as never,
    files: (doc.files ?? {}) as never,
  };

  let api: ExcalidrawImperativeAPI | null = null;
  let theme = opts.theme;
  const root = createRoot(host);

  const render = (): void => {
    root.render(
      createElement(Excalidraw, {
        excalidrawAPI: (a: ExcalidrawImperativeAPI) => {
          api = a;
        },
        initialData,
        theme,
        onChange: opts.onDirty,
        // The app owns light/dark (nav toggle + OS tracking) — hide
        // Excalidraw's own switch so there's a single source of truth.
        UIOptions: { canvasActions: { toggleTheme: false } },
      }),
    );
  };
  render();

  return {
    serialize: () =>
      api ? serializeAsJSON(api.getSceneElements(), api.getAppState(), api.getFiles(), "local") : "",
    replaceScene: (scene) => {
      if (!api) return;
      const next = (scene ?? {}) as SceneDoc;
      // Elements + files only: keeping the local appState means the viewer's
      // viewport/zoom stays put instead of jumping to the writer's.
      api.updateScene({ elements: (next.elements ?? []) as never[] });
      const files = Object.values(next.files ?? {});
      if (files.length > 0) api.addFiles(files as never[]);
    },
    setTheme: (t) => {
      theme = t;
      render();
    },
    unmount: () => root.unmount(),
  };
}
