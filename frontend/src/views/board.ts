// View 3 — todo board (route `/project/board`), roadmap "coding manager".
// v2 UX: clicking a card opens a docked right panel (Jira-style) where every
// field is edited; each column has a type-and-Enter composer (Trello-style);
// drag & drop shows a ghost card + highlighted target column; labels render
// as colored chips (name hashed onto a fixed palette). A card links to any
// number of sessions (a ticket spans several) and shows their summed cost plus
// a chip to whatever PR they opened. State lives server-side in todos.json;
// mutations broadcast `todos-updated` over SSE.

import {
  createBoardState,
  createBoardView,
  createDoc,
  createDrawing,
  createTodo,
  deleteBoardState,
  deleteBoardView,
  deleteTodo,
  getBoardStates,
  getBoardViews,
  getCycles,
  getDocs,
  getDrawings,
  getProjects,
  getSession,
  getTodos,
  patchBoardState,
  patchBoardView,
  patchTodo,
  subscribeRawEvents,
} from "../api";
import type {
  BoardQuery,
  BoardView,
  Cycle,
  Doc,
  Drawing,
  Session,
  Todo,
  TodoKind,
  TodoState,
  TodoStatus,
  ViewKind,
} from "../api";
import { mountBoardTable } from "../ui/boardTableIsland";
import { matchesQuery, renderTimeline } from "../domain/boardQuery";
import {
  chipAttrs,
  escapeHtml,
  formatCost,
  formatRelativeTime,
  formatTokens,
  truncate,
} from "../domain/format";
import { showError } from "../app/live";
import { renderInlineMarkdown, renderMarkdown } from "../domain/markdown";
import { getScope, getScopeSet, labelForFolder, navigate } from "../scope";

import { FALLBACK_COLUMNS, KIND_ICON, PRIORITY_LABEL, formatPoints, labelChipsHtml } from "./board/format";
import { panelSessHtml, sessionCandidates, sessLinksHtml, sessMetricsHtml } from "./board/sessionLinks";
import { fillJourney, panelJourneyHtml } from "./board/journey";

