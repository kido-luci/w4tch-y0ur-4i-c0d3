// View — UI designs (route `/project/ui`): the `.fig`/`.pen` documents in the
// scope's repos (`<repo>/design/`), opened in the OpenPencil desktop app.
// Split out of the wireframe library (now `/project/wireframe`; its route was
// spelled `design` before the rename) — both briefly shared one view.
//
// These are files on disk, not library entries: the server owns nothing about
// their content, so there is no SSE channel to follow — the list is what it
// was at mount, and a reload picks up new ones.

import { getDesignFiles, openDesignFile } from "../api";
import type { DesignFile } from "../api";
import { escapeHtml, formatRelativeTime } from "../domain/format";
import { announce, showError } from "../app/live";
import { getScope } from "../scope";

/** Renders the UI-design file list into `container`; returns a cleanup callback. */
export function renderUIView(container: HTMLElement): () => void {
  // The Claude FOLDERS the scope covers — what the design-files endpoints
  // match against (a rail label like `memoirme-app` is not a folder name).
  // The scope LABEL: /api/design-files resolves it through the repo bindings.
  const scopeParam = getScope();

  container.innerHTML = `
    <div class="page">
      <div id="ui-msg"></div>
      <section class="design-files" id="ui-list">
        <div class="empty-state">loading…</div>
      </section>
    </div>
  `;

  // A failed open must not take the list down with it, so the message gets its
  // own host — showError replaces its host's children.
  const msgEl = container.querySelector<HTMLElement>("#ui-msg")!;
  const listEl = container.querySelector<HTMLElement>("#ui-list")!;

  function renderFiles(files: DesignFile[]): void {
    msgEl.replaceChildren();
    if (files.length === 0) {
      listEl.innerHTML = `<div class="empty-state">no design files in this scope yet —
        drop a .fig or .pen into a repo's <code>design/</code> folder.</div>`;
      return;
    }
    // One group per repo, in first-seen (most-recent-file) order; a single-repo
    // scope is the common case and gets no heading it cannot use.
    const byRepo = new Map<string, DesignFile[]>();
    for (const f of files) {
      const group = byRepo.get(f.folder);
      if (group) group.push(f);
      else byRepo.set(f.folder, [f]);
    }
    const rows = (group: DesignFile[]): string =>
      group
        .map(
          (f) => `
            <button type="button" class="design-file" data-path="${escapeHtml(f.path)}">
              <span class="design-file-name">${escapeHtml(f.name)}</span>
              <span class="design-file-meta">${formatRelativeTime(f.modifiedAt)}</span>
            </button>`,
        )
        .join("");
    listEl.innerHTML = `
      <h2 class="design-topic-head">design files<span class="design-topic-count">${files.length}</span></h2>
      ${[...byRepo]
        .map(([folder, group]) =>
          byRepo.size > 1
            ? `<div class="design-file-repo"><span class="design-file-repo-name">${escapeHtml(
                folder,
              )}</span>${rows(group)}</div>`
            : `<div class="design-file-repo">${rows(group)}</div>`,
        )
        .join("")}`;
  }

  function load(): void {
    getDesignFiles(scopeParam)
      .then(renderFiles)
      .catch(() => {
        showError(listEl, "failed to load design files.", load);
      });
  }
  load();

  listEl.addEventListener("click", (e) => {
    const btn = (e.target as HTMLElement).closest<HTMLElement>(".design-file");
    if (!btn) return;
    const path = btn.dataset.path ?? "";
    const name = btn.querySelector(".design-file-name")?.textContent ?? "file";
    openDesignFile(scopeParam, path)
      .then(() => {
        msgEl.replaceChildren();
        announce(`opening ${name}`);
      })
      .catch((err: Error) => showError(msgEl, err.message, load));
  });

  return () => {};
}
