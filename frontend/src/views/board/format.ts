import type { Todo, TodoState } from "../../api";
import { escapeHtml } from "../../domain/format";

// The columns are data now (data.db v12), not a const: the board renders
// whatever todo_states holds for this scope. FALLBACK_COLUMNS only covers the
// gap before the first fetch lands — the three ids it names are seeded by the
// migration, so it is never wrong, only incomplete.
export const FALLBACK_COLUMNS: TodoState[] = [
  { id: "backlog", name: "backlog", category: "todo", order: 0, builtin: true },
  { id: "doing", name: "doing", category: "started", order: 1, builtin: true },
  { id: "done", name: "done", category: "done", order: 2, builtin: true },
];

export const KIND_ICON: Record<string, string> = {
  epic: "◈",
  story: "▢",
  task: "▪",
  bug: "▲",
};

export const PRIORITY_LABEL = ["none", "low", "medium", "high", "urgent"];

/** Points render trimmed: 3 not 3.0, 2.5 stays 2.5. */
export function formatPoints(v: number): string {
  return Number.isInteger(v) ? String(v) : v.toFixed(1);
}

/** Deterministic palette slot for a label name. */
export function labelClass(name: string): string {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) >>> 0;
  return `lbl-${h % 6}`;
}

export function labelChipsHtml(t: Todo, removable: boolean): string {
  return (t.labels ?? [])
    .map(
      (l) =>
        `<span class="todo-label ${labelClass(l)}" data-label="${escapeHtml(l)}">${escapeHtml(l)}${
          removable ? `<button type="button" class="label-remove" title="remove">✕</button>` : ""
        }</span>`,
    )
    .join("");
}