/** Renders the board view into `container`; returns a cleanup callback. */
export function renderBoardView(container: HTMLElement, initialCardId?: string): () => void {
  let todos: Todo[] = [];
  let selectedId: string | null = null; // card open in the right panel
  // A deep-link preselects on the FIRST todos load, not at construction: the
  // drawings/docs fetches can land first, and their render would read the
  // not-yet-loaded card as stale and clear it. A genuinely stale id is
  // consumed without selecting — the link degrades to a plain board.
  let pendingCardId: string | null = initialCardId ?? null;
  let noteEditingFor: string | null = null; // todo id whose note is in edit mode
  let composerStatus: TodoStatus | null = null; // column whose composer is open
  let dragging = false;
  // The workflow's columns for this scope, and the cycles cards can be planned
  // into. Both are refetched on their SSE events.
  let states: TodoState[] = FALLBACK_COLUMNS;
  let cycles: Cycle[] = [];
  let savedViews: BoardView[] = [];
  // Which shape the board is drawing, and the filter riding every shape. The
  // filter is deliberately NOT persisted per-scope in localStorage: an unseen
  // filter that survives a reload reads as "my cards are gone". Saving one is
  // an explicit act — that is what savedViews are for.
  let viewKind: ViewKind = "board";
  let query: BoardQuery = {};
  let activeViewId: string | null = null;
  let unmountTable: (() => void) | null = null;
  // Inline editors, replacing what used to be prompt() dialogs. Only one is
  // ever open, and each holds the id (or "" for "the new-column form").
  let addingColumn = false;
  let renamingColumn: string | null = null;
  let savingView = false;
  let renamingView = false;
  // The rail's global project scope; a change re-renders the whole view, so
  // reading it once per mount is enough. The label feeds new cards, the set
  // filters (a group scope covers its name plus its member projects).
  const scope = getScope();
  const scopeSet = getScopeSet();
  // Linked sessions by id — filled lazily per linked card, kept fresh by
  // `session-updated` SSE events. Powers the live metrics on cards + panel.
  const sessCache = new Map<string, Session>();
  // Design library metadata (names for linked-wireframe chips + the panel
  // picker); loaded once, kept fresh by `drawings-updated` SSE events.
  let drawingList: Drawing[] = [];
  // Docs-wiki metadata (titles for linked-doc chips + the panel picker);
  // loaded once, kept fresh by `docs-updated` SSE events.
  let docList: Doc[] = [];

  container.innerHTML = `
    <div class="page">
      <div class="board-toolbar" id="board-toolbar"></div>
      <div class="board-layout">
        <section class="board" id="board">
          <div class="empty-state">loading…</div>
        </section>
        <aside class="board-panel hidden" id="panel"></aside>
      </div>
      <datalist id="board-projects"></datalist>
    </div>
  `;

  const boardEl = container.querySelector<HTMLElement>("#board")!;
  const panelEl = container.querySelector<HTMLElement>("#panel")!;
  const toolbarEl = container.querySelector<HTMLElement>("#board-toolbar")!;
  const datalistEl = container.querySelector<HTMLDataListElement>("#board-projects")!;

  getProjects()
    .then((projects) => {
      datalistEl.innerHTML = projects
        .map((name) => `<option value="${escapeHtml(name)}"></option>`)
        .join("");
    })
    .catch(() => {
      /* datalist is a convenience; free-text repo input still works */
    });

  /** The rail's scope is the board's only filter — a card without a repo
   *  shows under "all projects" only (strict since v0.63). */
  function inScope(t: Todo): boolean {
    return !scopeSet || (!!t.repo && scopeSet.has(t.repo));
  }

  /** Everything the current scope AND filter let through. */
  function visible(): Todo[] {
    return todos.filter((t) => inScope(t) && matchesQuery(t, query));
  }

  /** How many cards the filter is hiding — a short board must never read as
   *  an empty one. */
  function hiddenCount(): number {
    const inScopeCount = todos.filter(inScope).length;
    return inScopeCount - visible().length;
  }

  function byColumn(status: TodoStatus): Todo[] {
    return visible().filter((t) => t.status === status);
  }

  function stateById(id: string): TodoState | undefined {
    return states.find((s) => s.id === id);
  }

  function cycleById(id?: string): Cycle | undefined {
    return id ? cycles.find((c) => c.id === id) : undefined;
  }

  /** Cards this one nests (already scope+filter-checked). */
  function childrenOf(id: string): Todo[] {
    return visible().filter((t) => t.parentId === id);
  }

  function selected(): Todo | undefined {
    return selectedId ? todos.find((t) => t.id === selectedId) : undefined;
  }

  function cardHtml(t: Todo): string {
    const labels = t.labels?.length ? `<div class="todo-labels">${labelChipsHtml(t, false)}</div>` : "";
    const repo = t.repo ? `<span class="todo-repo">${escapeHtml(t.repo)}</span>` : "";
    const noteInd = t.note ? `<span class="todo-note-ind" title="has a note">≡</span>` : "";
    const nDraw = t.linkedDrawingIds?.length ?? 0;
    const drawInd = nDraw
      ? `<span class="todo-draw-ind" title="${nDraw} linked wireframe${nDraw > 1 ? "s" : ""}">✎ ${nDraw}</span>`
      : "";
    const nDoc = t.linkedDocIds?.length ?? 0;
    const docInd = nDoc
      ? `<span class="todo-doc-ind" title="${nDoc} linked doc${nDoc > 1 ? "s" : ""}">¶ ${nDoc}</span>`
      : "";
    const sel = t.id === selectedId ? " selected" : "";
    const kind = t.kind ?? "task";
    const kindInd = `<span class="todo-kind todo-kind-${kind}" title="${kind}">${KIND_ICON[kind] ?? "▪"}</span>`;
    const prio = t.priority
      ? `<span class="todo-prio todo-prio-${t.priority}" title="priority: ${PRIORITY_LABEL[t.priority]}"></span>`
      : "";
    const est = t.estimate
      ? `<span class="todo-est" title="${formatPoints(t.estimate)} story points">${formatPoints(t.estimate)}</span>`
      : "";
    const cyc = cycleById(t.cycleId);
    const cycInd = cyc ? `<span class="todo-cycle" title="cycle">${escapeHtml(cyc.name)}</span>` : "";
    // A parent shows how far its children have got; a child shows whose it is.
    const roll = t.rollup
      ? `<div class="todo-rollup" title="${t.rollup.done}/${t.rollup.children} children done">
           <span class="todo-rollup-bar"><i style="width:${
             t.rollup.children ? Math.round((t.rollup.done / t.rollup.children) * 100) : 0
           }%"></i></span>
           <span class="todo-rollup-text">${t.rollup.done}/${t.rollup.children}${
             t.rollup.estimate
               ? ` · ${formatPoints(t.rollup.estimateDone)}/${formatPoints(t.rollup.estimate)} pts`
               : ""
           }</span>
         </div>`
      : "";
    const parent = t.parentId ? todos.find((x) => x.id === t.parentId) : undefined;
    const parentInd = parent
      ? `<span class="todo-parent" title="child of #${parent.seq}">↳ #${parent.seq}</span>`
      : "";
    return `
      <div class="todo-card${sel}${t.parentId ? " todo-child" : ""}" draggable="true" data-id="${escapeHtml(t.id)}">
        ${labels}
        <div class="todo-title md-inline">${kindInd}${prio}${renderInlineMarkdown(t.title)}</div>
        ${roll}
        ${sessLinksHtml(t, sessCache)}
        ${sessMetricsHtml(t, sessCache)}
        <div class="todo-meta">
          <span class="todo-seq">#${t.seq}</span>
          ${parentInd}
          ${est}
          ${cycInd}
          ${repo}
          ${noteInd}
          ${drawInd}
          ${docInd}
          <span class="todo-actions">
            <button type="button" class="todo-btn todo-btn-danger todo-delete" title="delete">✕</button>
          </span>
        </div>
      </div>`;
  }

  function composerHtml(status: TodoStatus): string {
    if (composerStatus === status) {
      return `
        <div class="composer">
          <input class="composer-input" data-status="${status}" placeholder="type a title, Enter to add"
            autocomplete="off">
        </div>`;
    }
    return `<button type="button" class="board-add" data-status="${status}">+ add</button>`;
  }

  /** Σ of frozen snapshots over the (filtered) done cards. */
  function doneSumHtml(cards: Todo[]): string {
    let tokens = 0;
    let cost = 0;
    let n = 0;
    for (const t of cards) {
      if (!t.snapshot) continue;
      tokens += t.snapshot.tokens;
      cost += t.snapshot.costUsd;
      n++;
    }
    if (n === 0) return "";
    return `<span class="board-sum" title="frozen cost across ${n} done card${n > 1 ? "s" : ""}">Σ ${escapeHtml(
      formatTokens(tokens),
    )} tok · ${escapeHtml(formatCost(cost))}</span>`;
  }

  /** The filter + view switcher that rides every shape of the board. */
  function toolbarHtml(): string {
    const hidden = hiddenCount();
    const chip = (k: ViewKind, label: string) =>
      `<button type="button" ${chipAttrs(viewKind === k)} data-view="${k}">${label}</button>`;
    const kindChip = (k: TodoKind) =>
      `<button type="button" ${chipAttrs(query.kinds?.includes(k))} data-kind="${k}">${KIND_ICON[k]} ${k}</button>`;
    const viewOpts = savedViews
      .map(
        (v) =>
          `<option value="${escapeHtml(v.id)}"${v.id === activeViewId ? " selected" : ""}>${escapeHtml(
            v.name,
          )}</option>`,
      )
      .join("");
    const cycleOpts = cycles
      .map(
        (c) =>
          `<option value="${escapeHtml(c.id)}"${c.id === query.cycleId ? " selected" : ""}>${escapeHtml(
            c.name,
          )}</option>`,
      )
      .join("");
    return `
      <div class="board-toolbar-row">
        <span class="filter-group">${chip("board", "board")}${chip("table", "table")}${chip("timeline", "timeline")}</span>
        <input class="board-search" type="search" placeholder="search titles…"
          value="${escapeHtml(query.text ?? "")}" autocomplete="off">
        <span class="filter-group">${(["epic", "story", "task", "bug"] as TodoKind[]).map(kindChip).join("")}</span>
        <select class="board-cycle-filter">
          <option value="">any cycle</option>
          ${cycleOpts}
        </select>
        <button type="button" ${chipAttrs(query.unestimatedOnly)} data-unestimated="1">unestimated</button>
        <span class="board-toolbar-spacer"></span>
        <select class="board-view-picker">
          <option value="">saved views…</option>
          ${viewOpts}
        </select>
        ${
          savingView || renamingView
            ? `<form class="view-form">
                 <input class="view-f-name" placeholder="${
                   renamingView ? "rename this view" : "name this view"
                 }" value="${escapeHtml(renamingView ? (savedViews.find((v) => v.id === activeViewId)?.name ?? "") : "")}"
                   autocomplete="off" required>
                 <button type="submit" class="todo-btn">${renamingView ? "rename" : "save"}</button>
                 <button type="button" class="todo-btn view-f-cancel">cancel</button>
               </form>`
            : `<button type="button" class="todo-btn board-view-save" title="save the current filter as a view">save view</button>
               ${
                 activeViewId
                   ? `<button type="button" class="todo-btn board-view-rename" title="rename this saved view">rename</button>
                      <button type="button" class="todo-btn board-view-update" title="overwrite this view with the current filter">update</button>
                      <button type="button" class="todo-btn todo-btn-danger board-view-delete" title="delete this saved view">✕</button>`
                   : ""
               }`
        }
      </div>
      ${
        hidden > 0
          ? `<div class="board-filter-note">${hidden} card${hidden > 1 ? "s" : ""} hidden by the filter
               <button type="button" class="todo-btn board-filter-clear">clear</button></div>`
          : ""
      }`;
  }

  /** One column head, with its WIP state and the rename/delete affordances. */
  function colHeadHtml(s: TodoState, cards: Todo[]): string {
    const over = s.wipLimit ? cards.length > s.wipLimit : false;
    const wip = s.wipLimit
      ? `<span class="board-wip${over ? " over" : ""}" title="WIP limit">/${s.wipLimit}</span>`
      : "";
    const sum = s.category === "done" ? doneSumHtml(cards) : "";
    return `
      <div class="board-col-head" data-status="${escapeHtml(s.id)}">
        ${
          renamingColumn === s.id
            ? `<input class="board-col-rename" value="${escapeHtml(s.name)}" autocomplete="off">`
            : `<span class="board-col-name" title="${escapeHtml(
                s.category,
              )} column — double-click to rename">${escapeHtml(s.name)}</span>`
        }
        <span class="board-count">${cards.length}</span>${wip}
        ${sum}
        ${
          s.builtin
            ? ""
            : `<button type="button" class="todo-btn todo-btn-danger board-col-delete" title="delete column">✕</button>`
        }
      </div>`;
  }

  function renderBoardShape(): void {
    boardEl.innerHTML =
      states
        .map((s) => {
          const cards = byColumn(s.id);
          return `
        <div class="board-col">
          ${colHeadHtml(s, cards)}
          <div class="board-cards" data-status="${escapeHtml(s.id)}">${cards.map(cardHtml).join("")}</div>
          ${composerHtml(s.id)}
        </div>`;
        })
        .join("") +
      (addingColumn
        ? `<div class="board-col board-col-add">
             <form class="col-form">
               <input class="col-f-name" placeholder="column name" autocomplete="off" required>
               <select class="col-f-cat" title="what this column means to the board">
                 <option value="todo">todo — not started</option>
                 <option value="started" selected>started — in progress</option>
                 <option value="done">done — freezes the cost snapshot</option>
               </select>
               <input class="col-f-wip" type="number" min="0" step="1" placeholder="WIP limit (optional)">
               <div class="col-form-row">
                 <button type="submit" class="todo-btn">add</button>
                 <button type="button" class="todo-btn col-f-cancel">cancel</button>
               </div>
             </form>
           </div>`
        : `<div class="board-col board-col-add">
             <button type="button" class="board-add-col" title="add a workflow column">+ column</button>
           </div>`);
    wireBoard();
  }

  function render(): void {
    toolbarEl.innerHTML = toolbarHtml();
    wireToolbar();
    renderBody();
  }

  // Only the board underneath the toolbar. Typing in the search box re-renders
  // THIS, never the toolbar — rebuilding the chrome per keystroke takes the
  // focus out of the input mid-word, the same trap the git tab's commit filter
  // documents.
  function renderBody(): void {
    // The React island owns its subtree; tear it down before innerHTML would
    // yank the DOM out from under it.
    if (unmountTable) {
      unmountTable();
      unmountTable = null;
    }
    boardEl.classList.toggle("board-wide", viewKind !== "board");
    if (viewKind === "board") {
      renderBoardShape();
    } else if (viewKind === "table") {
      boardEl.innerHTML = `<div class="board-island" id="board-island"></div>`;
      unmountTable = mountBoardTable(boardEl.querySelector<HTMLElement>("#board-island")!, {
        todos: visible(),
        states,
        cycles,
        selectedId,
        onSelect: (id) => {
          if (selectedId !== id) noteEditingFor = null;
          selectedId = id;
          render();
        },
        onPatch: (id, patch) => saveField(id, patch),
      });
    } else {
      boardEl.innerHTML = renderTimeline(visible(), cycles, states);
      wireTimeline();
    }
    renderPanel();
  }

  // --- right panel -----------------------------------------------------------

  function fillCandidates(t: Todo): void {
    const listEl = panelEl.querySelector<HTMLElement>(".panel-sess-list");
    if (!listEl) return;
    const linked = t.linkedSessionIds ?? [];
    sessionCandidates(t.repo)
      .then((all) => {
        if (!listEl.isConnected) return;
        const cands = all.filter((s) => !linked.includes(s.id));
        listEl.innerHTML = cands.length
          ? cands
              .map(
                (s) => `
                  <button type="button" class="panel-sess-cand" data-sid="${escapeHtml(s.id)}">
                    ${s.running ? `<span class="live-dot live-dot-inline"></span>` : ""}
                    <span class="cand-title">${escapeHtml(truncate(s.title || "untitled session", 30))}</span>
                    <span class="cand-meta">${escapeHtml(labelForFolder(s.project))} · ${escapeHtml(formatRelativeTime(s.startedAt))}</span>
                  </button>`,
              )
              .join("")
          : `<div class="panel-sess-empty">no ${linked.length ? "other " : ""}recent sessions${
              t.repo ? ` in ${escapeHtml(t.repo)}` : ""
            }</div>`;
        listEl.querySelectorAll<HTMLButtonElement>(".panel-sess-cand").forEach((btn) => {
          btn.addEventListener("click", () => {
            editList(t.id, "linkedSessionIds", (cur) => [...cur, btn.dataset["sid"]!]);
          });
        });
      })
      .catch(() => {
        if (listEl.isConnected) {
          listEl.innerHTML = `<div class="panel-sess-empty">failed to load sessions</div>`;
        }
      });
  }

  /** Wireframe block: linked drawings as rows, a picker to link, + new. */
  function panelDrawingsHtml(t: Todo): string {
    const linkedIds = t.linkedDrawingIds ?? [];
    const rows = linkedIds
      .map((id) => {
        const d = drawingList.find((x) => x.id === id);
        return `
          <div class="panel-draw-row">
            <a class="panel-draw" href="/project/design/${encodeURIComponent(id)}" title="open in design">
              ✎ ${escapeHtml(truncate(d?.name ?? `${id.slice(0, 8)}…`, 34))}
            </a>
            <button type="button" class="panel-draw-unlink" data-did="${escapeHtml(id)}" title="unlink">✕</button>
          </div>`;
      })
      .join("");
    const candidates = drawingList.filter((d) => !linkedIds.includes(d.id));
    const picker = candidates.length
      ? `<select class="panel-draw-add">
          <option value="">link a wireframe…</option>
          ${candidates
            .map((d) => `<option value="${escapeHtml(d.id)}">${escapeHtml(truncate(d.name, 40))}</option>`)
            .join("")}
        </select>`
      : "";
    return `
      <div class="panel-field">
        <div class="panel-label">wireframes</div>
        ${rows}
        ${picker}
        <button type="button" class="panel-draw-new">+ new wireframe</button>
      </div>`;
  }

  /** Docs block: linked wiki pages as rows, a picker to link, + new. */
  function panelDocsHtml(t: Todo): string {
    const linkedIds = t.linkedDocIds ?? [];
    const rows = linkedIds
      .map((id) => {
        const d = docList.find((x) => x.id === id);
        return `
          <div class="panel-doc-row">
            <a class="panel-doc" href="/project/docs/${encodeURIComponent(id)}" title="open in docs">
              ¶ ${escapeHtml(truncate(d?.title ?? `${id.slice(0, 8)}…`, 34))}
            </a>
            <button type="button" class="panel-doc-unlink" data-docid="${escapeHtml(id)}" title="unlink">✕</button>
          </div>`;
      })
      .join("");
    const candidates = docList.filter((d) => !linkedIds.includes(d.id));
    const picker = candidates.length
      ? `<select class="panel-doc-add">
          <option value="">link a doc…</option>
          ${candidates
            .map((d) => `<option value="${escapeHtml(d.id)}">${escapeHtml(truncate(d.title, 40))}</option>`)
            .join("")}
        </select>`
      : "";
    return `
      <div class="panel-field">
        <div class="panel-label">docs</div>
        ${rows}
        ${picker}
        <button type="button" class="panel-doc-new">+ new doc</button>
      </div>`;
  }

  /** Name tables for the shared event renderer (domain/boardEvents.ts). */
  const eventNames = {
    state: (id: string): string => stateById(id)?.name ?? id ?? "—",
    cycle: (id: string): string => (id ? (cycleById(id)?.name ?? id) : "no cycle"),
    card: (id: string): string => {
      const c = todos.find((x) => x.id === id);
      return c ? `#${c.seq}` : "a card";
    },
  };

  /** Note block: markdown-rendered view by default, textarea while editing. */
  function panelNoteHtml(t: Todo): string {
    if (noteEditingFor === t.id) {
      return `<textarea class="panel-note" rows="7" placeholder="details…">${escapeHtml(t.note ?? "")}</textarea>`;
    }
    if (t.note) return `<div class="panel-note-md md" title="click to edit">${renderMarkdown(t.note)}</div>`;
    return `<div class="panel-note-md panel-note-empty">details…</div>`;
  }

  /** Kind, priority, points and cycle — the four fields that turn a card into
   *  something a plan can be built out of. */
  function panelPlanningHtml(t: Todo): string {
    const kinds: TodoKind[] = ["epic", "story", "task", "bug"];
    return `
      <div class="panel-field panel-grid">
        <div>
          <div class="panel-label">kind</div>
          <select class="panel-kind">
            ${kinds
              .map(
                (k) =>
                  `<option value="${k}"${(t.kind ?? "task") === k ? " selected" : ""}>${KIND_ICON[k]} ${k}</option>`,
              )
              .join("")}
          </select>
        </div>
        <div>
          <div class="panel-label">priority</div>
          <select class="panel-priority">
            ${PRIORITY_LABEL.map(
              (label, i) =>
                `<option value="${i}"${(t.priority ?? 0) === i ? " selected" : ""}>${label}</option>`,
            ).join("")}
          </select>
        </div>
        <div>
          <div class="panel-label">points</div>
          <input class="panel-estimate" type="number" min="0" step="0.5" placeholder="—"
            value="${t.estimate ? formatPoints(t.estimate) : ""}">
        </div>
        <div>
          <div class="panel-label">cycle</div>
          <select class="panel-cycle">
            <option value="">— none —</option>
            ${cycles
              .map(
                (c) =>
                  `<option value="${escapeHtml(c.id)}"${c.id === t.cycleId ? " selected" : ""}>${escapeHtml(
                    c.name,
                  )}</option>`,
              )
              .join("")}
          </select>
        </div>
      </div>`;
  }

  /** Parent picker + the children this card owns. Only top-level cards can be
   *  parents, so the picker offers exactly those (minus this card itself). */
  function panelParentHtml(t: Todo): string {
    const kids = childrenOf(t.id);
    const canNest = kids.length === 0;
    const options = todos
      .filter((x) => x.id !== t.id && !x.parentId && inScope(x))
      .map(
        (x) =>
          `<option value="${escapeHtml(x.id)}"${x.id === t.parentId ? " selected" : ""}>#${x.seq} ${escapeHtml(
            truncate(x.title, 40),
          )}</option>`,
      )
      .join("");
    const kidRows = kids
      .map(
        (k) =>
          `<button type="button" class="panel-child" data-id="${escapeHtml(k.id)}">
             <span class="todo-kind">${KIND_ICON[k.kind ?? "task"]}</span>
             <span class="cand-title">#${k.seq} ${escapeHtml(truncate(k.title, 34))}</span>
             <span class="cand-meta">${escapeHtml(stateById(k.status)?.name ?? k.status)}</span>
           </button>`,
      )
      .join("");
    return `
      <div class="panel-field">
        <div class="panel-label">parent</div>
        ${
          canNest
            ? `<select class="panel-parent">
                 <option value="">— top level —</option>
                 ${options}
               </select>`
            : `<div class="panel-hint">this card has children, so it stays top level</div>`
        }
        ${kids.length ? `<div class="panel-children">${kidRows}</div>` : ""}
      </div>`;
  }

  function renderPanel(): void {
    const t = selected();
    if (!t) {
      selectedId = null;
      panelEl.classList.add("hidden");
      panelEl.innerHTML = "";
      return;
    }
    panelEl.classList.remove("hidden");
    panelEl.innerHTML = `
      <div class="panel-head">
        <span class="todo-seq">#${t.seq}</span>
        <select class="panel-status">
          ${states
            .map(
              (s) =>
                `<option value="${escapeHtml(s.id)}"${s.id === t.status ? " selected" : ""}>${escapeHtml(
                  s.name,
                )}</option>`,
            )
            .join("")}
        </select>
        <button type="button" class="todo-btn panel-close" title="close">✕</button>
      </div>
      <input class="panel-title" value="${escapeHtml(t.title)}" autocomplete="off">
      <div class="panel-field">
        <div class="panel-label">labels</div>
        <div class="panel-labels">
          ${labelChipsHtml(t, true)}
          <input class="panel-label-input" placeholder="add label…" autocomplete="off">
        </div>
      </div>
      <div class="panel-field">
        <div class="panel-label">repo</div>
        <input class="panel-repo" list="board-projects" value="${escapeHtml(t.repo ?? "")}"
          placeholder="project name" autocomplete="off">
      </div>
      ${panelPlanningHtml(t)}
      ${panelParentHtml(t)}
      ${panelSessHtml(t, sessCache)}
      ${panelDrawingsHtml(t)}
      ${panelDocsHtml(t)}
      ${panelJourneyHtml(t)}
      <div class="panel-field">
        <div class="panel-label">note <span class="panel-label-hint">markdown</span></div>
        ${panelNoteHtml(t)}
      </div>
      <div class="panel-foot">
        <span class="panel-created">created ${escapeHtml(formatRelativeTime(t.createdAt))}</span>
        <button type="button" class="panel-delete">delete</button>
      </div>
    `;
    wirePanel(t);
  }

  function saveField(id: string, patch: Parameters<typeof patchTodo>[1]): void {
    patchTodo(id, patch)
      .then(refresh)
      .catch((err: unknown) => {
        console.error("todo save failed", err);
        // Say so before rolling the field back to server state (refresh below):
        // silently reverting made a failed edit — a note, a link — vanish with
        // no explanation. The create/delete paths already alert on failure.
        alert(err instanceof Error ? err.message : "save failed — your change was not saved");
        void refresh();
      });
  }

  type ListField = "labels" | "linkedSessionIds" | "linkedDrawingIds" | "linkedDocIds";

  // Update one of a card's array fields from the LATEST local state — not a Todo
  // captured when the panel rendered — mutating the in-memory copy optimistically
  // so a rapid second edit compounds onto the first. patchTodo replaces the whole
  // array, so two handlers reading the same frozen array would have the slower
  // write silently drop the other's change (unlink A then quickly unlink B, and
  // A comes back). saveField then persists and refresh reconciles with the server.
  function editList(id: string, field: ListField, fn: (cur: string[]) => string[]): void {
    const cur = todos.find((x) => x.id === id);
    if (!cur) return;
    const next = fn(cur[field] ?? []);
    cur[field] = next;
    saveField(id, { [field]: next } as Parameters<typeof patchTodo>[1]);
  }

  function wirePanel(t: Todo): void {
    const q = <E extends HTMLElement>(sel: string): E => panelEl.querySelector<E>(sel)!;

    q<HTMLButtonElement>(".panel-close").addEventListener("click", () => {
      selectedId = null;
      noteEditingFor = null;
      render();
    });
    q<HTMLSelectElement>(".panel-status").addEventListener("change", (e) => {
      saveField(t.id, { status: (e.target as HTMLSelectElement).value as TodoStatus });
    });

    const title = q<HTMLInputElement>(".panel-title");
    title.addEventListener("change", () => {
      if (title.value.trim()) saveField(t.id, { title: title.value });
      else title.value = t.title; // blank titles are rejected server-side anyway
    });
    title.addEventListener("keydown", (e) => {
      if (e.key === "Enter") title.blur();
    });

    const repo = q<HTMLInputElement>(".panel-repo");
    repo.addEventListener("change", () => saveField(t.id, { repo: repo.value }));

    q<HTMLSelectElement>(".panel-kind").addEventListener("change", (e) => {
      saveField(t.id, { kind: (e.target as HTMLSelectElement).value as TodoKind });
    });
    q<HTMLSelectElement>(".panel-priority").addEventListener("change", (e) => {
      saveField(t.id, { priority: Number((e.target as HTMLSelectElement).value) });
    });
    q<HTMLSelectElement>(".panel-cycle").addEventListener("change", (e) => {
      saveField(t.id, { cycleId: (e.target as HTMLSelectElement).value });
    });
    const est = q<HTMLInputElement>(".panel-estimate");
    est.addEventListener("change", () => {
      const v = est.value.trim() === "" ? 0 : Number(est.value);
      if (Number.isNaN(v) || v < 0) {
        est.value = t.estimate ? formatPoints(t.estimate) : "";
        return;
      }
      saveField(t.id, { estimate: v });
    });
    // Re-parenting is refused server-side for a nesting the board cannot draw
    // (three levels, a cycle); saveField surfaces that error rather than
    // silently reverting the select.
    panelEl.querySelector<HTMLSelectElement>(".panel-parent")?.addEventListener("change", (e) => {
      saveField(t.id, { parentId: (e.target as HTMLSelectElement).value });
    });
    panelEl.querySelectorAll<HTMLButtonElement>(".panel-child").forEach((btn) => {
      btn.addEventListener("click", () => {
        selectedId = btn.dataset["id"]!;
        noteEditingFor = null;
        render();
      });
    });

    // Note: rendered markdown by default; click swaps in the textarea, change
    // (blur with a new value) saves, Escape cancels, blur without a change
    // just falls back to the rendered view.
    const noteView = panelEl.querySelector<HTMLElement>(".panel-note-md");
    noteView?.addEventListener("click", (e) => {
      if ((e.target as HTMLElement).closest("a")) return; // links stay links
      noteEditingFor = t.id;
      render();
      const ta = panelEl.querySelector<HTMLTextAreaElement>(".panel-note");
      ta?.focus();
      ta?.setSelectionRange(ta.value.length, ta.value.length);
    });
    const note = panelEl.querySelector<HTMLTextAreaElement>(".panel-note");
    // Escape must swallow the change event Chrome fires when render() tears
    // the modified textarea out of the DOM — otherwise "cancel" still saves.
    let noteCanceled = false;
    note?.addEventListener("change", () => {
      if (noteCanceled) return;
      noteEditingFor = null;
      saveField(t.id, { note: note.value });
    });
    note?.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      noteCanceled = true;
      noteEditingFor = null;
      render();
    });
    note?.addEventListener("blur", () => {
      // Let a click land first (same trick as the column composers).
      setTimeout(() => {
        if (!note.isConnected || document.activeElement === note) return;
        noteEditingFor = null;
        render();
      }, 150);
    });

    const labelInput = q<HTMLInputElement>(".panel-label-input");
    labelInput.addEventListener("keydown", (e) => {
      if (e.key !== "Enter") return;
      e.preventDefault();
      const name = labelInput.value.trim();
      if (!name) return;
      labelInput.value = ""; // clear now, so a fast second label doesn't append onto this one
      // Same latest-state + optimistic model as editList, but kept inline so it
      // can refocus the input after the refresh re-renders the panel.
      const curCard = todos.find((x) => x.id === t.id);
      const nextLabels = [...(curCard?.labels ?? []), name];
      if (curCard) curCard.labels = nextLabels;
      patchTodo(t.id, { labels: nextLabels })
        .then(refresh)
        .then(() => panelEl.querySelector<HTMLInputElement>(".panel-label-input")?.focus())
        .catch((err: unknown) => {
          console.error("label add failed", err);
          alert(err instanceof Error ? err.message : "label add failed");
          void refresh();
        });
    });
    panelEl.querySelectorAll<HTMLButtonElement>(".label-remove").forEach((btn) => {
      btn.addEventListener("click", () => {
        const name = btn.closest<HTMLElement>(".todo-label")!.dataset["label"]!;
        editList(t.id, "labels", (cur) => cur.filter((l) => l !== name));
      });
    });

    panelEl.querySelectorAll<HTMLButtonElement>(".panel-sess-unlink").forEach((btn) => {
      btn.addEventListener("click", () => {
        const sid = btn.dataset["sid"]!;
        editList(t.id, "linkedSessionIds", (cur) => cur.filter((x) => x !== sid));
      });
    });
    fillCandidates(t);
    fillJourney(t, panelEl, sessCache, eventNames);

    panelEl.querySelector<HTMLSelectElement>(".panel-draw-add")?.addEventListener("change", (e) => {
      const did = (e.target as HTMLSelectElement).value;
      if (did) editList(t.id, "linkedDrawingIds", (cur) => [...cur, did]);
    });
    panelEl.querySelectorAll<HTMLButtonElement>(".panel-draw-unlink").forEach((btn) => {
      btn.addEventListener("click", () => {
        const did = btn.dataset["did"]!;
        editList(t.id, "linkedDrawingIds", (cur) => cur.filter((x) => x !== did));
      });
    });
    // Creates a wireframe named after the card, links it, and jumps into the
    // editor — "draw the wireframe for #12" in one click.
    panelEl.querySelector<HTMLButtonElement>(".panel-draw-new")?.addEventListener("click", () => {
      createDrawing(truncate(`#${t.seq} ${t.title}`, 60))
        .then(async (d) => {
          const cur = todos.find((x) => x.id === t.id);
          await patchTodo(t.id, { linkedDrawingIds: [...(cur?.linkedDrawingIds ?? []), d.id] });
          navigate(`/project/design/${encodeURIComponent(d.id)}`);
        })
        .catch((err: unknown) => console.error("new wireframe failed", err));
    });

    panelEl.querySelector<HTMLSelectElement>(".panel-doc-add")?.addEventListener("change", (e) => {
      const docid = (e.target as HTMLSelectElement).value;
      if (docid) editList(t.id, "linkedDocIds", (cur) => [...cur, docid]);
    });
    panelEl.querySelectorAll<HTMLButtonElement>(".panel-doc-unlink").forEach((btn) => {
      btn.addEventListener("click", () => {
        const docid = btn.dataset["docid"]!;
        editList(t.id, "linkedDocIds", (cur) => cur.filter((x) => x !== docid));
      });
    });
    // Creates a doc named after the card, links it, and jumps into the editor
    // — "write the doc for #12" in one click.
    panelEl.querySelector<HTMLButtonElement>(".panel-doc-new")?.addEventListener("click", () => {
      createDoc({ title: truncate(`#${t.seq} ${t.title}`, 60) })
        .then(async (d) => {
          const cur = todos.find((x) => x.id === t.id);
          await patchTodo(t.id, { linkedDocIds: [...(cur?.linkedDocIds ?? []), d.id] });
          navigate(`/project/docs/${encodeURIComponent(d.id)}`);
        })
        .catch((err: unknown) => console.error("new doc failed", err));
    });

    q<HTMLButtonElement>(".panel-delete").addEventListener("click", () => {
      if (!confirm(`Delete "${t.title}"?`)) return;
      selectedId = null;
      deleteTodo(t.id)
        .then(refresh)
        .catch((err: unknown) => {
          console.error("todo delete failed", err);
          alert(err instanceof Error ? err.message : "delete failed");
        });
    });
  }

  // --- columns: cards, composers, drag & drop --------------------------------

  /** DOM position of a (just-dropped) card → its new status + order. */
  function placementOf(cardEl: HTMLElement): { status: TodoStatus; order: number } {
    const colEl = cardEl.closest<HTMLElement>(".board-cards")!;
    const status = colEl.dataset["status"] as TodoStatus;
    const siblings = [...colEl.querySelectorAll<HTMLElement>(".todo-card")];
    const idx = siblings.indexOf(cardEl);
    const byId = new Map(todos.map((t) => [t.id, t]));
    const prev = idx > 0 ? byId.get(siblings[idx - 1]!.dataset["id"]!) : undefined;
    const next = idx < siblings.length - 1 ? byId.get(siblings[idx + 1]!.dataset["id"]!) : undefined;
    let order: number;
    if (prev && next) order = (prev.order + next.order) / 2;
    else if (prev) order = prev.order + 1;
    else if (next) order = next.order - 1;
    else order = 1;
    return { status, order };
  }

  /** The card in `colEl` that the cursor at `y` sits above, if any. */
  function cardAfter(colEl: HTMLElement, y: number): HTMLElement | null {
    for (const card of colEl.querySelectorAll<HTMLElement>(".todo-card:not(.dragging)")) {
      const rect = card.getBoundingClientRect();
      if (y < rect.top + rect.height / 2) return card;
    }
    return null;
  }

  function clearDragOver(): void {
    boardEl.querySelectorAll(".drag-over").forEach((el) => el.classList.remove("drag-over"));
  }

  /** The filter row. Handlers that only change what is SHOWN re-render the
   *  body; the toolbar itself is left alone so the search keeps focus. */
  function wireToolbar(): void {
    toolbarEl.querySelectorAll<HTMLButtonElement>("[data-view]").forEach((btn) => {
      btn.addEventListener("click", () => {
        viewKind = btn.dataset["view"] as ViewKind;
        render();
      });
    });
    toolbarEl.querySelectorAll<HTMLButtonElement>("[data-kind]").forEach((btn) => {
      btn.addEventListener("click", () => {
        const k = btn.dataset["kind"] as TodoKind;
        const cur = query.kinds ?? [];
        query = { ...query, kinds: cur.includes(k) ? cur.filter((x) => x !== k) : [...cur, k] };
        if (!query.kinds?.length) delete query.kinds;
        render();
      });
    });
    toolbarEl.querySelector<HTMLButtonElement>("[data-unestimated]")?.addEventListener("click", () => {
      query = { ...query, unestimatedOnly: !query.unestimatedOnly };
      if (!query.unestimatedOnly) delete query.unestimatedOnly;
      render();
    });
    const search = toolbarEl.querySelector<HTMLInputElement>(".board-search");
    search?.addEventListener("input", () => {
      const v = search.value.trim();
      query = { ...query, text: v };
      if (!v) delete query.text;
      renderBody(); // NOT render() — see the comment on renderBody
      updateFilterNote();
    });
    toolbarEl.querySelector<HTMLSelectElement>(".board-cycle-filter")?.addEventListener("change", (e) => {
      const v = (e.target as HTMLSelectElement).value;
      query = { ...query, cycleId: v };
      if (!v) delete query.cycleId;
      render();
    });
    toolbarEl.querySelector<HTMLButtonElement>(".board-filter-clear")?.addEventListener("click", () => {
      query = {};
      activeViewId = null;
      render();
    });
    toolbarEl.querySelector<HTMLSelectElement>(".board-view-picker")?.addEventListener("change", (e) => {
      const id = (e.target as HTMLSelectElement).value;
      const v = savedViews.find((x) => x.id === id);
      activeViewId = v ? v.id : null;
      query = v ? { ...v.query } : {};
      if (v) viewKind = v.kind;
      render();
    });
    toolbarEl.querySelector<HTMLButtonElement>(".board-view-save")?.addEventListener("click", () => {
      savingView = true;
      renamingView = false;
      render();
      toolbarEl.querySelector<HTMLInputElement>(".view-f-name")?.focus();
    });
    toolbarEl.querySelector<HTMLButtonElement>(".board-view-rename")?.addEventListener("click", () => {
      renamingView = true;
      savingView = false;
      render();
      const el = toolbarEl.querySelector<HTMLInputElement>(".view-f-name");
      el?.focus();
      el?.select();
    });
    // "update" rewrites the active view's filter to whatever is on screen —
    // the reason a saved view was previously a write-once thing.
    toolbarEl.querySelector<HTMLButtonElement>(".board-view-update")?.addEventListener("click", () => {
      if (!activeViewId) return;
      patchBoardView(activeViewId, { kind: viewKind, query })
        .then(loadViews)
        .catch((err: unknown) => alert(err instanceof Error ? err.message : "could not update the view"));
    });
    toolbarEl.querySelector<HTMLButtonElement>(".view-f-cancel")?.addEventListener("click", () => {
      savingView = false;
      renamingView = false;
      render();
    });
    toolbarEl.querySelector<HTMLFormElement>(".view-form")?.addEventListener("submit", (e) => {
      e.preventDefault();
      const name = toolbarEl.querySelector<HTMLInputElement>(".view-f-name")!.value.trim();
      if (!name) return;
      const done = (): Promise<void> => {
        savingView = false;
        renamingView = false;
        return loadViews();
      };
      const fail = (err: unknown): void => {
        alert(err instanceof Error ? err.message : "could not save the view");
      };
      if (renamingView && activeViewId) {
        patchBoardView(activeViewId, { name }).then(done).catch(fail);
        return;
      }
      createBoardView({ name, repo: scope || undefined, kind: viewKind, query })
        .then((v) => {
          activeViewId = v.id;
          return done();
        })
        .catch(fail);
    });
    toolbarEl.querySelector<HTMLButtonElement>(".board-view-delete")?.addEventListener("click", () => {
      if (!activeViewId) return;
      const v = savedViews.find((x) => x.id === activeViewId);
      if (!confirm(`Delete the view "${v?.name ?? ""}"?`)) return;
      deleteBoardView(activeViewId)
        .then(() => {
          activeViewId = null;
          return loadViews();
        })
        .catch((err: unknown) => alert(err instanceof Error ? err.message : "delete failed"));
    });
  }

  /** Keep the "N hidden" line honest while typing, without rebuilding the row
   *  the caret is sitting in. */
  function updateFilterNote(): void {
    const hidden = hiddenCount();
    let note = toolbarEl.querySelector<HTMLElement>(".board-filter-note");
    if (hidden <= 0) {
      note?.remove();
      return;
    }
    if (!note) {
      note = document.createElement("div");
      note.className = "board-filter-note";
      toolbarEl.appendChild(note);
    }
    note.innerHTML = `${hidden} card${hidden > 1 ? "s" : ""} hidden by the filter
      <button type="button" class="todo-btn board-filter-clear">clear</button>`;
    note.querySelector<HTMLButtonElement>(".board-filter-clear")?.addEventListener("click", () => {
      query = {};
      activeViewId = null;
      render();
    });
  }

  /** The timeline is read-only chrome; clicking a bar opens the card. */
  function wireTimeline(): void {
    boardEl.querySelectorAll<HTMLElement>("[data-todo-id]").forEach((el) => {
      el.addEventListener("click", () => {
        const id = el.dataset["todoId"]!;
        if (selectedId !== id) noteEditingFor = null;
        selectedId = id;
        render();
      });
    });
  }

  function wireBoard(): void {
    boardEl.querySelector<HTMLButtonElement>(".board-add-col")?.addEventListener("click", () => {
      addingColumn = true;
      renderBody();
      boardEl.querySelector<HTMLInputElement>(".col-f-name")?.focus();
    });
    boardEl.querySelector<HTMLButtonElement>(".col-f-cancel")?.addEventListener("click", () => {
      addingColumn = false;
      renderBody();
    });
    boardEl.querySelector<HTMLFormElement>(".col-form")?.addEventListener("submit", (e) => {
      e.preventDefault();
      const name = boardEl.querySelector<HTMLInputElement>(".col-f-name")!.value.trim();
      if (!name) return;
      const wipRaw = boardEl.querySelector<HTMLInputElement>(".col-f-wip")!.value.trim();
      const wip = wipRaw === "" ? 0 : Number(wipRaw);
      if (Number.isNaN(wip) || wip < 0) return;
      createBoardState({
        name,
        category: boardEl.querySelector<HTMLSelectElement>(".col-f-cat")!.value as
          | "todo"
          | "started"
          | "done",
        repo: scope || undefined,
        wipLimit: wip,
      })
        .then(() => {
          addingColumn = false;
          return loadStates();
        })
        .catch((err: unknown) => alert(err instanceof Error ? err.message : "could not add the column"));
    });

    boardEl.querySelectorAll<HTMLElement>(".board-col-name").forEach((el) => {
      el.addEventListener("dblclick", () => {
        renamingColumn = el.closest<HTMLElement>(".board-col-head")!.dataset["status"]!;
        renderBody();
        const input = boardEl.querySelector<HTMLInputElement>(".board-col-rename");
        input?.focus();
        input?.select();
      });
    });

    // Enter commits, Escape and blur-without-change abandon — the same shape
    // the card note's inline editor uses.
    const rename = boardEl.querySelector<HTMLInputElement>(".board-col-rename");
    if (rename && renamingColumn) {
      const id = renamingColumn;
      const before = stateById(id)?.name ?? "";
      const close = (): void => {
        renamingColumn = null;
        renderBody();
      };
      const commit = (): void => {
        const name = rename.value.trim();
        if (!name || name === before) {
          close();
          return;
        }
        renamingColumn = null;
        patchBoardState(id, { name })
          .then(loadStates)
          .catch((err: unknown) => {
            alert(err instanceof Error ? err.message : "rename failed");
            void loadStates();
          });
      };
      rename.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          commit();
        } else if (e.key === "Escape") {
          close();
        }
      });
      rename.addEventListener("blur", () => {
        // A re-render can replace the input mid-flight; that stale blur must
        // not fight the fresh one.
        if (rename.isConnected && renamingColumn === id) commit();
      });
    }

    boardEl.querySelectorAll<HTMLButtonElement>(".board-col-delete").forEach((btn) => {
      btn.addEventListener("click", () => {
        const id = btn.closest<HTMLElement>(".board-col-head")!.dataset["status"]!;
        if (!confirm(`Delete the "${stateById(id)?.name ?? id}" column?`)) return;
        // The server refuses (409) while cards are still parked in it, which
        // is the message worth showing verbatim.
        deleteBoardState(id)
          .then(loadStates)
          .catch((err: unknown) => alert(err instanceof Error ? err.message : "delete failed"));
      });
    });

    boardEl.querySelectorAll<HTMLButtonElement>(".board-add").forEach((btn) => {
      btn.addEventListener("click", () => {
        composerStatus = btn.dataset["status"] as TodoStatus;
        render();
        boardEl.querySelector<HTMLInputElement>(".composer-input")?.focus();
      });
    });

    boardEl.querySelectorAll<HTMLInputElement>(".composer-input").forEach((input) => {
      const status = input.dataset["status"] as TodoStatus;
      input.addEventListener("keydown", (e) => {
        if (e.key === "Escape") {
          composerStatus = null;
          render();
          return;
        }
        if (e.key !== "Enter") return;
        e.preventDefault();
        const title = input.value.trim();
        if (!title) return;
        input.value = "";
        // A scoped board creates into the scope — otherwise the new card
        // would be filtered out of view the moment it lands.
        createTodo({ title, status, repo: scope || undefined })
          .then(refresh)
          .then(() => boardEl.querySelector<HTMLInputElement>(".composer-input")?.focus())
          .catch((err: unknown) => {
            console.error("todo create failed", err);
            alert(err instanceof Error ? err.message : "create failed");
          });
      });
      input.addEventListener("blur", () => {
        // Let a click land first (e.g. on another column's + add).
        setTimeout(() => {
          // A re-render replaces the input (e.g. right after Enter creates a
          // card); that stale blur must not close the fresh composer.
          if (!input.isConnected) return;
          if (composerStatus === status && !input.value.trim() && document.activeElement !== input) {
            composerStatus = null;
            render();
          }
        }, 150);
      });
    });

    boardEl.querySelectorAll<HTMLElement>(".todo-card").forEach((card) => {
      const id = card.dataset["id"]!;

      card.addEventListener("click", (e) => {
        if ((e.target as HTMLElement).closest(".todo-delete")) return;
        if ((e.target as HTMLElement).closest("a")) return; // markdown link in a title
        if (selectedId !== id) noteEditingFor = null;
        selectedId = id;
        render();
      });
      card.querySelector<HTMLButtonElement>(".todo-delete")!.addEventListener("click", () => {
        const t = todos.find((x) => x.id === id);
        if (!confirm(`Delete "${t?.title ?? "this todo"}"?`)) return;
        if (selectedId === id) selectedId = null;
        deleteTodo(id)
          .then(refresh)
          .catch((err: unknown) => {
            console.error("todo delete failed", err);
            alert(err instanceof Error ? err.message : "delete failed");
          });
      });

      // Drag & drop: dragover live-moves the card in the DOM (no placeholder);
      // dragend reads its final DOM position and persists it.
      let startCol: HTMLElement | null = null;
      let startNext: Element | null = null;
      card.addEventListener("dragstart", (e) => {
        dragging = true;
        startCol = card.parentElement;
        startNext = card.nextElementSibling;
        card.classList.add("dragging");
        e.dataTransfer!.effectAllowed = "move";
        e.dataTransfer!.setData("text/plain", id);
      });
      card.addEventListener("dragend", () => {
        dragging = false;
        card.classList.remove("dragging");
        clearDragOver();
        if (card.parentElement === startCol && card.nextElementSibling === startNext) {
          return; // didn't move
        }
        const { status, order } = placementOf(card);
        const t = todos.find((x) => x.id === id);
        patchTodo(id, { status, order })
          .then(() => {
            // Dropping into a started-category column links the card to the
            // session doing the work: auto-link when exactly one is running in
            // the todo's repo, otherwise open the panel so the user picks. A
            // card that already has sessions is left alone — adding a second
            // one behind the user's back would quietly change what the done
            // snapshot counts. Category, not the name "doing", so a renamed or
            // custom in-progress column behaves the same.
            if (stateById(status)?.category !== "started" || !t || t.linkedSessionIds?.length) return;
            return autoLink(id, t.repo);
          })
          .catch((err: unknown) => console.error("todo move failed", err))
          .finally(() => void refresh());
      });
    });

    boardEl.querySelectorAll<HTMLElement>(".board-cards").forEach((colEl) => {
      colEl.addEventListener("dragover", (e) => {
        e.preventDefault();
        const draggingEl = boardEl.querySelector<HTMLElement>(".todo-card.dragging");
        if (!draggingEl) return;
        clearDragOver();
        colEl.classList.add("drag-over");
        const after = cardAfter(colEl, e.clientY);
        if (after) colEl.insertBefore(draggingEl, after);
        else colEl.appendChild(draggingEl);
      });
    });
  }

  async function autoLink(id: string, repo?: string): Promise<void> {
    try {
      const running = (await sessionCandidates(repo)).filter((s) => s.running);
      if (running.length === 1) {
        await patchTodo(id, { linkedSessionIds: [running[0]!.id] });
      } else {
        selectedId = id;
      }
    } catch {
      /* linking is best-effort; the panel picker remains available */
    }
  }

  /** Fetch any linked sessions we haven't cached yet (SSE keeps them fresh). */
  function loadLinkedSessions(): void {
    for (const t of todos) {
      for (const sid of t.linkedSessionIds ?? []) {
        if (sessCache.has(sid)) continue;
        getSession(sid)
          .then((s) => {
            sessCache.set(sid, s);
            renderIfIdle();
          })
          .catch(() => {
            /* session gone from disk; the panel still shows the id + unlink */
          });
      }
    }
  }

  // Don't yank the DOM out from under an in-progress drag or edit; the next
  // refresh/render picks the change up.
  function renderIfIdle(): void {
    if (dragging) return;
    const active = document.activeElement;
    if (
      active &&
      (panelEl.contains(active) ||
        toolbarEl.contains(active) ||
        active.classList.contains("composer-input"))
    ) {
      return;
    }
    render();
  }

  /** The workflow's columns for this scope. A failure leaves the fallback trio
   *  in place rather than an empty board. */
  function loadStates(): Promise<void> {
    return getBoardStates(scope || undefined)
      .then((list) => {
        if (list.length) states = list;
        renderIfIdle();
      })
      .catch(() => {
        /* keep whatever we last had; the builtin trio always renders */
      });
  }

  function loadCycles(): Promise<void> {
    return getCycles(scope || undefined)
      .then((list) => {
        cycles = list;
        renderIfIdle();
      })
      .catch(() => {
        /* the cycle pickers just stay empty */
      });
  }

  function loadViews(): Promise<void> {
    return getBoardViews(scope || undefined)
      .then((list) => {
        savedViews = list;
        render();
      })
      .catch(() => {
        /* saved views are a convenience; the live filter still works */
      });
  }

  async function refresh(): Promise<void> {
    try {
      todos = await getTodos();
      if (pendingCardId) {
        if (todos.some((t) => t.id === pendingCardId)) selectedId = pendingCardId;
        pendingCardId = null;
      }
      // A save resolving mid-drag must not yank the board out from under the
      // gesture — the SSE path guards this too (renderIfIdle); drag-end fires
      // its own refresh, so the update isn't lost.
      if (dragging) return;
      render();
      loadLinkedSessions();
    } catch (err) {
      showError(boardEl, "failed to load todos", () => void refresh());
      console.error("failed to load todos", err);
    }
  }

  void refresh();
  void loadStates();
  void loadCycles();
  void loadViews();
  getDrawings()
    .then((list) => {
      drawingList = list;
      renderIfIdle();
    })
    .catch(() => {
      /* chips fall back to short ids; the picker just stays empty */
    });
  getDocs()
    .then((list) => {
      docList = list;
      renderIfIdle();
    })
    .catch(() => {
      /* picker just stays empty */
    });

  const unsubscribe = subscribeRawEvents((type, data) => {
    if (type === "session-updated") {
      const s = data as Session;
      if (!todos.some((t) => t.linkedSessionIds?.includes(s.id))) return;
      sessCache.set(s.id, s);
      renderIfIdle();
      return;
    }
    if (type === "drawings-updated") {
      drawingList = (data as Drawing[] | null) ?? [];
      renderIfIdle();
      return;
    }
    if (type === "docs-updated") {
      docList = (data as Doc[] | null) ?? [];
      renderIfIdle();
      return;
    }
    if (type === "ship-recorded") {
      const t = selected();
      if (t) fillJourney(t, panelEl, sessCache, eventNames);
      return;
    }
    // The columns, cycles and saved views each carry their whole list in the
    // event, but re-fetching keeps the scope filter server-side rather than
    // duplicating the union rule here.
    if (type === "board-states-updated") {
      void loadStates();
      return;
    }
    if (type === "cycles-updated") {
      void loadCycles();
      return;
    }
    if (type === "board-views-updated") {
      void loadViews();
      return;
    }
    if (type !== "todos-updated") return;
    todos = (data as Todo[] | null) ?? [];
    loadLinkedSessions();
    renderIfIdle();
  });

  return () => {
    if (unmountTable) unmountTable();
    unsubscribe();
  };
}
