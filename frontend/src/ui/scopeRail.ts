import {
  deleteGroup,
  deleteProject,
  deleteProjectLogo,
  getGroups,
  getProjectRegistry,
  getUnmappedFolders,
  projectLogoURL,
  putGroup,
  putProject,
  putProjectLogo,
  renameProject,
  subscribeRawEvents,
} from "../api";
import type { Project, ProjectGroup } from "../api";
import { escapeHtml } from "../format";
import {
  getKnownGroups,
  getKnownProjects,
  getScope,
  getScopeParam,
  loadScopeIndex,
  setKnownTaxonomy,
  setScope,
} from "../scope/scope";

const COLLAPSED_KEY = "wyac-scope-collapsed"; // rail tree nodes the user has folded
const LOGO_MAX = 128; // px — downscale a raster logo before upload to keep data.db small

/** Rail tree collapse state — the set of folded node ids ("g:<name>" for a
    group, "p:<name>" for a project). Default is expanded (absent from the set). */
function loadCollapsed(): Set<string> {
  try {
    const raw = localStorage.getItem(COLLAPSED_KEY);
    if (raw) return new Set(JSON.parse(raw) as string[]);
  } catch {
    /* ignore corrupt/unavailable storage */
  }
  return new Set();
}

function saveCollapsed(set: Set<string>): void {
  try {
    localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...set]));
  } catch {
    /* storage unavailable — collapse state just won't persist */
  }
}

/** Downscale a raster logo to ≤LOGO_MAX px (PNG) so the stored blob stays tiny;
 *  an SVG keeps its vector crispness and uploads as-is. */
async function resizeLogo(file: File): Promise<Blob> {
  if (file.type === "image/svg+xml") return file;
  const bmp = await createImageBitmap(file);
  const scale = Math.min(1, LOGO_MAX / Math.max(bmp.width, bmp.height));
  const w = Math.max(1, Math.round(bmp.width * scale));
  const h = Math.max(1, Math.round(bmp.height * scale));
  const canvas = document.createElement("canvas");
  canvas.width = w;
  canvas.height = h;
  canvas.getContext("2d")!.drawImage(bmp, 0, 0, w, h);
  const blob = await new Promise<Blob | null>((res) => canvas.toBlob(res, "image/png"));
  if (!blob) throw new Error("could not process the image");
  return blob;
}

/** Mount the project rail (the scope list plus the group- and project-manager
    panels); onChange fires after anything that changes what the scope means. */
