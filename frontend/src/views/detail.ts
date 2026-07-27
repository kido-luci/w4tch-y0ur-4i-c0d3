// View 2 — session detail (`/claude/session/<id>`).

import { getSession, getTodos, patchTodo, subscribeRawEvents, subscribeSessionEvents } from "../api";
import type { SessionDetail, Todo, ToolCount } from "../api";
import {
  escapeHtml,
  formatAbsoluteTime,
  formatCost,
  formatDuration,
  formatToolStats,
  formatTokens,
  linesBadgeHtml,
  modelBadgeHtml,
  modelColor,
  truncate,
} from "../format";
import { labelForFolder } from "../scope";
import { renderModelDistribution } from "../distribution";
import { renderSessionFlow } from "../flow";
import { renderSessionMilestones } from "../milestones";
import { renderAgentGraph } from "../graph";
import { renderInspectorBody } from "../inspector";
import { renderSessionRail } from "../rail";
import { renderSessionTimeline } from "../timeline";

/** Renders the session detail view into `container`; returns a cleanup callback. */
export function renderSessionDetailView(container: HTMLElement, id: string): () => void {
  let unsubscribe: (() => void) | null = null;
  // Set in cleanup: if the view is torn down while load()'s getSession is still
  // in flight, the resolved fetch must not render into detached nodes or open a
  // subscription that outlives the view — the leak the disposed guard prevents.
  let disposed = false;
  let latest: SessionDetail | null = null;
  let openTarget = "main"; // "main" or an agent id — the panel is always open
  let activeTab = "overview"; // which detail tab is showing; survives SSE re-renders
  let todos: Todo[] = []; // for the header's card backlinks / quick-link picker

  container.innerHTML = `
    <div class="page page-detail">
      <div class="detail-main">
        <div class="detail-topbar">
          <a href="/" class="back-link">← sessions</a>
        </div>
        <div id="detail-content" class="detail-content">
          <div class="empty-state">loading…</div>
        </div>
      </div>
      <aside id="inspector" class="inspector-panel" aria-label="node details"></aside>
    </div>
  `;

  const content = container.querySelector<HTMLElement>("#detail-content")!;
  const panel = container.querySelector<HTMLElement>("#inspector")!;

  // Left rail: a live session switcher. It renders async and takes updates from
  // the single SSE subscription opened below, so no second stream is needed.
  const rail = renderSessionRail(id);
  container.querySelector<HTMLElement>(".page-detail")!.prepend(rail.el);

  // Ring the node the panel is showing, across graph / timeline / cost cards.
  function applySelection(): void {
    content.querySelectorAll(".node-selected").forEach((n) => n.classList.remove("node-selected"));
    const sel = openTarget === "main" ? '[data-node="main"]' : `[data-agent-id="${openTarget}"]`;
    content.querySelectorAll(sel).forEach((n) => n.classList.add("node-selected"));
  }

  // Render the always-open panel for the current target, falling back to the
  // main session if a selected agent is gone; then highlight it.
  function syncInspector(): void {
    if (!latest) return;
    let body = renderInspectorBody(latest, openTarget);
    if (!body) {
      openTarget = "main";
      body = renderInspectorBody(latest, "main");
    }
    panel.innerHTML = body;
    applySelection();
  }

  function openInspector(target: string): void {
    openTarget = target;
    panel.scrollTop = 0;
    syncInspector();
  }

  // Show the panels for the current tab; re-applied after every renderDetail so
  // an SSE update mid-session doesn't snap the view back to the first tab.
  function applyActiveTab(): void {
    content
      .querySelectorAll<HTMLElement>("[data-tab]")
      .forEach((b) => b.classList.toggle("detail-tab-active", b.dataset["tab"] === activeTab));
    content
      .querySelectorAll<HTMLElement>("[data-tab-panel]")
      .forEach((p) => p.classList.toggle("tab-panel-active", p.dataset["tabPanel"] === activeTab));
  }

  content.addEventListener("click", (evt) => {
    // Tab switch is a pure show/hide — no re-render, so scroll and inspector
    // state are untouched.
    const tabBtn = (evt.target as HTMLElement).closest<HTMLElement>("[data-tab]");
    if (tabBtn) {
      const name = tabBtn.dataset["tab"];
      if (name && name !== activeTab) {
        activeTab = name;
        applyActiveTab();
      }
      return;
    }
    const el = (evt.target as HTMLElement).closest<HTMLElement>("[data-agent-id],[data-node]");
    if (!el) return;
    const target = el.dataset["node"] === "main" ? "main" : el.dataset["agentId"];
    if (target) openInspector(target);
  });

  /** Header slot: chips for the board cards claiming this session, or — when
   *  none does — a picker to link it to an open card right from here. */
  function syncCardLinks(): void {
    const slot = content.querySelector<HTMLElement>("#card-links-slot");
    if (!slot) return;
    const claims = todos.filter((t) => t.linkedSessionIds?.includes(id));
    if (claims.length) {
      slot.innerHTML = claims
        .map(
          (t) =>
            `<span class="dot-sep">·</span><a class="card-link" href="/project/board/${encodeURIComponent(
              t.id,
            )}" title="open on the board">#${t.seq} ${escapeHtml(truncate(t.title, 28))}</a>`,
        )
        .join("");
      return;
    }
    // A running session re-renders the header on every SSE tick — never yank
    // the picker out from under a user who has it focused/open.
    const existing = slot.querySelector<HTMLSelectElement>(".card-link-add");
    if (existing && document.activeElement === existing) return;
    const proj = latest?.project ?? "";
    const open = todos
      .filter((t) => t.status !== "done")
      .sort((a, b) => Number(b.repo === proj) - Number(a.repo === proj) || b.seq - a.seq);
    if (!open.length) {
      slot.innerHTML = "";
      return;
    }
    slot.innerHTML = `<span class="dot-sep">·</span><select class="card-link-add">
      <option value="">link to a card…</option>
      ${open
        .map(
          (t) =>
            `<option value="${escapeHtml(t.id)}">#${t.seq} ${escapeHtml(truncate(t.title, 32))}</option>`,
        )
        .join("")}
    </select>`;
    slot.querySelector<HTMLSelectElement>(".card-link-add")?.addEventListener("change", (e) => {
      const cardId = (e.target as HTMLSelectElement).value;
      const t = todos.find((x) => x.id === cardId);
      if (!t) return;
      // The todos-updated echo flips the slot into its claimed state.
      patchTodo(cardId, { linkedSessionIds: [...(t.linkedSessionIds ?? []), id] }).catch(
        (err: unknown) => console.error("link to card failed", err),
      );
    });
  }

  getTodos()
    .then((list) => {
      todos = list;
      syncCardLinks();
    })
    .catch(() => {
      /* backlinks are an extra — the header stands without them */
    });
  const unsubTodos = subscribeRawEvents((type, data) => {
    if (type !== "todos-updated") return;
    todos = (data as Todo[] | null) ?? [];
    syncCardLinks();
  });

  async function load(): Promise<void> {
    try {
      const session = await getSession(id);
      if (disposed) return; // navigated away mid-fetch — don't render or subscribe
      latest = session;
      renderDetail(content, session);
      applyActiveTab();
      syncInspector();
      syncCardLinks();
      // Pin the current session onto the rail even if the status filter would
      // otherwise exclude it (e.g. viewing an archived session).
      rail.update(session);

      // Subscribe regardless of running state: a session that is idle now may
      // resume later (the user sends another message), and the detail view
      // should pick that up without a manual reload. One connection per open
      // detail page, closed on unmount.
      if (!unsubscribe) {
        unsubscribe = subscribeSessionEvents((updated) => {
          rail.update(updated);
          if (updated.id === id) {
            latest = updated;
            renderDetail(content, updated);
            applyActiveTab();
            syncInspector();
            syncCardLinks(); // renderDetail rebuilt the header, slot included
          }
        });
      }
    } catch (err) {
      content.innerHTML = `<div class="empty-state">failed to load session</div>`;
      console.error("failed to load session", err);
    }
  }

  void load();

  return () => {
    disposed = true;
    unsubscribe?.();
    unsubTodos();
    rail.destroy();
  };
}

function renderDetail(el: HTMLElement, session: SessionDetail): void {
  const combinedTokens = session.totalTokens + session.agentTokens;
  const combinedCost = session.costUsd + session.agentCostUsd;

  const metaParts: string[] = [
    escapeHtml(labelForFolder(session.project)),
    escapeHtml(session.gitBranch),
  ];
  if (session.prUrl) {
    metaParts.push(
      `<a href="${escapeHtml(session.prUrl)}" target="_blank" rel="noopener noreferrer" class="pr-link">PR ↗</a>`,
    );
  }
  metaParts.push(escapeHtml(formatAbsoluteTime(session.startedAt)));
  metaParts.push(escapeHtml(formatDuration(session.durationMs)));
  metaParts.push(`${session.compactCount} compaction${session.compactCount === 1 ? "" : "s"}`);
  metaParts.push(`${session.messageCount} messages`);
  if (session.running) {
    metaParts.push(`<span class="live-dot live-dot-inline"></span>running`);
  }

  el.innerHTML = `
    <div class="card detail-header">
      <h1 class="detail-title">${escapeHtml(session.title || "untitled session")}</h1>
      <div class="detail-meta">${metaParts.join('<span class="dot-sep">·</span>')}<span id="card-links-slot"></span></div>
      <div class="totals-row">
        <div class="totals-item">
          <div class="totals-label">main tokens</div>
          <div class="totals-value">${formatTokens(session.totalTokens)}</div>
        </div>
        <div class="totals-item">
          <div class="totals-label">agent tokens</div>
          <div class="totals-value">${formatTokens(session.agentTokens)}</div>
        </div>
        <div class="totals-item">
          <div class="totals-label">combined</div>
          <div class="totals-value">${formatTokens(combinedTokens)}</div>
        </div>
        <div class="totals-item">
          <div class="totals-label">files changed</div>
          <div class="totals-value">${session.filesChanged || "—"}</div>
        </div>
        <div class="totals-item">
          <div class="totals-label">lines changed</div>
          <div class="totals-value">${linesBadgeHtml(session.linesAdded, session.linesRemoved)}</div>
        </div>
        <div class="totals-item">
          <div class="totals-label">est. cost</div>
          <div class="totals-value">${formatCost(combinedCost)}</div>
        </div>
      </div>
    </div>

    <div class="detail-tabs" role="tablist">
      <button type="button" class="detail-tab" role="tab" data-tab="overview">overview</button>
      <button type="button" class="detail-tab" role="tab" data-tab="agents">agents</button>
      <button type="button" class="detail-tab" role="tab" data-tab="cost">cost</button>
      <button type="button" class="detail-tab" role="tab" data-tab="activity">activity</button>
    </div>

    <div class="tab-panel" data-tab-panel="overview">
      <section class="card">
        <h2 class="section-heading">milestones</h2>
        <div id="milestones-slot"></div>
      </section>
      <section class="card">
        <h2 class="section-heading">flow</h2>
        <div id="flow-slot"></div>
      </section>
    </div>

    <div class="tab-panel" data-tab-panel="agents">
      <section class="card graph-card">
        <h2 class="section-heading">agent graph</h2>
        <div id="graph-slot"></div>
      </section>
      <section class="card">
        <h2 class="section-heading">agents</h2>
        <div id="agents-table-slot"></div>
      </section>
    </div>

    <div class="tab-panel" data-tab-panel="cost">
      <section class="card">
        <h2 class="section-heading">model distribution</h2>
        <div id="dist-slot"></div>
      </section>
      <section class="card">
        <h2 class="section-heading">costs</h2>
        <div id="cost-cards-slot"></div>
      </section>
    </div>

    <div class="tab-panel" data-tab-panel="activity">
      <section class="card">
        <h2 class="section-heading">timeline</h2>
        <div id="timeline-slot"></div>
      </section>
      <section class="card">
        <h2 class="section-heading">tool usage</h2>
        <div id="tool-usage-slot"></div>
      </section>
    </div>
  `;

  el.querySelector<HTMLElement>("#milestones-slot")!.appendChild(renderSessionMilestones(session));
  el.querySelector<HTMLElement>("#dist-slot")!.appendChild(
    renderModelDistribution(session.modelBreakdown ?? []),
  );
  el.querySelector<HTMLElement>("#flow-slot")!.appendChild(renderSessionFlow(session));
  el.querySelector<HTMLElement>("#graph-slot")!.appendChild(renderAgentGraph(session));
  el.querySelector<HTMLElement>("#timeline-slot")!.appendChild(renderSessionTimeline(session));
  el.querySelector<HTMLElement>("#tool-usage-slot")!.innerHTML = renderToolUsage(session.mainTools);
  el.querySelector<HTMLElement>("#cost-cards-slot")!.innerHTML = renderCostCards(session);
  el.querySelector<HTMLElement>("#agents-table-slot")!.innerHTML = renderAgentsTable(session);
}

// One cost card per participant — the main thread first, then each subagent,
// ranked by cost. Clickable (data-node/data-agent-id) so they open the same
// inspector as the graph and timeline.
interface CostEntry {
  attr: string;
  type: string;
  model: string;
  costUsd: number;
  totalTokens: number;
}

function renderCostCards(session: SessionDetail): string {
  const entries: CostEntry[] = [
    {
      attr: 'data-node="main"',
      type: "main",
      model: session.models[0] ?? "other",
      costUsd: session.costUsd,
      totalTokens: session.totalTokens,
    },
    ...session.agents.map((a) => ({
      attr: `data-agent-id="${escapeHtml(a.id)}"`,
      type: a.agentType || "agent",
      model: a.model,
      costUsd: a.costUsd,
      totalTokens: a.totalTokens,
    })),
  ];
  const ranked = entries.sort((a, b) => b.costUsd - a.costUsd);
  const maxCost = Math.max(...ranked.map((e) => e.costUsd), 0.000001);
  const cards = ranked
    .map((e) => {
      const share = Math.max(2, (e.costUsd / maxCost) * 100);
      return `
        <div class="cost-card" ${e.attr}>
          <div class="cost-card-head">
            <span class="cost-card-type">${escapeHtml(truncate(e.type, 18))}</span>
            ${modelBadgeHtml(e.model)}
          </div>
          <div class="cost-card-value">${escapeHtml(formatCost(e.costUsd))}</div>
          <div class="cost-card-meta">${escapeHtml(formatTokens(e.totalTokens))} tok</div>
          <div class="cost-card-bar"><div class="cost-card-bar-fill" style="width:${share.toFixed(1)}%;background:${modelColor(e.model)}"></div></div>
        </div>`;
    })
    .join("");
  return `<div class="cost-cards">${cards}</div>`;
}

// Main-thread tool usage — per-tool counts (MCP tools grouped by server), as a
// ranked bar list. Fills in what the main agent actually did.
function renderToolUsage(tools: ToolCount[] | undefined): string {
  if (!tools || tools.length === 0) {
    return `<div class="empty-state">no tool calls on the main thread</div>`;
  }
  const max = Math.max(...tools.map((t) => t.count), 1);
  const rows = tools
    .map((t) => {
      const w = Math.max(2, (t.count / max) * 100);
      return `
        <div class="tool-row">
          <span class="tool-name">${escapeHtml(t.name)}</span>
          <div class="tool-bar"><div class="tool-bar-fill" style="width:${w.toFixed(1)}%"></div></div>
          <span class="tool-count">${t.count}</span>
        </div>`;
    })
    .join("");
  return `<div class="tool-usage">${rows}</div>`;
}

function renderAgentsTable(session: SessionDetail): string {
  const ms = session.mainToolStats ?? null;
  const mainRow = `
    <tr data-node="main" class="agent-row-main">
      <td>main</td>
      <td class="cell-models">${session.models.map((m) => modelBadgeHtml(m)).join("")}</td>
      <td class="cell-desc">—</td>
      <td>${escapeHtml(formatTokens(session.totalTokens))}</td>
      <td>${escapeHtml(formatDuration(session.durationMs))}</td>
      <td>${escapeHtml(formatToolStats(ms))}</td>
      <td>${session.mainFilesChanged || "—"}</td>
      <td class="cell-lines">${ms ? linesBadgeHtml(ms.linesAdded, ms.linesRemoved) : "—"}</td>
      <td>${session.running ? "running" : "idle"}</td>
    </tr>`;

  const rows = session.agents
    .map(
      (a) => `
        <tr data-agent-id="${escapeHtml(a.id)}">
          <td>${escapeHtml(a.agentType)}</td>
          <td>${modelBadgeHtml(a.model)}</td>
          <td class="cell-desc">${escapeHtml(a.description || "—")}</td>
          <td>${escapeHtml(formatTokens(a.totalTokens))}</td>
          <td>${escapeHtml(formatDuration(a.durationMs))}</td>
          <td>${escapeHtml(formatToolStats(a.toolStats))}</td>
          <td>${a.filesChanged || "—"}</td>
          <td class="cell-lines">${linesBadgeHtml(a.linesAdded, a.linesRemoved)}</td>
          <td>${escapeHtml(a.status)}</td>
        </tr>`,
    )
    .join("");

  return `
    <table class="sessions-table agents-table">
      <thead>
        <tr>
          <th>type</th>
          <th>model</th>
          <th>description</th>
          <th>tokens</th>
          <th>duration</th>
          <th>tools</th>
          <th>files</th>
          <th>lines</th>
          <th>status</th>
        </tr>
      </thead>
      <tbody>${mainRow}${rows}</tbody>
    </table>
  `;
}
