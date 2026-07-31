// The topbar's presentation-mode toggle: one switch hides every private
// project — its rail chip AND the Claude work recorded on it (sessions,
// insights, search, git, MCP) — while you demo or screenshot. The state is
// server-side, so the SSE echo drives the glyph and every open tab agrees;
// the rail keeps its own subscription for chip filtering and the scope
// bounce (scopeRail.ts).

import { getPresentation, putPresentation, subscribeRawEvents } from "../api";

/** Wire the topbar button to reflect and flip presentation mode. The
 *  editorial glyph pair, monochrome like the theme toggle's: ◉ while
 *  everything shows, ⊘ while private projects are hidden. */
export function mountPresentationToggle(btn: HTMLElement): void {
  let on = false;
  const sync = (): void => {
    btn.textContent = on ? "⊘" : "◉";
    btn.title = on
      ? "private projects are hidden — click to show them again"
      : "hide private projects and their Claude sessions everywhere (mark projects private in the rail's + projects…)";
    btn.setAttribute("aria-pressed", String(on));
  };
  sync();
  getPresentation()
    .then((p) => {
      on = !!p.hidden;
      sync();
    })
    .catch(() => {
      /* stays off; the SSE echo corrects it on the first flip */
    });
  subscribeRawEvents((type, data) => {
    if (type !== "presentation-updated") return;
    on = !!(data as { hidden?: boolean } | null)?.hidden;
    sync();
  });
  btn.addEventListener("click", () => {
    putPresentation(!on).catch((err: unknown) => {
      console.error("presentation toggle failed", err);
      alert(err instanceof Error ? err.message : "presentation toggle failed");
    });
  });
}
