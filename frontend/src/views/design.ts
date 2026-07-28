// View 4 — design library (routes `/project/design` and `/project/design/<id>`): local
// Excalidraw wireframes. The library is a plain grid over /api/drawings
// metadata (SSE-synced like the board); opening a drawing lazy-loads the
// React/Excalidraw island (see excalidrawIsland.ts) so the rest of the app
// never pays for it. Saves are debounced last-writer-wins PUTs of the whole
// .excalidraw document.

import {
  createDrawing,
  deleteDrawing,
  DrawingConflictError,
  drawingThumbnailURL,
  duplicateDrawing,
  getDrawingContent,
  getDrawings,
  getProjects,
  getTodos,
  hasFreshThumbnail,
  isPublished,
  isPublishFresh,
  moveDrawing,
  publishDrawing,
  putDrawingContent,
  renameDrawing,
  setDrawingTopics,
  subscribeRawEvents,
} from "../api";
import type { Drawing, Todo } from "../api";
import { escapeHtml, formatRelativeTime, truncate } from "../format";
import { getScope, getScopeSet, navigate } from "../scope";
import { getTheme } from "../theme";
import type { ExcalidrawIsland } from "../excalidrawIsland";

const SAVE_DEBOUNCE_MS = 800;

/** Renders the drawing library into `container`; returns a cleanup callback. */
export function renderDesignView(container: HTMLElement): () => void {
  let drawings: Drawing[] = [];
  let projects: string[] = []; // known project names, offered in the group datalist
  let renamingId: string | null = null; // grid rebuilds pause while a rename input is open
  let movingId: string | null = null; // …and while a move (group) input is open
  let taggingId: string | null = null; // …and while a topics input is open
  // The nav's global project scope; a change re-renders the whole view, so
  // reading it once per mount is enough. The set filters the grid (a group
  // scope covers its name plus its members), the label feeds "+ new".
  const scope = getScope();
  const scopeSet = getScopeSet();

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          <input class="board-search" id="dw-name" placeholder="new drawing…" autocomplete="off">
          <button type="button" class="nav-link" id="dw-create">+ new</button>
        </div>
      </header>
      <section class="design-sections" id="dw-grid">
        <div class="empty-state">loading…</div>
      </section>
      <datalist id="design-groups"></datalist>
    </div>
  `;

  const gridEl = container.querySelector<HTMLElement>("#dw-grid")!;
  const datalistEl = container.querySelector<HTMLDataListElement>("#design-groups")!;
  const nameEl = container.querySelector<HTMLInputElement>("#dw-name")!;
  const createBtn = container.querySelector<HTMLButtonElement>("#dw-create")!;

  async function create(): Promise<void> {
    const name = nameEl.value.trim();
    if (!name) {
      nameEl.focus();
      return;
    }
    try {
      // A scoped library creates into the scope (the board composer's call);
      // unscoped creation stays ungrouped.
      const d = await createDrawing(name, scope);
      nameEl.value = "";
      navigate(`/project/design/${encodeURIComponent(d.id)}`);
    } catch (err) {
      console.error("create drawing failed", err);
    }
  }
  createBtn.addEventListener("click", () => void create());
  nameEl.addEventListener("keydown", (e) => {
    if (e.key === "Enter") void create();
  });

  const groupOf = (d: Drawing): string => d.group ?? "";

  /** Distinct non-empty group labels, case-insensitively sorted. */
  function groupNames(): string[] {
    const set = new Set<string>();
    for (const d of drawings) {
      const g = groupOf(d);
      if (g) set.add(g);
    }
    return [...set].sort((a, b) => a.localeCompare(b));
  }

  /** The library through the rail's scope (server order is most-recently-
   *  updated first, so the grid stays recency-ordered). Scoped, only matching
   *  groups show — ungrouped drawings live under "all projects" (strict since
   *  v0.63; board and docs behave the same). */
  function visible(): Drawing[] {
    return drawings.filter((d) => !scopeSet || scopeSet.has(groupOf(d)));
  }

  // Datalist for the move/create inputs: known projects ∪ groups already in use.
  function renderDatalist(): void {
    const set = new Set<string>(projects);
    for (const g of groupNames()) set.add(g);
    datalistEl.innerHTML = [...set]
      .sort((a, b) => a.localeCompare(b))
      .map((g) => `<option value="${escapeHtml(g)}"></option>`)
      .join("");
  }

  const cardHTML = (d: Drawing): string => `
        <article class="design-card" data-id="${escapeHtml(d.id)}">
          <div class="design-card-thumb">${
            hasFreshThumbnail(d)
              ? `<img src="${drawingThumbnailURL(d)}" alt="" loading="lazy">`
              : `<span class="design-thumb-empty">no preview yet</span>`
          }</div>
          <div class="design-card-name">${escapeHtml(d.name)}</div>
          <div class="design-card-meta">updated ${formatRelativeTime(d.updatedAt)}${
            isPublishFresh(d) ? " · shared ✓" : isPublished(d) ? " · shared (stale)" : ""
          }</div>
          <div class="design-card-actions">
            <button type="button" class="design-act" data-act="rename">rename</button>
            <button type="button" class="design-act" data-act="move">move</button>
            <button type="button" class="design-act" data-act="topics">topics</button>
            <button type="button" class="design-act" data-act="copy">copy</button>
            <button type="button" class="design-act" data-act="share">share</button>
            <button type="button" class="design-act" data-act="delete">✕</button>
          </div>
        </article>`;

  function render(): void {
    if (renamingId !== null || movingId !== null || taggingId !== null) return;
    renderDatalist();
    if (drawings.length === 0) {
      gridEl.innerHTML = `<div class="empty-state">no drawings yet — name one above and hit enter.</div>`;
      return;
    }
    const shown = visible();
    if (shown.length === 0) {
      gridEl.innerHTML = `<div class="empty-state">no drawings in this scope yet.</div>`;
      return;
    }
    // One section per topic tag (A→Z), a drawing under each of its tags —
    // topics are many-to-many, so a card appearing twice is the model, not a
    // bug. Untagged drawings close the list; a library with no tags at all
    // keeps the flat grid (no headings to say nothing with). Recency order
    // inside a section is inherited from the server's List().
    const topics = [...new Set(shown.flatMap((d) => d.topics))].sort((a, b) => a.localeCompare(b));
    if (topics.length === 0) {
      gridEl.innerHTML = `<div class="design-grid">${shown.map(cardHTML).join("")}</div>`;
      refreshThumbnails();
      return;
    }
    const sections = topics.map((t) => ({
      head: t,
      cards: shown.filter((d) => d.topics.includes(t)),
    }));
    const untagged = shown.filter((d) => d.topics.length === 0);
    if (untagged.length > 0) sections.push({ head: "untagged", cards: untagged });
    gridEl.innerHTML = sections
      .map(
        (s) => `
        <section class="design-topic">
          <h2 class="design-topic-head">${escapeHtml(s.head)}<span class="design-topic-count">${s.cards.length}</span></h2>
          <div class="design-grid">${s.cards.map(cardHTML).join("")}</div>
        </section>`,
      )
      .join("");
    refreshThumbnails();
  }

  // Render missing/stale thumbnails one at a time; each success is broadcast
  // back as drawings-updated, which re-renders the grid with the fresh image.
  // generateThumbnail() itself skips versions it has already attempted, so
  // the render → SSE → render cycle cannot loop.
  function refreshThumbnails(): void {
    const stale = drawings.filter((d) => !hasFreshThumbnail(d));
    if (stale.length === 0) return;
    void (async () => {
      const { generateThumbnail } = await import("../thumbs");
      for (const d of stale) {
        try {
          await generateThumbnail(d.id, d.updatedAt);
        } catch (err) {
          console.error("thumbnail render failed", err);
        }
      }
    })();
  }

  function startRename(card: HTMLElement, d: Drawing): void {
    renamingId = d.id;
    const nameNode = card.querySelector<HTMLElement>(".design-card-name")!;
    nameNode.innerHTML = `<input class="design-rename" value="${escapeHtml(d.name)}">`;
    const input = nameNode.querySelector<HTMLInputElement>("input")!;
    input.focus();
    input.select();
    const finish = async (commit: boolean): Promise<void> => {
      if (renamingId !== d.id) return;
      renamingId = null;
      const name = input.value.trim();
      if (commit && name && name !== d.name) {
        try {
          await renameDrawing(d.id, name);
          return; // SSE broadcast re-renders with the fresh list
        } catch (err) {
          console.error("rename failed", err);
        }
      }
      render();
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") void finish(true);
      if (e.key === "Escape") void finish(false);
    });
    input.addEventListener("blur", () => void finish(true));
  }

  // Inline move: swap the card's meta line for a group input (with the
  // project/group datalist). Commit reassigns via PATCH; SSE re-renders.
  function startMove(card: HTMLElement, d: Drawing): void {
    movingId = d.id;
    const metaNode = card.querySelector<HTMLElement>(".design-card-meta")!;
    const current = groupOf(d);
    metaNode.innerHTML = `<input class="design-rename" list="design-groups" value="${escapeHtml(
      current,
    )}" placeholder="project or custom tab…">`;
    const input = metaNode.querySelector<HTMLInputElement>("input")!;
    input.focus();
    input.select();
    const finish = async (commit: boolean): Promise<void> => {
      if (movingId !== d.id) return;
      movingId = null;
      const g = input.value.trim();
      if (commit && g !== current) {
        try {
          await moveDrawing(d.id, g);
          return; // SSE broadcast re-renders with the fresh group
        } catch (err) {
          console.error("move failed", err);
        }
      }
      render();
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") void finish(true);
      if (e.key === "Escape") void finish(false);
    });
    input.addEventListener("blur", () => void finish(true));
  }

  // Inline topics edit: swap the card's meta line for a comma-separated tag
  // input (the full new set — clearing it untags). Commit replaces via PATCH;
  // SSE re-renders, resectioning the grid.
  function startTopics(card: HTMLElement, d: Drawing): void {
    taggingId = d.id;
    const metaNode = card.querySelector<HTMLElement>(".design-card-meta")!;
    const current = d.topics.join(", ");
    metaNode.innerHTML = `<input class="design-rename" value="${escapeHtml(
      current,
    )}" placeholder="topics, comma-separated…">`;
    const input = metaNode.querySelector<HTMLInputElement>("input")!;
    input.focus();
    input.select();
    const finish = async (commit: boolean): Promise<void> => {
      if (taggingId !== d.id) return;
      taggingId = null;
      const topics = input.value
        .split(",")
        .map((t) => t.trim())
        .filter(Boolean);
      if (commit && input.value.trim() !== current.trim()) {
        try {
          await setDrawingTopics(d.id, topics);
          return; // SSE broadcast re-renders with the fresh tags
        } catch (err) {
          console.error("set topics failed", err);
        }
      }
      render();
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") void finish(true);
      if (e.key === "Escape") void finish(false);
    });
    input.addEventListener("blur", () => void finish(true));
  }

  gridEl.addEventListener("click", (e) => {
    const target = e.target as HTMLElement;
    const card = target.closest<HTMLElement>(".design-card");
    if (!card) return;
    const d = drawings.find((x) => x.id === card.dataset.id);
    if (!d) return;
    const act = target.closest<HTMLElement>(".design-act")?.dataset.act;
    if (act === "rename") {
      startRename(card, d);
      return;
    }
    if (act === "move") {
      startMove(card, d);
      return;
    }
    if (act === "topics") {
      startTopics(card, d);
      return;
    }
    if (act === "copy") {
      void duplicateDrawing(d.id).catch(console.error); // SSE re-renders with the copy
      return;
    }
    if (act === "share") {
      const btn = target.closest<HTMLButtonElement>(".design-act")!;
      btn.disabled = true;
      void publishDrawing(d.id)
        .then(async (url) => {
          // SSE re-renders the card with "shared ✓"; hand the reviewer link
          // to the clipboard so sharing is paste-ready.
          await navigator.clipboard.writeText(url).catch(() => {});
          alert(`review link copied:\n${url}`);
        })
        .catch((err: unknown) => alert(`share failed: ${err instanceof Error ? err.message : err}`))
        .finally(() => {
          btn.disabled = false;
        });
      return;
    }
    if (act === "delete") {
      if (confirm(`delete "${d.name}"?`)) void deleteDrawing(d.id).catch(console.error);
      return;
    }
    if (renamingId !== d.id && movingId !== d.id && taggingId !== d.id) {
      navigate(`/project/design/${encodeURIComponent(d.id)}`);
    }
  });

  getDrawings()
    .then((list) => {
      drawings = list;
      render();
    })
    .catch(() => {
      gridEl.innerHTML = `<div class="empty-state">failed to load drawings.</div>`;
    });

  // Project names feed the move/create group datalist; a failure just leaves
  // the datalist to the groups already in use.
  getProjects()
    .then((list) => {
      projects = list;
      renderDatalist();
    })
    .catch(() => {});

  const unsubscribe = subscribeRawEvents((type, data) => {
    if (type !== "drawings-updated") return;
    drawings = data as Drawing[];
    render();
  });

  return unsubscribe;
}

