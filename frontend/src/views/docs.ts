// View 8 — docs wiki (routes `/project/docs` and `/project/docs/<id>`): FLAT pages, one
// project each — no nesting. The page index lives INSIDE this view: a left
// column lists the pages in the current scope (the rail picks the project),
// and the body pane to its right is a rendered read mode + a split
// source/preview edit mode, whose autosave lifecycle mirrors the design
// library editor's exactly. Docs metadata still carries parentId server-side;
// the UI ignores it — a page's own `group` is its whole address.

import {
  createDoc,
  deleteDoc,
  DocConflictError,
  getDoc,
  getDocs,
  getProjects,
  getTodos,
  patchDoc,
  putDocBody,
  subscribeRawEvents,
} from "../api";
import type { Doc, Todo } from "../api";
import { escapeHtml, truncate } from "../domain/format";
import { announce } from "../app/live";
import { renderMarkdown } from "../domain/markdown";
import { getKnownGroupNames, getScope, getScopeSet, navigate } from "../scope";

const SAVE_DEBOUNCE_MS = 800;
const PREVIEW_DEBOUNCE_MS = 150;

/** Renders the docs wiki into `container`; returns a cleanup callback. `id`
 *  is the currently-open page, undefined on the bare `/project/docs` route. */
export function renderDocsView(container: HTMLElement, initialId?: string): () => void {
  // The open page id. Mutable because autoOpenFirst() adopts the scope's first
  // page in place: its replace-navigate fires no re-render (see the note there),
  // so this instance updates `id` itself rather than waiting for a fresh mount.
  let id = initialId;
  let docs: Doc[] = [];
  let docsLoaded = false;
  let docsLoadFailed = false;
  let bodyLoaded = false; // getDoc(id) has resolved at least once (id-only)
  let bodyLoadFailed = false;
  let editing = false;
  let disposed = false;
  let projects: string[] = []; // known project names, offered in the group datalist
  let todos: Todo[] = []; // for the open page's card backlink chips

  // Body autosave lifecycle — mirrors design.ts's editor exactly, adapted to
  // read from a textarea that only exists while `editing` (the tree/read pane
  // share this view, so unlike the design editor, the DOM node it reads comes
  // and goes; the guard below plays the role `island` plays there).
  let textareaEl: HTMLTextAreaElement | null = null;
  let saveTimer: number | undefined;
  let previewTimer: number | undefined;
  let lastSaved = ""; // body as of the last successful PUT — also read mode's source
  let baseUpdatedAt = ""; // server updatedAt this edit is based on
  let conflicted = false; // a save elsewhere collided with local edits
  let saving = false; // a PUT is in flight (its SSE echo must not alarm us)

  container.innerHTML = `
    <div class="page">
      <header class="topbar">
        <div class="topbar-controls">
          <input class="board-search" id="doc-new" placeholder="new page…" autocomplete="off">
          <button type="button" class="nav-link" id="doc-new-btn">+ new</button>
        </div>
      </header>
      <div class="doc-layout">
        <nav class="doc-index" id="doc-index"></nav>
        <div class="doc-main" id="doc-main"><div class="empty-state">loading…</div></div>
      </div>
      <datalist id="doc-groups"></datalist>
    </div>
  `;

  const layoutEl = container.querySelector<HTMLElement>(".doc-layout")!;
  const indexEl = container.querySelector<HTMLElement>("#doc-index")!;
  const mainEl = container.querySelector<HTMLElement>("#doc-main")!;
  const newInput = container.querySelector<HTMLInputElement>("#doc-new")!;
  const newBtn = container.querySelector<HTMLButtonElement>("#doc-new-btn")!;
  const groupsDatalistEl = container.querySelector<HTMLDataListElement>("#doc-groups")!;

  // Datalist for the edit bar's group input: known projects ∪ project-group
  // names ∪ groups already in use (same recipe as the design library's move
  // input) — a root can sit under a group's own label too.
  function syncGroupOptions(): void {
    const set = new Set(projects);
    for (const g of getKnownGroupNames()) set.add(g);
    for (const d of docs) if (d.group) set.add(d.group);
    groupsDatalistEl.innerHTML = [...set]
      .sort((a, b) => a.localeCompare(b))
      .map((g) => `<option value="${escapeHtml(g)}"></option>`)
      .join("");
  }
  getProjects()
    .then((list) => {
      projects = list;
      syncGroupOptions();
    })
    .catch(() => {
      /* datalist is a convenience; free-text group input still works */
    });

  getTodos()
    .then((list) => {
      if (disposed) return;
      todos = list;
      syncCardLinks();
    })
    .catch(() => {
      /* backlink chips are an extra — the page stands without them */
    });

  async function createTopLevel(): Promise<void> {
    const title = newInput.value.trim();
    if (!title) {
      newInput.focus();
      return;
    }
    try {
      const d = await createDoc({ title });
      // A scoped wiki creates into the scope — otherwise the new page lives
      // under "all projects" until the edit bar assigns it a project.
      const scope = getScope();
      if (scope) await patchDoc(d.id, { group: scope });
      newInput.value = "";
      navigate(`/project/docs/${encodeURIComponent(d.id)}`);
    } catch (err) {
      console.error("create doc failed", err);
    }
  }
  newBtn.addEventListener("click", () => void createTopLevel());
  newInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") void createTopLevel();
  });

  // --- page index (left column) --------------------------------------------

  // A page's own `group` is its whole address; a legacy empty-group page
  // resolves through its stored parent chain (mirrors the rail's old logic).
  function effectiveGroup(d: Doc, byId: Map<string, Doc>): string {
    let cur: Doc | undefined = d;
    while (cur) {
      if (cur.group) return cur.group;
      cur = cur.parentId ? byId.get(cur.parentId) : undefined;
    }
    return "";
  }

  /** The flat, scope-filtered page list, in index order. The rail sets the
   *  scope; a scope change re-mounts this whole view, so the filter is re-read
   *  for free — only docs-updated re-runs it within a mounted view. */
  function scopedPages(): Doc[] {
    const set = getScopeSet();
    const byId = new Map(docs.map((d) => [d.id, d]));
    return docs
      .filter((d) => !set || set.has(effectiveGroup(d, byId)))
      .sort((a, b) => a.order - b.order || a.title.localeCompare(b.title));
  }

  /** Landing on `/project/docs` with no page open shows the scope's first page rather
   *  than an empty detail. replace() (not a push) so Back doesn't bounce onto
   *  `/project/docs` and redirect here again. An explicit page id — a deep link or a
   *  board backlink — is always honored, even out of the current scope, so this
   *  only fires when there's no id. Returns whether it redirected. */
  function autoOpenFirst(): boolean {
    if (id) return false;
    const first = scopedPages()[0];
    if (!first) return false;
    // replace() (not push) so Back doesn't bounce onto the bare route and
    // redirect here again — but a replace fires no popstate, so render() never
    // re-runs. Open the page in place instead: adopt its id, fetch its body,
    // and render now, rather than returning on the promise of a re-render that
    // never arrives (which left the view stuck on "loading…").
    navigate(`/project/docs/${encodeURIComponent(first.id)}`, true);
    id = first.id;
    loadDoc(id);
    renderIndex();
    renderMain();
    return true;
  }

  function renderIndex(): void {
    if (!docsLoaded) return;
    const pages = scopedPages();
    if (pages.length === 0) {
      indexEl.innerHTML = `<div class="doc-index-empty">no pages in scope</div>`;
      return;
    }
    indexEl.innerHTML = pages
      .map(
        (d) =>
          `<button type="button" class="doc-index-item${
            d.id === id ? " doc-index-item--active" : ""
          }" data-doc="${escapeHtml(d.id)}" title="${escapeHtml(d.title)}">${escapeHtml(d.title)}</button>`,
      )
      .join("");
  }

  indexEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>(".doc-index-item");
    const docId = btn?.dataset["doc"];
    if (docId) navigate(`/project/docs/${encodeURIComponent(docId)}`);
  });

  // --- main pane: shared helpers ------------------------------------------

  /** Flat address: `project / title` (no page nesting). */
  function breadcrumbsHtml(meta: Doc): string {
    const project = meta.group
      ? `<span class="doc-crumb">${escapeHtml(meta.group)}</span>`
      : `<span class="doc-crumb">no project</span>`;
    return `${project}<span class="doc-crumb-sep">·</span><span class="doc-crumb-current">${escapeHtml(meta.title)}</span>`;
  }

  /** Board cards linking to the open page, as chips right after the
   *  breadcrumbs. A no-op until read mode is showing (the slot only exists
   *  there) and getTodos() has resolved at least once. */
  function syncCardLinks(): void {
    const slot = mainEl.querySelector<HTMLElement>("#doc-card-links");
    if (!slot) return;
    slot.innerHTML = todos
      .filter((t) => id && t.linkedDocIds?.includes(id))
      .map(
        (t) =>
          ` <a class="card-link" href="/project/board/${encodeURIComponent(t.id)}" title="open on the board">#${t.seq} ${escapeHtml(truncate(t.title, 24))}</a>`,
      )
      .join("");
  }

  /** Scroll spy for the toc — the observer must be torn down before each
   *  rebuild or every re-render leaks one and stale callbacks fight over the
   *  active class. */
  let tocSpy: IntersectionObserver | null = null;

  function clearToc(): void {
    tocSpy?.disconnect();
    tocSpy = null;
    layoutEl.classList.remove("doc-layout--toc");
    layoutEl.querySelector("#doc-toc")?.remove();
  }

  /** Builds the "on this page" jump list from the rendered body's headings.
   *  Anchors are plain buttons (not real `href="#..."`) since this app is
   *  hash-routed — a real anchor would change the route. */
  function renderToc(): void {
    const bodyEl = mainEl.querySelector<HTMLElement>("#doc-body");
    const headings = bodyEl ? [...bodyEl.querySelectorAll<HTMLElement>("h1, h2, h3")] : [];
    if (headings.length === 0) {
      clearToc();
      return;
    }
    const seen = new Set<string>();
    const items = headings.map((h) => {
      const base =
        (h.textContent ?? "")
          .trim()
          .toLowerCase()
          .replace(/[^a-z0-9]+/g, "-")
          .replace(/^-+|-+$/g, "") || "section";
      let slug = base;
      let n = 2;
      while (seen.has(slug)) slug = `${base}-${n++}`;
      seen.add(slug);
      h.id = slug;
      return { id: slug, text: h.textContent ?? "", level: h.tagName.toLowerCase() };
    });
    let tocEl = layoutEl.querySelector<HTMLElement>("#doc-toc");
    if (!tocEl) {
      tocEl = document.createElement("aside");
      tocEl.className = "doc-toc";
      tocEl.id = "doc-toc";
      layoutEl.append(tocEl);
    }
    layoutEl.classList.add("doc-layout--toc");
    tocEl.innerHTML =
      `<div class="doc-toc-label">on this page</div>` +
      items
        .map(
          (it) =>
            `<button type="button" class="doc-toc-link doc-toc-link--${it.level}" data-target="${escapeHtml(it.id)}">${escapeHtml(it.text)}</button>`,
        )
        .join("");
    tocEl.querySelectorAll<HTMLButtonElement>(".doc-toc-link").forEach((btn) => {
      btn.addEventListener("click", () => {
        document.getElementById(btn.dataset["target"]!)?.scrollIntoView({ behavior: "smooth", block: "start" });
      });
    });
    // The section under the reading line speaks purple in the toc.
    const links = [...tocEl.querySelectorAll<HTMLButtonElement>(".doc-toc-link")];
    const mark = (id: string): void => {
      for (const b of links) b.classList.toggle("doc-toc-link--active", b.dataset["target"] === id);
    };
    if (items[0]) mark(items[0].id);
    tocSpy?.disconnect();
    tocSpy = new IntersectionObserver(
      (entries) => {
        for (const e of entries) if (e.isIntersecting) mark(e.target.id);
      },
      // A band near the top of the viewport: a heading crossing it is the one
      // being read.
      { rootMargin: "0% 0% -75% 0%" },
    );
    for (const it of items) {
      const el = document.getElementById(it.id);
      if (el) tocSpy.observe(el);
    }
  }

  // --- main pane: read mode ------------------------------------------------

  function readModeHtml(meta: Doc): string {
    const bodyHtml = lastSaved.trim()
      ? `<div class="md" id="doc-body">${renderMarkdown(lastSaved)}</div>`
      : `<div class="empty-state">This page is empty — hit edit to write it.</div>`;
    return `
      <div class="doc-breadcrumbs">${breadcrumbsHtml(meta)}<span id="doc-card-links"></span></div>
      <div class="doc-head-row">
        <h1 class="doc-title">${escapeHtml(meta.title)}</h1>
        <div class="doc-actions">
          <button type="button" class="nav-link" id="doc-edit">edit</button>
          <button type="button" class="design-act" data-act="delete">delete</button>
        </div>
      </div>
      ${bodyHtml}
    `;
  }

  function wireReadMode(meta: Doc): void {
    mainEl.querySelector<HTMLButtonElement>("#doc-edit")!.addEventListener("click", () => {
      editing = true;
      renderMain();
    });
    mainEl.querySelector<HTMLButtonElement>("[data-act='delete']")!.addEventListener("click", () => {
      if (!confirm(`delete "${meta.title}"?`)) return;
      deleteDoc(meta.id)
        .then(() => {
          navigate("/project/docs");
        })
        .catch((err: unknown) => console.error("delete doc failed", err));
    });
  }

  // --- main pane: edit mode + body autosave lifecycle ----------------------

  function setStatus(s: "saved" | "saving" | "failed" | "conflict" | ""): void {
    const saveEl = mainEl.querySelector<HTMLElement>("#doc-save");
    if (!saveEl) return; // read mode has nowhere to show this
    saveEl.classList.toggle("doc-save-failed", s === "failed" || s === "conflict");
    if (s === "conflict") {
      saveEl.innerHTML = `changed elsewhere — <button type="button" class="design-act" data-conflict="theirs">load theirs</button> <button type="button" class="design-act" data-conflict="mine">keep mine</button>`;
      return;
    }
    saveEl.textContent = s === "" ? "" : s === "saving" ? "saving…" : s === "failed" ? "save failed — retrying on next change" : "saved";
  }

  async function save(): Promise<void> {
    if (!textareaEl || conflicted) return;
    if (saving) {
      scheduleSave(); // settle the in-flight PUT first, then retry
      return;
    }
    const body = textareaEl.value;
    if (body === lastSaved) return;
    setStatus("saving");
    saving = true;
    try {
      const meta = await putDocBody(id!, body, baseUpdatedAt);
      lastSaved = body;
      baseUpdatedAt = meta.updatedAt;
      if (!disposed) setStatus("saved");
    } catch (err) {
      if (err instanceof DocConflictError) {
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

  function updatePreviewNow(): void {
    if (!textareaEl) return;
    const previewEl = mainEl.querySelector<HTMLElement>(".doc-editor-preview");
    if (previewEl) previewEl.innerHTML = renderMarkdown(textareaEl.value);
  }

  const schedulePreview = (): void => {
    window.clearTimeout(previewTimer);
    previewTimer = window.setTimeout(updatePreviewNow, PREVIEW_DEBOUNCE_MS);
  };

  // Adopt the version saved elsewhere, dropping any local edits.
  async function loadTheirs(): Promise<void> {
    try {
      const fresh = await getDoc(id!);
      if (disposed) return;
      lastSaved = fresh.body;
      baseUpdatedAt = fresh.updatedAt;
      conflicted = false;
      if (editing && textareaEl) {
        textareaEl.value = lastSaved;
        updatePreviewNow();
        setStatus("saved");
      } else {
        // Exited to read mode while this fetch was in flight (stopEditing awaits
        // its own save, not this one) — repaint so the page shows the adopted
        // version rather than the pre-fetch content.
        renderMain();
      }
    } catch (err) {
      console.error("reload failed", err);
      if (!disposed && conflicted) setStatus("conflict");
    }
  }

  // Overwrite the other save with the local draft (deliberate).
  async function keepMine(): Promise<void> {
    conflicted = false;
    baseUpdatedAt = ""; // this one save goes through unconditionally
    lastSaved = ""; // and even a body matching the last save re-PUTs
    await save();
  }

  async function stopEditing(): Promise<void> {
    window.clearTimeout(saveTimer);
    await save(); // flush any pending edit; reads the textarea before teardown
    // Only leave edit mode once the body is actually persisted. A conflict or a
    // failed PUT leaves the textarea ≠ lastSaved; exiting then would drop the
    // unsaved text silently (read mode has no save status, and the conflict bar
    // would vanish). Stay put so the user can resolve it first.
    if (textareaEl && textareaEl.value !== lastSaved) return;
    editing = false;
    renderMain();
  }

  function editModeHtml(meta: Doc): string {
    // Every page carries one project (its whole address in the flat wiki);
    // empty means it shows only under "all projects".
    const groupInput = `<input class="doc-group-input" list="doc-groups" placeholder="project…"
             value="${escapeHtml(meta.group ?? "")}" autocomplete="off"
             title="project — empty shows only under all projects">`;
    return `
      <div class="doc-edit-bar">
        <input class="doc-title-input" value="${escapeHtml(meta.title)}" autocomplete="off">
        ${groupInput}
        <span class="doc-save" id="doc-save"></span>
        <button type="button" class="nav-link" id="doc-done">done</button>
      </div>
      <div class="doc-editor">
        <textarea class="doc-editor-input" spellcheck="false">${escapeHtml(lastSaved)}</textarea>
        <div class="md doc-editor-preview">${renderMarkdown(lastSaved)}</div>
      </div>
    `;
  }

  function wireEditMode(meta: Doc): void {
    textareaEl = mainEl.querySelector<HTMLTextAreaElement>(".doc-editor-input")!;
    const saveEl = mainEl.querySelector<HTMLElement>("#doc-save")!;
    const titleInput = mainEl.querySelector<HTMLInputElement>(".doc-title-input")!;
    const doneBtn = mainEl.querySelector<HTMLButtonElement>("#doc-done")!;

    setStatus(conflicted ? "conflict" : saving ? "saving" : "");

    textareaEl.addEventListener("input", () => {
      schedulePreview();
      scheduleSave();
    });

    saveEl.addEventListener("click", (e) => {
      const act = (e.target as HTMLElement).closest<HTMLElement>("[data-conflict]")?.dataset.conflict;
      if (act === "theirs") void loadTheirs();
      else if (act === "mine") void keepMine();
    });

    titleInput.addEventListener("change", () => {
      const v = titleInput.value.trim();
      if (v && v !== meta.title) {
        patchDoc(meta.id, { title: v }).catch((err: unknown) => console.error("title save failed", err));
      } else {
        titleInput.value = meta.title;
      }
    });
    titleInput.addEventListener("keydown", (e) => {
      if (e.key === "Enter") titleInput.blur();
    });

    const groupInput = mainEl.querySelector<HTMLInputElement>(".doc-group-input");
    groupInput?.addEventListener("change", () => {
      patchDoc(meta.id, { group: groupInput.value }).catch((err: unknown) =>
        console.error("group save failed", err),
      );
    });

    doneBtn.addEventListener("click", () => void stopEditing());
  }

  // --- main pane: dispatch --------------------------------------------------

  function renderMain(): void {
    if (docsLoadFailed) {
      mainEl.innerHTML = `<div class="empty-state">failed to load pages.</div>`;
      announce("failed to load pages.");
      clearToc();
      return;
    }
    if (!docsLoaded) return; // still loading; the initial placeholder stands
    if (docs.length === 0) {
      mainEl.innerHTML = `<div class="empty-state">No pages yet — name one in the top bar and hit enter.</div>`;
      clearToc();
      return;
    }
    if (!id) {
      // autoOpenFirst() has already redirected when the scope HAS a page, so
      // reaching here with no id means the scope is empty.
      mainEl.innerHTML = `<div class="empty-state">No pages in this scope yet — name one in the top bar.</div>`;
      clearToc();
      return;
    }
    const meta = docs.find((d) => d.id === id);
    if (!meta || bodyLoadFailed) {
      mainEl.innerHTML = `<div class="empty-state">failed to load this page.</div>`;
      announce("failed to load this page.");
      clearToc();
      return;
    }
    if (!bodyLoaded) {
      mainEl.innerHTML = `<div class="empty-state">loading…</div>`;
      clearToc();
      return;
    }
    if (editing) {
      mainEl.innerHTML = editModeHtml(meta);
      clearToc();
      wireEditMode(meta);
    } else {
      mainEl.innerHTML = readModeHtml(meta);
      wireReadMode(meta);
      renderToc();
      syncCardLinks();
    }
  }

  // --- data loading + SSE ----------------------------------------------------

  function loadDocs(): void {
    getDocs()
      .then((list) => {
        if (disposed) return;
        docs = list;
        docsLoaded = true;
        syncGroupOptions();
        if (autoOpenFirst()) return; // land on the first page, not an empty detail
        renderIndex();
        renderMain();
      })
      .catch((err: unknown) => {
        console.error("docs load failed", err);
        docsLoadFailed = true;
        renderMain();
      });
  }

  function loadDoc(docId: string): void {
    getDoc(docId)
      .then((d) => {
        // Bail if the view was torn down, or if edit mode was entered while the
        // fetch was in flight — clobbering lastSaved/baseUpdatedAt under an open
        // editor would drop the user's edits. The stale base then just makes
        // their next save 409 into the conflict bar, which is the right handoff.
        if (disposed || editing) return;
        lastSaved = d.body;
        baseUpdatedAt = d.updatedAt;
        bodyLoaded = true;
        document.title = `${d.title} — W4tch y0ur 4I c0d3`;
        renderMain();
      })
      .catch((err: unknown) => {
        console.error("doc load failed", err);
        bodyLoadFailed = true;
        renderMain();
      });
  }

  loadDocs();
  if (id) loadDoc(id);

  const unsubscribe = subscribeRawEvents((type, data) => {
    if (type !== "docs-updated") return;
    docs = (data as Doc[] | null) ?? [];
    docsLoadFailed = false; // a live push always supersedes a stale load failure
    syncGroupOptions();
    renderIndex();

    if (!id) {
      if (autoOpenFirst()) return; // a page now exists in scope — open it
      renderMain();
      return;
    }
    const meta = docs.find((d) => d.id === id);
    if (!meta) {
      navigate("/project/docs"); // deleted elsewhere
      return;
    }
    document.title = `${meta.title} — W4tch y0ur 4I c0d3`;

    if (!editing) {
      // The body was written elsewhere (e.g. an MCP write from a Claude
      // session)? updatedAt moved, but metadata-only SSE doesn't carry the
      // body — re-fetch it. A bare metadata edit (rename/move) leaves
      // updatedAt alone, so it just refreshes title/breadcrumbs.
      if (meta.updatedAt !== baseUpdatedAt) loadDoc(id);
      else renderMain();
      return;
    }
    // Edit mode: only the body-save conflict story reacts here — title/project
    // inputs are left alone so a foreign metadata change can't yank focus
    // mid-keystroke. While our own PUT is in flight this event is (usually)
    // its echo — skip; a genuinely-foreign write racing us fails that
    // conditional PUT anyway.
    if (saving || conflicted || meta.updatedAt === baseUpdatedAt) return;
    if (textareaEl && textareaEl.value === lastSaved) {
      void loadTheirs();
    } else {
      conflicted = true;
      setStatus("conflict");
    }
  });

  return () => {
    disposed = true;
    unsubscribe();
    window.clearTimeout(saveTimer);
    window.clearTimeout(previewTimer);
    if (editing) void save(); // flush any pending edit (no-op when conflicted)
    document.title = "W4tch y0ur 4I c0d3";
  };
}
