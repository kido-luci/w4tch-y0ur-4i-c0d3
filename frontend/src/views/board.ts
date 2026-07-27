// View 3 — todo board (route `/project/board`), roadmap "coding manager".
// v2 UX: clicking a card opens a docked right panel (Jira-style) where every
// field is edited; each column has a type-and-Enter composer (Trello-style);
// drag & drop shows a ghost card + highlighted target column; labels render
// as colored chips (name hashed onto a fixed palette). A card links to any
// number of sessions (a ticket spans several) and shows their summed cost plus
// a chip to whatever PR they opened. State lives server-side in todos.json;
// mutations broadcast `todos-updated` over SSE.

import {
  createDoc,
  createDrawing,
  createTodo,
  deleteTodo,
  getDocs,
  getDrawings,
  getProjects,
  getSession,
  getSessions,
  getShips,
  getTodos,
  patchTodo,
  subscribeRawEvents,
} from "../api";
import type { Doc, Drawing, Session, Todo, TodoStatus } from "../api";
import {
  escapeHtml,
  formatCost,
  formatDuration,
  formatRelativeTime,
  formatTokens,
  truncate,
} from "../format";
import { renderInlineMarkdown, renderMarkdown } from "../markdown";
import { getScope, getScopeSet, labelForFolder, navigate } from "../scope";

const COLUMNS: { status: TodoStatus; label: string }[] = [
  { status: "backlog", label: "backlog" },
  { status: "doing", label: "doing" },
  { status: "done", label: "done" },
];

/** Deterministic palette slot for a label name. */
function labelClass(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return `lbl-${h % 6}`;
}