/** Renders one drawing's editor into `container`; returns a cleanup callback. */
export function renderDesignEditorView(container: HTMLElement, id: string): () => void {
  container.innerHTML = `
    <div class="page design-editor-page">
      <div class="design-editor-head">
        <a class="nav-link" href="/project/design">← design</a>
        <span class="design-editor-name" id="dw-title"></span>
        <span class="design-editor-cards" id="dw-cards"></span>
        <span class="design-save" id="dw-save"></span>
      </div>
      <div class="design-editor-host" id="dw-host">
        <div class="empty-state">loading editor…</div>
      </div>
    </div>
  `;

  const hostEl = container.querySelector<HTMLElement>("#dw-host")!;
  const titleEl = container.querySelector<HTMLElement>("#dw-title")!;
  const saveEl = container.querySelector<HTMLElement>("#dw-save")!;

  let disposed = false;
  let island: ExcalidrawIsland | null = null;
  let saveTimer: number | undefined;
  let lastSaved = ""; // serialized doc as of the last successful PUT
  let baseUpdatedAt = ""; // server updatedAt this canvas is based on
  let conflicted = false; // a save elsewhere collided with local edits
  let saving = false; // a PUT is in flight (its SSE echo must not alarm us)
  let todos: Todo[] = []; // for the head's card backlink chips

  /** Board cards linking to this drawing, as chips beside the title. */
  function syncCardLinks(): void {
    const slot = container.querySelector<HTMLElement>("#dw-cards");
    if (!slot || !slot.isConnected) return;
    slot.innerHTML = todos
      .filter((t) => t.linkedDrawingIds?.includes(id))
      .map(
        (t) =>
          `<a class="card-link" href="/project/board/${encodeURIComponent(t.id)}" title="open on the board">#${t.seq} ${escapeHtml(truncate(t.title, 24))}</a>`,
      )
      .join(" ");
  }

  const setStatus = (s: "saved" | "saving" | "failed" | "conflict" | ""): void => {
    saveEl.classList.toggle("design-save-failed", s === "failed" || s === "conflict");
    if (s === "conflict") {
      saveEl.innerHTML = `changed elsewhere — <button type="button" class="design-act" data-conflict="theirs">load theirs</button> <button type="button" class="design-act" data-conflict="mine">keep mine</button>`;
      return;
    }
    saveEl.textContent = s === "" ? "" : s === "saving" ? "saving…" : s === "failed" ? "save failed — retrying on next change" : "saved";
  };

  async function save(): Promise<void> {
    if (!island || conflicted) return;
    if (saving) {
      scheduleSave(); // settle the in-flight PUT first, then retry
      return;
    }
    const json = island.serialize();
    if (json === "" || json === lastSaved) return;
    setStatus("saving");
    saving = true;
    try {
      const meta = await putDrawingContent(id, json, baseUpdatedAt);
      lastSaved = json;
      baseUpdatedAt = meta.updatedAt;
      if (!disposed) setStatus("saved");
    } catch (err) {
      if (err instanceof DrawingConflictError) {
        conflicted = true;
        if (!disposed) setStatus("conflict");
      } else {
        console.error("save failed", err);
        if (!disposed) setStatus("failed");
      }
    } finally {
      saving = false;
    }
  }

  const scheduleSave = (): void => {
    window.clearTimeout(saveTimer);
    saveTimer = window.setTimeout(() => void save(), SAVE_DEBOUNCE_MS);
  };

  // Adopt the version saved elsewhere, dropping any local edits.
  async function loadTheirs(): Promise<void> {
    try {
      const [scene, list] = await Promise.all([getDrawingContent(id), getDrawings()]);
      if (disposed || !island) return;
      island.replaceScene(scene);
      lastSaved = island.serialize();
      baseUpdatedAt = list.find((d) => d.id === id)?.updatedAt ?? baseUpdatedAt;
      conflicted = false;
      setStatus("saved");
    } catch (err) {
      console.error("reload failed", err);
      if (!disposed && conflicted) setStatus("conflict");
    }
  }

  // Overwrite the other save with the local canvas (deliberate).
  async function keepMine(): Promise<void> {
    conflicted = false;
    baseUpdatedAt = ""; // this one save goes through unconditionally
    lastSaved = ""; // and even a canvas matching the last save re-PUTs
    await save();
  }

  saveEl.addEventListener("click", (e) => {
    const act = (e.target as HTMLElement).closest<HTMLElement>("[data-conflict]")?.dataset.conflict;
    if (act === "theirs") void loadTheirs();
    else if (act === "mine") void keepMine();
  });

  getTodos()
    .then((list) => {
      if (disposed) return;
      todos = list;
      syncCardLinks();
    })
    .catch(() => {
      /* backlink chips are an extra — the editor stands without them */
    });

  void (async () => {
    try {
      const [scene, list, mod] = await Promise.all([
        getDrawingContent(id),
        getDrawings(),
        import("../excalidrawIsland"),
      ]);
      if (disposed) return;
      const meta = list.find((d) => d.id === id);
      titleEl.textContent = meta?.name ?? "(unknown drawing)";
      baseUpdatedAt = meta?.updatedAt ?? "";
      document.title = `${meta?.name ?? "drawing"} — W4tch y0ur 4I c0d3`;
      hostEl.innerHTML = "";
      island = mod.mountExcalidraw(hostEl, {
        scene,
        theme: getTheme(),
        onDirty: scheduleSave,
      });
      lastSaved = island.serialize();
      setStatus("saved");
    } catch (err) {
      console.error("editor load failed", err);
      if (!disposed) {
        hostEl.innerHTML = `<div class="empty-state">failed to load this drawing.</div>`;
      }
    }
  })();

  // The app owns the theme (nav toggle + OS tracking); mirror data-theme
  // flips into the island so the canvas follows without a reload.
  const themeObserver = new MutationObserver(() => island?.setTheme(getTheme()));
  themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });

  // Follow saves that happen elsewhere (another tab, an MCP client): a clean
  // canvas reloads live; local edits switch to the pick-a-side conflict bar.
  const unsubscribe = subscribeRawEvents((type, data) => {
    if (type !== "drawings-updated") return;
    const meta = (data as Drawing[]).find((d) => d.id === id);
    if (!meta) {
      // Deleted elsewhere — nothing left to save to, so leave the editor rather
      // than letting the next save 404 into an endless "retrying…" (matches the
      // docs wiki, which navigates away on a foreign delete).
      navigate("/project/design");
      return;
    }
    titleEl.textContent = meta.name;
    document.title = `${meta.name} — W4tch y0ur 4I c0d3`;
    // While our own PUT is in flight this event is (usually) its echo — skip;
    // a genuinely-foreign write racing us fails that conditional PUT anyway.
    if (!island || saving || conflicted || meta.updatedAt === baseUpdatedAt) return;
    if (island.serialize() === lastSaved) {
      void loadTheirs();
    } else {
      conflicted = true;
      setStatus("conflict");
    }
  });

  return () => {
    disposed = true;
    unsubscribe();
    themeObserver.disconnect();
    window.clearTimeout(saveTimer);
    void save(); // flush any pending edits (no-op when conflicted — deliberate)
    island?.unmount();
    island = null;
    document.title = "W4tch y0ur 4I c0d3";
  };
}