export function mountScopeRail(host: HTMLElement, onChange: () => void): void {
  const list = document.createElement("div");
  list.className = "rail-list";
  list.title = "project scope — every view but web follows it";
  const groupPanel = document.createElement("div");
  groupPanel.className = "scope-panel";
  groupPanel.hidden = true;
  const projPanel = document.createElement("div");
  projPanel.className = "scope-panel";
  projPanel.hidden = true;
  host.append(list, groupPanel, projPanel);

  // Unmapped Claude folders (owned by no project) — loaded when the project
  // manager opens, offered there to be claimed or merged.
  let unmapped: string[] = [];

  // Which tree nodes are folded (persisted). Read once; toggles mutate it.
  const collapsed = loadCollapsed();

  function visibleProjects(): Project[] {
    return getKnownProjects().filter((p) => !p.hidden);
  }

  /** The default scope for a fresh load — first group, else first visible
   *  project (rail order), so a project is always selected now that "all
   *  projects" is gone. "" only when there's nothing to select. */
  function firstScope(): string {
    if (getKnownGroups().length) {
      return [...getKnownGroups()].map((g) => g.name).sort((a, b) => a.localeCompare(b))[0]!;
    }
    return visibleProjects()[0]?.name ?? "";
  }

  function renderRows(): void {
    const current = getScope();
    const projs = visibleProjects();

    // Children of each project, by parent name, in rail order (ord then name).
    const kids = new Map<string, Project[]>();
    for (const p of projs) {
      if (!p.parent) continue;
      const arr = kids.get(p.parent);
      if (arr) arr.push(p);
      else kids.set(p.parent, [p]);
    }
    for (const arr of kids.values()) {
      arr.sort((a, b) => a.ord - b.ord || a.name.localeCompare(b.name));
    }

    const logoImg = (p: Project): string =>
      p.logoVersion > 0
        ? `<img class="rail-logo" src="${projectLogoURL(p.name, p.logoVersion)}" alt="">`
        : "";

    // Groups sit open by default; a project WITH children folds by default, to
    // keep the rail tidy (a parent with 20 nested repos stays tucked until wanted).
    // The stored set records deviations from that default — so a manual
    // fold/unfold still persists, and toggling twice returns to the default.
    const foldedByDefault = (kind: "group" | "project"): boolean => kind === "project";
    const isFolded = (nodeId: string, kind: "group" | "project"): boolean =>
      collapsed.has(nodeId) ? !foldedByDefault(kind) : foldedByDefault(kind);

    // One rail row. `depth` indents it; an expandable node leads with a fold
    // toggle (data-toggle) whose glyph reflects `folded`, a leaf with a spacer.
    const nodeRow = (
      scope: string,
      label: string,
      depth: number,
      opts: { logo?: string; nodeId?: string; expandable?: boolean; folded?: boolean } = {},
    ): string => {
      const isActive = scope === current;
      const active = isActive ? " rail-item--active" : "";
      // The rail is a list of locations and one of them is where you are, so
      // aria-current carries it. These are <button>s, not links, hence "true"
      // rather than "page" — the token has to describe the control, and only a
      // link can claim to be the current page.
      const cur = isActive ? ` aria-current="true"` : "";
      const pad = 8 + depth * 14;
      const lead =
        opts.expandable && opts.nodeId
          ? `<span class="rail-tree-toggle" data-toggle="${escapeHtml(opts.nodeId)}">${
              opts.folded ? "▸" : "▾"
            }</span>`
          : `<span class="rail-tree-spacer"></span>`;
      return `<button type="button" class="rail-item${active}"${cur} data-scope="${escapeHtml(
        scope,
      )}" style="padding-left:${pad}px">${lead}${opts.logo ?? ""}${escapeHtml(label)}</button>`;
    };

    // A project and, unless folded, its descendant subtree.
    const renderProject = (p: Project, depth: number): string => {
      const children = kids.get(p.name) ?? [];
      const nodeId = `p:${p.name}`;
      const folded = isFolded(nodeId, "project");
      const row = nodeRow(p.name, p.name, depth, {
        logo: logoImg(p),
        nodeId,
        expandable: children.length > 0,
        folded,
      });
      if (!children.length || folded) return row;
      return row + children.map((c) => renderProject(c, depth + 1)).join("");
    };

    // A group and, unless folded, its member projects (each with its subtree).
    const renderGroup = (g: ProjectGroup): string => {
      const nodeId = `g:${g.name}`;
      const folded = isFolded(nodeId, "group");
      const row = nodeRow(g.name, g.name, 0, { nodeId, expandable: g.projects.length > 0, folded });
      if (!g.projects.length || folded) return row;
      const members = new Set(g.projects);
      const body = g.projects
        .map((name) => {
          const p = projs.find((x) => x.name === name);
          // A member whose parent is also in this group renders under that
          // parent (nested), not a second time as a direct member.
          if (p && p.parent && members.has(p.parent)) return "";
          // A phantom member (a group name with no registry project) is a leaf.
          return p ? renderProject(p, 1) : nodeRow(name, name, 1);
        })
        .join("");
      return row + body;
    };

    const groupsHtml = getKnownGroups().length
      ? `<div class="rail-heading">groups</div>` +
        [...getKnownGroups()].sort((a, b) => a.name.localeCompare(b.name)).map(renderGroup).join("")
      : "";

    // Top-level projects: in no group and with no visible parent (their children
    // still nest under them). Usually empty once everything is grouped/parented.
    const inGroup = new Set<string>();
    for (const g of getKnownGroups()) for (const n of g.projects) inGroup.add(n);
    const topRows = projs
      .filter((p) => !inGroup.has(p.name) && !(p.parent && projs.some((x) => x.name === p.parent)))
      .map((p) => renderProject(p, 0));

    // A persisted scope may name a hidden/deleted project or group that shows
    // nowhere in the tree — keep it selectable rather than dropping the selection.
    const shown = new Set<string>();
    for (const g of getKnownGroups()) {
      shown.add(g.name);
      for (const n of g.projects) shown.add(n);
    }
    for (const p of projs) shown.add(p.name);
    if (current && !shown.has(current)) topRows.push(nodeRow(current, current, 0));

    const projHtml = topRows.length ? `<div class="rail-heading">projects</div>` + topRows.join("") : "";

    list.innerHTML =
      groupsHtml +
      projHtml +
      `<button type="button" class="rail-item rail-manage" data-act="manage-groups">+ groups…</button>` +
      `<button type="button" class="rail-item rail-manage" data-act="manage-projects">+ projects…</button>`;
  }

  list.addEventListener("click", (e) => {
    // A click on a fold toggle folds/unfolds that node — it must not also
    // change the scope, so handle it first (the toggle sits inside the button).
    const toggle = (e.target as HTMLElement).closest<HTMLElement>("[data-toggle]");
    if (toggle) {
      const id = toggle.dataset["toggle"]!;
      if (collapsed.has(id)) collapsed.delete(id);
      else collapsed.add(id);
      saveCollapsed(collapsed);
      renderRows();
      return;
    }
    const btn = (e.target as HTMLElement).closest<HTMLButtonElement>(".rail-item");
    if (!btn) return;
    if (btn.dataset["act"] === "manage-groups") {
      renderGroupPanel();
      projPanel.hidden = true;
      groupPanel.hidden = !groupPanel.hidden;
      return;
    }
    if (btn.dataset["act"] === "manage-projects") {
      renderProjectPanel();
      groupPanel.hidden = true;
      projPanel.hidden = !projPanel.hidden;
      if (!projPanel.hidden) refreshUnmapped();
      return;
    }
    const scope = btn.dataset["scope"];
    if (scope === undefined) return;
    groupPanel.hidden = true;
    projPanel.hidden = true;
    renderRows(); // instant highlight; setScope's popstate also re-renders view + rows
    setScope(scope); // persist + push into the path; the view re-renders via popstate
  });

  // --- group manager panel -------------------------------------------------

  // The member checklist offers every registry project name plus any member a
  // group already carries (e.g. a phantom from a repo whose folder aged out).
  function memberChoices(g: ProjectGroup | undefined): string[] {
    const set = new Set(getKnownProjects().map((p) => p.name));
    for (const p of g?.projects ?? []) set.add(p);
    return [...set].sort((a, b) => a.localeCompare(b));
  }

  /** editing: undefined = list only, "" = new-group form, else that group's. */
  function renderGroupPanel(editing?: string): void {
    const rows = getKnownGroups()
      .map(
        (g) => `
      <div class="scope-panel-row">
        <button type="button" class="scope-panel-name" data-act="edit" data-name="${escapeHtml(g.name)}">${escapeHtml(g.name)}</button>
        <span class="scope-panel-count">${g.projects.length}</span>
        <button type="button" class="scope-panel-del" data-act="del" data-name="${escapeHtml(g.name)}" title="delete group">✕</button>
      </div>`,
      )
      .join("");
    let form = "";
    if (editing !== undefined) {
      const g = getKnownGroups().find((k) => k.name === editing);
      const checks = memberChoices(g)
        .map(
          (p) => `
        <label class="scope-panel-check"><input type="checkbox" value="${escapeHtml(p)}"${
          g?.projects.includes(p) ? " checked" : ""
        }> ${escapeHtml(p)}</label>`,
        )
        .join("");
      form = `
      <form class="scope-panel-form" data-editing="${escapeHtml(editing)}">
        <input class="scope-panel-input" name="name" placeholder="group name…" value="${escapeHtml(editing)}" autocomplete="off">
        <div class="scope-panel-checks">${checks}</div>
        <div class="scope-panel-btns">
          <button type="submit" class="nav-link">save</button>
          <button type="button" class="nav-link" data-act="cancel">cancel</button>
        </div>
      </form>`;
    }
    groupPanel.innerHTML =
      (rows || `<div class="scope-panel-empty">no groups yet.</div>`) +
      (editing === undefined ? `<button type="button" class="nav-link" data-act="new">+ new group</button>` : "") +
      form;
  }

  groupPanel.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>("[data-act]");
    if (!btn) return;
    const act = btn.dataset["act"];
    if (act === "new") renderGroupPanel("");
    else if (act === "cancel") renderGroupPanel();
    else if (act === "edit") renderGroupPanel(btn.dataset["name"] ?? "");
    else if (act === "del") {
      const name = btn.dataset["name"] ?? "";
      if (!confirm(`delete group "${name}"? Projects and their data are untouched.`)) return;
      deleteGroup(name).catch((err: unknown) => console.error("delete group failed", err));
    }
  });

  groupPanel.addEventListener("submit", (e) => {
    e.preventDefault();
    const form = e.target as HTMLFormElement;
    const editing = form.dataset["editing"] ?? "";
    const name = (form.querySelector<HTMLInputElement>(".scope-panel-input")?.value ?? "").trim();
    if (!name) return;
    const members = [...form.querySelectorAll<HTMLInputElement>("input[type=checkbox]:checked")].map(
      (c) => c.value,
    );
    void (async () => {
      try {
        await putGroup(name, members);
        // A rename is save-under-new-name + drop the old row; the scope follows.
        if (editing && editing !== name) {
          await deleteGroup(editing);
          if (getScope() === editing) setScope(name, true);
        }
        renderGroupPanel();
      } catch (err) {
        console.error("save group failed", err);
        alert(err instanceof Error ? err.message : "save failed");
      }
    })();
  });

  // --- project manager panel -----------------------------------------------

  function refreshUnmapped(): void {
    getUnmappedFolders()
      .then((u) => {
        unmapped = u;
        if (!projPanel.hidden && !projPanel.querySelector(".scope-panel-form")) renderProjectPanel();
      })
      .catch(() => {
        /* claiming still works from a project's own folders */
      });
  }

  // The folder checklist offers this project's own folders plus every unmapped
  // one — claiming a folder here strips it off whatever project held it.
  function folderChoices(p: Project | undefined): string[] {
    const set = new Set(unmapped);
    for (const f of p?.folders ?? []) set.add(f);
    return [...set].sort((a, b) => a.localeCompare(b));
  }

  function logoBoxHtml(name: string, hasLogo: boolean, version: number): string {
    return hasLogo
      ? `<img class="scope-panel-logo-img" src="${projectLogoURL(name, version)}" alt=""><button type="button" class="scope-panel-del" data-act="logo-del" title="remove logo">✕</button>`
      : `<span class="scope-panel-logo-none">none</span>`;
  }

  // The projects-updated SSE won't re-render a form mid-edit, so a logo upload
  // or removal refreshes the preview in place; a fresh timestamp busts the cache.
  function setLogoPreview(name: string, hasLogo: boolean): void {
    const box = projPanel.querySelector<HTMLElement>(".scope-panel-logo-box");
    if (box) box.innerHTML = logoBoxHtml(name, hasLogo, Date.now());
  }

  /** editing: undefined = list only, "" = new-project form, else that project. */
  function renderProjectPanel(editing?: string): void {
    const rows = getKnownProjects()
      .map(
        (p) => `
      <div class="scope-panel-row">
        <button type="button" class="scope-panel-name${p.hidden ? " scope-panel-name--off" : ""}" data-act="edit" data-name="${escapeHtml(p.name)}">${escapeHtml(p.name)}${p.hidden ? " · hidden" : ""}</button>
        <span class="scope-panel-count">${(p.folders ?? []).length}</span>
        <button type="button" class="scope-panel-del" data-act="del" data-name="${escapeHtml(p.name)}" title="delete project">✕</button>
      </div>`,
      )
      .join("");
    let form = "";
    if (editing !== undefined) {
      const p = editing ? getKnownProjects().find((k) => k.name === editing) : undefined;
      const checks = folderChoices(p)
        .map(
          (f) => `
        <label class="scope-panel-check"><input type="checkbox" value="${escapeHtml(f)}"${
          (p?.folders ?? []).includes(f) ? " checked" : ""
        }> ${escapeHtml(f)}</label>`,
        )
        .join("");
      // Editing the name renames the project AND cascades the new name across
      // every card / page / drawing / group that labels it (confirmed on save).
      const nameField = `<input class="scope-panel-input" name="name" value="${escapeHtml(editing)}" placeholder="project name…" autocomplete="off">`;
      // The logo attaches to an existing name (uploaded on pick, not on save),
      // so it only shows when editing a real project — not the new-project form.
      const hasLogo = !!p && p.logoVersion > 0;
      const logoSection = editing
        ? `<div class="scope-panel-sub">logo</div>
        <div class="scope-panel-logo">
          <span class="scope-panel-logo-box">${logoBoxHtml(editing, hasLogo, hasLogo ? p!.logoVersion : 0)}</span>
          <label class="nav-link scope-panel-logo-pick">choose…<input type="file" accept="image/*" class="scope-panel-logo-input" hidden></label>
        </div>`
        : "";
      // The tree parent this project nests under. Offer every project except
      // itself and its own descendants (picking one of those would loop the
      // tree); the server also rejects a cycle.
      const banned = new Set<string>(editing ? [editing] : []);
      if (editing) {
        for (;;) {
          let grew = false;
          for (const q of getKnownProjects()) {
            if (q.parent && banned.has(q.parent) && !banned.has(q.name)) {
              banned.add(q.name);
              grew = true;
            }
          }
          if (!grew) break;
        }
      }
      const curParent = p?.parent ?? "";
      const parentOpts = [`<option value="">(none — top level)</option>`]
        .concat(
          getKnownProjects()
            .map((q) => q.name)
            .filter((n) => !banned.has(n))
            .sort((a, b) => a.localeCompare(b))
            .map(
              (n) =>
                `<option value="${escapeHtml(n)}"${n === curParent ? " selected" : ""}>${escapeHtml(n)}</option>`,
            ),
        )
        .join("");
      const parentSection = `<div class="scope-panel-sub">parent</div>
        <select class="scope-panel-input scope-panel-parent" name="parent">${parentOpts}</select>`;
      form = `
      <form class="scope-panel-form" data-editing="${escapeHtml(editing)}">
        ${nameField}
        <label class="scope-panel-check"><input type="checkbox" name="hidden"${p?.hidden ? " checked" : ""}> hidden (keep off the rail)</label>
        ${parentSection}
        ${logoSection}
        <div class="scope-panel-sub">folders${checks ? "" : " — none unmapped"}</div>
        <div class="scope-panel-checks">${checks}</div>
        <div class="scope-panel-btns">
          <button type="submit" class="nav-link">save</button>
          <button type="button" class="nav-link" data-act="cancel">cancel</button>
        </div>
      </form>`;
    }
    projPanel.innerHTML =
      (rows || `<div class="scope-panel-empty">no projects yet.</div>`) +
      (editing === undefined ? `<button type="button" class="nav-link" data-act="new">+ new project</button>` : "") +
      form;
  }

  projPanel.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>("[data-act]");
    if (!btn) return;
    const act = btn.dataset["act"];
    if (act === "new") renderProjectPanel("");
    else if (act === "cancel") renderProjectPanel();
    else if (act === "edit") renderProjectPanel(btn.dataset["name"] ?? "");
    else if (act === "del") {
      const name = btn.dataset["name"] ?? "";
      if (!confirm(`delete project "${name}"? Its folders fall back to unmapped; cards/pages keep their label.`)) return;
      deleteProject(name).catch((err: unknown) => console.error("delete project failed", err));
    } else if (act === "logo-del") {
      const name = btn.closest<HTMLFormElement>(".scope-panel-form")?.dataset["editing"];
      if (!name) return;
      deleteProjectLogo(name)
        .then(() => setLogoPreview(name, false))
        .catch((err: unknown) => console.error("remove logo failed", err));
    }
  });

  // Picking a file uploads the logo immediately (its own endpoint), separate
  // from the form's save; the preview updates in place.
  projPanel.addEventListener("change", (e) => {
    const input = (e.target as HTMLElement).closest<HTMLInputElement>(".scope-panel-logo-input");
    const file = input?.files?.[0];
    const name = input?.closest<HTMLFormElement>(".scope-panel-form")?.dataset["editing"];
    if (!file || !name) return;
    void (async () => {
      try {
        await putProjectLogo(name, await resizeLogo(file));
        setLogoPreview(name, true);
      } catch (err) {
        console.error("logo upload failed", err);
        alert(err instanceof Error ? err.message : "logo upload failed");
      } finally {
        input.value = ""; // let the same file be re-picked
      }
    })();
  });

  projPanel.addEventListener("submit", (e) => {
    e.preventDefault();
    const form = e.target as HTMLFormElement;
    const editing = form.dataset["editing"] ?? "";
    const name = (form.querySelector<HTMLInputElement>(".scope-panel-input")?.value ?? "").trim();
    if (!name) return;
    const folders = [...form.querySelectorAll<HTMLInputElement>('.scope-panel-checks input[type=checkbox]:checked')].map(
      (c) => c.value,
    );
    const hidden = form.querySelector<HTMLInputElement>('input[name="hidden"]')?.checked ?? false;
    const parent = form.querySelector<HTMLSelectElement>('select[name="parent"]')?.value ?? "";
    // ord is preserved on an edit/rename (from the original row), fresh on a new one.
    const cur = getKnownProjects().find((k) => k.name === editing);
    const ord = cur ? cur.ord : getKnownProjects().reduce((m, p) => Math.max(m, p.ord), -1) + 1;
    void (async () => {
      try {
        // A changed name renames + cascades the label across the stores first
        // (a user-data change — confirm it), then folder/hidden edits apply
        // under the new name. The scope follows the rename.
        if (editing && name !== editing) {
          if (
            !confirm(
              `Rename "${editing}" → "${name}"?\nEvery card, page, drawing and group labelled "${editing}" will be relabelled.`,
            )
          )
            return;
          await renameProject(editing, name);
          if (getScope() === editing) setScope(name, true);
        }
        await putProject(name, { folders, hidden, ord, parent });
        renderProjectPanel();
      } catch (err) {
        console.error("save project failed", err);
        alert(err instanceof Error ? err.message : "save failed");
      }
    })();
  });

  // --- live sync ------------------------------------------------------------

  // Mutations (our own included) come back as an SSE echo, the board/design
  // recipe. The nav chrome mounts once, so this subscription never needs
  // tearing down.
  subscribeRawEvents((type, data) => {
    if (type === "groups-updated") {
      const scope = getScope();
      const before = getScopeParam();
      setKnownTaxonomy(getKnownProjects(), (data as ProjectGroup[] | null) ?? []);
      renderRows();
      // Group membership changed, so what a label covers did too — the server
      // owns that rule, so refetch rather than recompute.
      void loadScopeIndex().then(onChange);
      if (!groupPanel.hidden && !groupPanel.querySelector(".scope-panel-form")) renderGroupPanel();
      const after = getScopeParam();
      if (scope && before !== after) onChange();
      return;
    }
    if (type === "projects-updated") {
      const before = getScopeParam();
      setKnownTaxonomy((data as Project[] | null) ?? [], getKnownGroups());
      renderRows();
      void loadScopeIndex().then(onChange); // the parent tree moved; see above
      if (!projPanel.hidden && !projPanel.querySelector(".scope-panel-form")) renderProjectPanel();
      refreshUnmapped(); // ownership changed → the unmapped set did too
      const after = getScopeParam();
      if (before !== after) onChange();
      return;
    }
  });

  // Clicking outside the rail closes any open manager panel.
  document.addEventListener("click", (e) => {
    if (host.contains(e.target as Node)) return;
    groupPanel.hidden = true;
    projPanel.hidden = true;
  });

  // The scope now lives in the path, so any navigation (a rail pick, Back/Forward,
  // or a bare-path link that syncScopeToURL re-scoped) must re-light the active
  // row. renderRows reads getScope(), which reads the path — so this stays correct.
  window.addEventListener("popstate", renderRows);

  // --- boot ----------------------------------------------------------------

  // Render immediately with just the persisted scope so the rail never flashes
  // empty, then fill in the registry and groups.
  renderRows();
  const hadScope = getScope();
  const beforeParam = getScopeParam(); // pre-load: labels resolve as their own folder
  Promise.all([getProjectRegistry(), getGroups(), loadScopeIndex()])
    .then(([projects, groups]) => {
      setKnownTaxonomy(projects, groups);
      // No "all projects" anymore: a fresh load (or a cleared scope) lands on a
      // default project so a scope is always active.
      if (!hadScope) {
        const def = firstScope();
        if (def) setScope(def, true);
      }
      renderRows();
      // Re-render the views when the load changed what the active scope resolves
      // to: a default was applied, a group scope expanded to its members, or a
      // merged project now owns more than its own name.
      if (getScopeParam() !== beforeParam) onChange();
    })
    .catch(() => {
      /* the persisted-scope-only rows stay */
    });
}