function labelChipsHtml(t: Todo, removable: boolean): string {
  return (t.labels ?? [])
    .map(
      (l) =>
        `<span class="todo-label ${labelClass(l)}" data-label="${escapeHtml(l)}">${escapeHtml(l)}${
          removable ? `<button type="button" class="label-remove" title="remove">✕</button>` : ""
        }</span>`,
    )
    .join("");
}

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

  function byColumn(status: TodoStatus): Todo[] {
    return todos.filter((t) => t.status === status && inScope(t));
  }

  function selected(): Todo | undefined {
    return selectedId ? todos.find((t) => t.id === selectedId) : undefined;
  }

  /** The card's linked sessions we've cached so far, in link order. */
  function linkedSessions(t: Todo): Session[] {
    const out: Session[] = [];
    for (const id of t.linkedSessionIds ?? []) {
      const s = sessCache.get(id);
      if (s) out.push(s);
    }
    return out;
  }

  /**
   * Tokens/cost strip for a linked card: the frozen snapshot once the card is
   * done, otherwise the cached sessions' live numbers summed — a ticket
   * routinely spans several sessions, and only the total is meaningful.
   */
  function sessMetricsHtml(t: Todo): string {
    if (t.snapshot) {
      const tok = formatTokens(t.snapshot.tokens);
      const cost = formatCost(t.snapshot.costUsd);
      const over = t.snapshot.sessions > 1 ? ` over ${t.snapshot.sessions} sessions` : "";
      return `<div class="todo-sess todo-sess-frozen" title="frozen when done${over}">✓ ${escapeHtml(tok)} tok · ${escapeHtml(cost)}</div>`;
    }
    const linked = linkedSessions(t);
    if (!linked.length) return "";
    const tok = formatTokens(linked.reduce((n, s) => n + s.totalTokens + s.agentTokens, 0));
    const cost = formatCost(linked.reduce((n, s) => n + s.costUsd + s.agentCostUsd, 0));
    const dot = linked.some((s) => s.running) ? `<span class="live-dot live-dot-inline"></span>` : "";
    const n =
      linked.length > 1
        ? `<span class="todo-sess-n" title="${linked.length} linked sessions">×${linked.length}</span>`
        : "";
    return `<div class="todo-sess">${dot}<span>${escapeHtml(tok)} tok · ${escapeHtml(cost)}</span>${n}</div>`;
  }

  /** `https://github.com/o/r/pull/123` → `PR #123`, falling back to "PR". */
  function prLabel(url: string): string {
    const n = /\/pull\/(\d+)/.exec(url)?.[1];
    return n ? `PR #${n}` : "PR";
  }

  /**
   * The review end of the loop: once a linked session opens a PR, the card
   * carries a chip straight to it, so the board alone answers "what's waiting
   * on me". Before that, it shows the branch the work is on — but never the
   * base branch, which is just noise.
   */
  function sessLinksHtml(t: Todo): string {
    const linked = linkedSessions(t);
    const prs = [...new Set(linked.map((s) => s.prUrl).filter(Boolean))] as string[];
    if (prs.length) {
      return `<div class="todo-links">${prs
        .map(
          (url) =>
            `<a class="todo-pr" href="${escapeHtml(url)}" target="_blank" rel="noreferrer"
               title="${escapeHtml(url)}">${escapeHtml(prLabel(url))}</a>`,
        )
        .join("")}</div>`;
    }
    const branches = [
      ...new Set(linked.map((s) => s.gitBranch).filter((b) => b && b !== "main" && b !== "master")),
    ];
    if (branches.length !== 1) return ""; // 0 = nothing to say; >1 = noise
    return `<div class="todo-links"><span class="todo-branch" title="branch">⑂ ${escapeHtml(
      truncate(branches[0]!, 28),
    )}</span></div>`;
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
    return `
      <div class="todo-card${sel}" draggable="true" data-id="${escapeHtml(t.id)}">
        ${labels}
        <div class="todo-title md-inline">${renderInlineMarkdown(t.title)}</div>
        ${sessLinksHtml(t)}
        ${sessMetricsHtml(t)}
        <div class="todo-meta">
          <span class="todo-seq">#${t.seq}</span>
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

  function render(): void {
    boardEl.innerHTML = COLUMNS.map(({ status, label }) => {
      const cards = byColumn(status);
      const sum = status === "done" ? doneSumHtml(cards) : "";
      return `
        <div class="board-col">
          <div class="board-col-head">${label}
            <span class="board-count">${cards.length}</span>
            ${sum}
          </div>
          <div class="board-cards" data-status="${status}">${cards.map(cardHtml).join("")}</div>
          ${composerHtml(status)}
        </div>`;
    }).join("");
    wireBoard();
    renderPanel();
  }

  // --- right panel -----------------------------------------------------------

  /** One linked-session row: live numbers, its PR, and an unlink button. */
  function panelSessRowHtml(id: string): string {
    const s = sessCache.get(id);
    const dot = s?.running ? `<span class="live-dot live-dot-inline"></span>` : "";
    const inner = s
      ? `${dot}<span class="cand-title">${escapeHtml(truncate(s.title || "untitled session", 30))}</span>
         <span class="cand-meta">${escapeHtml(labelForFolder(s.project))} · ${escapeHtml(
           formatTokens(s.totalTokens + s.agentTokens),
         )} tok · ${escapeHtml(formatCost(s.costUsd + s.agentCostUsd))}</span>`
      : `<span class="cand-meta">session ${escapeHtml(id.slice(0, 8))}…</span>`;
    const pr = s?.prUrl
      ? `<a class="panel-sess-pr" href="${escapeHtml(s.prUrl)}" target="_blank" rel="noreferrer"
           title="${escapeHtml(s.prUrl)}">${escapeHtml(prLabel(s.prUrl))}</a>`
      : "";
    return `
      <div class="panel-sess-row">
        <a class="panel-sess" href="/claude/session/${encodeURIComponent(id)}">${inner}</a>
        ${pr}
        <button type="button" class="panel-sess-unlink" data-sid="${escapeHtml(id)}" title="unlink">✕</button>
      </div>`;
  }

  /**
   * Panel block: every linked session as a row, plus a pick-list to link
   * another — a ticket that spans sessions is the norm, not the exception.
   */
  function panelSessHtml(t: Todo): string {
    const ids = t.linkedSessionIds ?? [];
    return `
      <div class="panel-field">
        <div class="panel-label">${ids.length ? "sessions" : "link a session"}</div>
        ${ids.map(panelSessRowHtml).join("")}
        <div class="panel-sess-list"><div class="panel-sess-empty">loading…</div></div>
      </div>`;
  }

  /** Running-first recent sessions in the todo's repo (all repos if unset).
   *  A card can carry a scope label rather than a real repo — an umbrella
   *  group name covering several repos — so an empty filtered list falls back
   *  to all repos instead of offering nothing to link. */
  async function sessionCandidates(repo?: string): Promise<Session[]> {
    let sessions = await getSessions(7, repo || undefined, "active");
    if (repo && sessions.length === 0) {
      sessions = await getSessions(7, undefined, "active");
    }
    return sessions
      .sort(
        (a, b) =>
          Number(b.running) - Number(a.running) || Date.parse(b.startedAt) - Date.parse(a.startedAt),
      )
      .slice(0, 8);
  }

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

  /** Journey block shell: the card's whole story as one chronological stream —
   *  created → sessions (PR riding its session) → check/release runs → done.
   *  fillJourney builds the rows; the shell only decides the hint. */
  function panelJourneyHtml(t: Todo): string {
    const hint = (t.linkedSessionIds ?? []).length
      ? ""
      : `<div class="panel-sess-empty">link a session to grow the journey</div>`;
    return `
      <div class="panel-field">
        <div class="panel-label">journey</div>
        <div class="panel-journey"><div class="panel-sess-empty">loading…</div></div>
        ${hint}
      </div>`;
  }

  /** One timeline entry: relative time, then whatever the event is. */
  function journeyRow(ts: string, body: string, cls = ""): string {
    return `
      <div class="panel-journey-row${cls}" title="${escapeHtml(ts)}">
        <span class="panel-journey-when">${escapeHtml(formatRelativeTime(ts))}</span>
        ${body}
      </div>`;
  }

  // A run's record can land moments after a session's last transcript line —
  // widen the match window past both ends so it isn't missed.
  const SHIP_WINDOW_PAD_MS = 5 * 60 * 1000;

  /** Fills the journey: created and done render from the card alone; linked
   *  sessions anchor the middle (their PR chips ride along — the data has no
   *  PR-opened timestamp, so a separate event would be an invented position),
   *  and the repo's runs inside the sessions' window slot in between, newest
   *  8 kept with a link into ships for the rest. */
  function fillJourney(t: Todo): void {
    const listEl = panelEl.querySelector<HTMLElement>(".panel-journey");
    if (!listEl) return;
    const linked = t.linkedSessionIds ?? [];

    const events: { ts: number; html: string }[] = [
      { ts: Date.parse(t.createdAt), html: journeyRow(t.createdAt, `<span>created</span>`) },
    ];
    if (t.snapshot) {
      const s = t.snapshot;
      events.push({
        ts: Date.parse(s.takenAt),
        html: journeyRow(
          s.takenAt,
          `<span class="panel-journey-done">✓ done</span><span>${escapeHtml(formatTokens(s.tokens))} tok · ${escapeHtml(
            formatCost(s.costUsd),
          )} · ${escapeHtml(formatDuration(s.durationMs))}</span>`,
          " panel-journey-row--done",
        ),
      });
    }
    const finish = (extra: { ts: number; html: string }[], footer = ""): void => {
      if (!listEl.isConnected) return;
      const all = [...events, ...extra].sort((a, b) => a.ts - b.ts);
      listEl.innerHTML = all.map((e) => e.html).join("") + footer;
    };
    if (!linked.length) {
      finish([]);
      return;
    }

    Promise.all(
      linked.map((id): Promise<Session | null> => {
        const cached = sessCache.get(id);
        if (cached) return Promise.resolve(cached);
        return getSession(id)
          .then((s) => {
            sessCache.set(id, s);
            return s;
          })
          .catch(() => null); // gone from disk — skipped, not fatal
      }),
    )
      .then((results) => {
        const sessions = results.filter((s): s is Session => s !== null);
        const sessEvents = sessions.map((s) => ({
          ts: Date.parse(s.startedAt),
          html: journeyRow(
            s.startedAt,
            `<a class="panel-journey-sess" href="/claude/session/${encodeURIComponent(s.id)}">${escapeHtml(
              truncate(s.title || "untitled session", 26),
            )}</a><span>${escapeHtml(formatDuration(s.durationMs))}</span>${
              s.prUrl
                ? `<a class="panel-sess-pr" href="${escapeHtml(s.prUrl)}" target="_blank" rel="noreferrer"
                     title="${escapeHtml(s.prUrl)}">${escapeHtml(prLabel(s.prUrl))}</a>`
                : ""
            }`,
          ),
        }));
        if (!t.repo || !sessions.length) {
          finish(sessEvents);
          return undefined;
        }
        const start = Math.min(...sessions.map((s) => Date.parse(s.startedAt))) - SHIP_WINDOW_PAD_MS;
        const end =
          (t.snapshot ? Date.parse(t.snapshot.takenAt) : Date.now()) + SHIP_WINDOW_PAD_MS;
        return getShips(0, t.repo, 200).then((res) => {
          const inWindow = res.ships.filter((r) => {
            const ts = Date.parse(r.ts);
            return ts >= start && ts <= end;
          });
          const shipEvents = inWindow.slice(0, 8).map((r) => {
            const exitBadge =
              r.exit === 0
                ? `<span class="ship-exit ship-exit--ok">green</span>`
                : `<span class="ship-exit ship-exit--fail">exit ${r.exit}</span>`;
            return {
              ts: Date.parse(r.ts),
              html: journeyRow(
                r.ts,
                `<span class="ship-kind">${escapeHtml(r.kind)}</span>${
                  r.version ? `<span>${escapeHtml(r.version)}</span>` : ""
                }${exitBadge}`,
              ),
            };
          });
          const footer =
            inWindow.length > 8
              ? `<a class="panel-ships-more" href="/project/ships">${inWindow.length - 8} earlier runs in ships →</a>`
              : "";
          finish([...sessEvents, ...shipEvents], footer);
        });
      })
      .catch(() => {
        if (listEl.isConnected) {
          listEl.innerHTML = `<div class="panel-sess-empty">failed to load the journey</div>`;
        }
      });
  }

  /** Note block: markdown-rendered view by default, textarea while editing. */
  function panelNoteHtml(t: Todo): string {
    if (noteEditingFor === t.id) {
      return `<textarea class="panel-note" rows="7" placeholder="details…">${escapeHtml(t.note ?? "")}</textarea>`;
    }
    if (t.note) return `<div class="panel-note-md md" title="click to edit">${renderMarkdown(t.note)}</div>`;
    return `<div class="panel-note-md panel-note-empty">details…</div>`;
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
          ${COLUMNS.map(
            ({ status, label }) =>
              `<option value="${status}"${status === t.status ? " selected" : ""}>${label}</option>`,
          ).join("")}
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
      ${panelSessHtml(t)}
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
    fillJourney(t);

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

  function wireBoard(): void {
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
            // Dropping into "doing" links the card to the session doing the
            // work: auto-link when exactly one is running in the todo's repo,
            // otherwise open the panel so the user picks. A card that already
            // has sessions is left alone — adding a second one behind the
            // user's back would quietly change what the done snapshot counts.
            if (status !== "doing" || !t || t.linkedSessionIds?.length) return;
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
    if (active && (panelEl.contains(active) || active.classList.contains("composer-input"))) return;
    render();
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
      boardEl.innerHTML = `<div class="empty-state">failed to load todos</div>`;
      console.error("failed to load todos", err);
    }
  }

  void refresh();
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
      if (t) fillJourney(t);
      return;
    }
    if (type !== "todos-updated") return;
    todos = (data as Todo[] | null) ?? [];
    loadLinkedSessions();
    renderIfIdle();
  });

  return unsubscribe;
}
