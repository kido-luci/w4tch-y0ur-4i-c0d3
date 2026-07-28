// The table view — a React island, the third one in this app after Excalidraw
// and the codegraph graph.
//
// Why React and dnd-kit here and NOT on the kanban: the kanban's native HTML5
// drag already works and only ever moves a card between two flat lists. The
// table's drag does something the kanban cannot — drop a row ONTO another card
// to nest it, and reorder rows inside a group — which is exactly the sortable/
// droppable bookkeeping dnd-kit exists for. Rewriting the working kanban to
// match would be churn; leaving the table without nesting would drop the
// feature. So the island is scoped to the view that needs it.
//
// The island owns nothing durable: every mutation goes back out through
// onPatch, board.ts persists it, and the refreshed list comes back down as
// props. There is no second copy of the board in here to drift.

import { useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import {
  DndContext,
  DragOverlay,
  KeyboardSensor,
  PointerSensor,
  closestCenter,
  useSensor,
  useSensors,
  type DragEndEvent,
  type DragStartEvent,
} from "@dnd-kit/core";
import {
  SortableContext,
  sortableKeyboardCoordinates,
  useSortable,
  verticalListSortingStrategy,
} from "@dnd-kit/sortable";
import { CSS } from "@dnd-kit/utilities";
import type { Cycle, Todo, TodoState } from "./api";

export interface BoardTableOptions {
  todos: Todo[];
  states: TodoState[];
  cycles: Cycle[];
  selectedId: string | null;
  onSelect(id: string): void;
  onPatch(id: string, patch: { status?: string; parentId?: string; order?: number }): void;
}

const KIND_ICON: Record<string, string> = { epic: "◈", story: "▢", task: "▪", bug: "▲" };
const PRIORITY_LABEL = ["", "low", "medium", "high", "urgent"];

function points(v?: number): string {
  if (!v) return "";
  return Number.isInteger(v) ? String(v) : v.toFixed(1);
}

/** A card plus the depth the tree renders it at (0 = top level, 1 = child). */
interface Row {
  todo: Todo;
  depth: number;
}

/**
 * Flatten the two-level hierarchy into display order: every top-level card
 * followed by its children. Cards whose parent was filtered out still render —
 * at depth 0, because hiding a card because of a card you cannot see is how a
 * filter starts lying about what exists.
 */
function toRows(todos: Todo[]): Row[] {
  const byParent = new Map<string, Todo[]>();
  const ids = new Set(todos.map((t) => t.id));
  const roots: Todo[] = [];
  for (const t of todos) {
    if (t.parentId && ids.has(t.parentId)) {
      const list = byParent.get(t.parentId) ?? [];
      list.push(t);
      byParent.set(t.parentId, list);
    } else {
      roots.push(t);
    }
  }
  const out: Row[] = [];
  for (const r of roots) {
    out.push({ todo: r, depth: 0 });
    for (const c of byParent.get(r.id) ?? []) out.push({ todo: c, depth: 1 });
  }
  return out;
}

function TableRow({
  row,
  states,
  cycles,
  selected,
  onSelect,
  overlay,
}: {
  row: Row;
  states: TodoState[];
  cycles: Cycle[];
  selected: boolean;
  onSelect(id: string): void;
  overlay?: boolean;
}) {
  const { todo, depth } = row;
  const { attributes, listeners, setNodeRef, transform, transition, isDragging } = useSortable({
    id: todo.id,
    disabled: overlay,
  });
  const state = states.find((s) => s.id === todo.status);
  const cycle = cycles.find((c) => c.id === todo.cycleId);
  const style = overlay
    ? undefined
    : { transform: CSS.Transform.toString(transform), transition, opacity: isDragging ? 0.4 : 1 };
  return (
    <tr
      ref={overlay ? undefined : setNodeRef}
      style={style}
      className={`bt-row${selected ? " selected" : ""}${depth ? " bt-child" : ""}${
        overlay ? " bt-overlay" : ""
      }`}
      onClick={() => onSelect(todo.id)}
    >
      <td className="bt-grip" {...attributes} {...listeners} title="drag to reorder, or drop onto a card to nest">
        ⠿
      </td>
      <td className="bt-seq">#{todo.seq}</td>
      <td className="bt-title" style={{ paddingLeft: depth ? 24 : undefined }}>
        <span className="todo-kind">{KIND_ICON[todo.kind ?? "task"] ?? "▪"}</span>
        {todo.title}
        {todo.rollup ? (
          <span className="bt-rollup">
            {todo.rollup.done}/{todo.rollup.children}
          </span>
        ) : null}
      </td>
      <td className="bt-status">{state?.name ?? todo.status}</td>
      <td className="bt-prio">{todo.priority ? PRIORITY_LABEL[todo.priority] : ""}</td>
      <td className="bt-pts">{points(todo.estimate)}</td>
      <td className="bt-cycle">{cycle?.name ?? ""}</td>
      <td className="bt-repo">{todo.repo ?? ""}</td>
    </tr>
  );
}

function BoardTable(opts: BoardTableOptions) {
  const [activeId, setActiveId] = useState<string | null>(null);
  const rows = useMemo(() => toRows(opts.todos), [opts.todos]);
  const sensors = useSensors(
    // A small distance threshold keeps a plain click on a row from being read
    // as a drag — the row's click opens the card panel.
    useSensor(PointerSensor, { activationConstraint: { distance: 4 } }),
    useSensor(KeyboardSensor, { coordinateGetter: sortableKeyboardCoordinates }),
  );

  function onDragStart(e: DragStartEvent) {
    setActiveId(String(e.active.id));
  }

  function onDragEnd(e: DragEndEvent) {
    setActiveId(null);
    const { active, over } = e;
    if (!over || active.id === over.id) return;
    const dragged = opts.todos.find((t) => t.id === active.id);
    const target = opts.todos.find((t) => t.id === over.id);
    if (!dragged || !target) return;

    // Dropping onto a TOP-LEVEL card nests under it; the server refuses the
    // nestings the board cannot draw (three levels, a card that has children),
    // and board.ts surfaces that error — so the rule lives in one place.
    if (!target.parentId && target.id !== dragged.parentId && !dragged.rollup) {
      opts.onPatch(dragged.id, { parentId: target.id, status: target.status });
      return;
    }
    // Otherwise it is a reorder within the target's column: take the target's
    // status and slot in beside it.
    opts.onPatch(dragged.id, { status: target.status, order: target.order + 0.5 });
  }

  if (!rows.length) {
    return <div className="empty-state">no cards match this filter</div>;
  }

  const active = activeId ? rows.find((r) => r.todo.id === activeId) : undefined;

  return (
    <DndContext
      sensors={sensors}
      collisionDetection={closestCenter}
      onDragStart={onDragStart}
      onDragEnd={onDragEnd}
      onDragCancel={() => setActiveId(null)}
    >
      <table className="board-table">
        <thead>
          <tr>
            <th />
            <th>#</th>
            <th>title</th>
            <th>column</th>
            <th>priority</th>
            <th>pts</th>
            <th>cycle</th>
            <th>repo</th>
          </tr>
        </thead>
        <tbody>
          <SortableContext items={rows.map((r) => r.todo.id)} strategy={verticalListSortingStrategy}>
            {rows.map((r) => (
              <TableRow
                key={r.todo.id}
                row={r}
                states={opts.states}
                cycles={opts.cycles}
                selected={r.todo.id === opts.selectedId}
                onSelect={opts.onSelect}
              />
            ))}
          </SortableContext>
        </tbody>
      </table>
      <DragOverlay>
        {active ? (
          <table className="board-table">
            <tbody>
              <TableRow
                row={active}
                states={opts.states}
                cycles={opts.cycles}
                selected={false}
                onSelect={() => {}}
                overlay
              />
            </tbody>
          </table>
        ) : null}
      </DragOverlay>
    </DndContext>
  );
}

/** Mounts the table into `host`; returns the teardown board.ts must call
 *  before it reclaims the DOM. */
export function mountBoardTable(host: HTMLElement, opts: BoardTableOptions): () => void {
  const root = createRoot(host);
  root.render(<BoardTable {...opts} />);
  // Deferred so React finishes its current commit before the tree goes away —
  // unmounting synchronously from inside a render is the classic island crash.
  return () => queueMicrotask(() => root.unmount());
}
