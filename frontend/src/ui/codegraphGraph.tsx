// The code-graph canvas, as a React Flow island mounted into the vanilla view.
// Nodes are HTML cards (React Flow custom nodes); edges are ELK's orthogonal
// routes drawn as a plain SVG layer inside a <ViewportPortal> (so they pan and
// zoom with the graph) — deliberately NOT React Flow edges, which need measured
// handle bounds and silently drop otherwise. ELK routes edges through separated
// channels, so the layered graph reads instead of tangling.
//
// The vanilla side (chrome, chips, side panel, search, data prep) owns
// everything else and drives this island through `mountCodegraphGraph(el)`:
// call `update(props)` on every re-render, `destroy()` on teardown.

import { useEffect, useMemo, useState } from "react";
import { createRoot, type Root } from "react-dom/client";
import {
  Background,
  ReactFlow,
  ReactFlowProvider,
  useNodesInitialized,
  useReactFlow,
  ViewportPortal,
} from "@xyflow/react";
import type { Node, NodeProps } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import ELK from "elkjs/lib/elk.bundled.js";

const NODE_W = 200;
const NODE_H = 46;

const ICON: Record<string, string> = {
  file: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3v4a1 1 0 0 0 1 1h4"/><path d="M17 21H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h7l5 5v11a2 2 0 0 1-2 2z"/></svg>',
  folder: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 4h4l3 3h7a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2"/></svg>',
  test: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M9 3h6M10 3v6l-4.5 9a2 2 0 0 0 1.8 3h9.4a2 2 0 0 0 1.8-3L14 9V3"/></svg>',
  ghost: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6a2 2 0 0 1 2-2h4l3 3h7a2 2 0 0 1 2 2v6M3 6v12a2 2 0 0 0 2 2h11M4 4l16 16"/></svg>',
};

// One card node's display fields — computed vanilla-side and passed straight in.
export interface CardData extends Record<string, unknown> {
  label: string;
  badge: string;
  sub: string;
  count: string;
  icon: string;
  bg: string;
  fg: string;
  dot: string;
  cls: string; // file | dir | ghost
  info: string; // tooltip second line
  sel: boolean;
  dim: boolean;
}

export interface GVNode extends CardData {
  id: string;
}

export interface GVEdge {
  a: string;
  b: string;
  kind: string; // calls | imports | references
  weight: number;
}

export interface GraphProps {
  vnodes: GVNode[];
  vedges: GVEdge[];
  layout: string; // layered | tree | force | concentric
  selectedId: string | null;
  onDrill: (id: string) => void;
  onSelectFile: (id: string) => void;
  onHover: (id: string | null, clientX?: number, clientY?: number, info?: string) => void;
}

interface LaidEdge {
  id: string;
  a: string;
  b: string;
  kind: string;
  weight: number;
  points: { x: number; y: number }[];
}

const elk = new ELK();

// ELK's own result shape (elkjs ships no useful types for `layout()`'s return).
interface ElkPoint {
  x: number;
  y: number;
}
interface ElkResult {
  children?: { id: string; x?: number; y?: number }[];
  edges?: {
    id: string;
    sections?: { startPoint: ElkPoint; endPoint: ElkPoint; bendPoints?: ElkPoint[] }[];
  }[];
}

// --- ELK layout -------------------------------------------------------------

function elkOptions(layout: string): Record<string, string> {
  const base = { "elk.spacing.nodeNode": "44" };
  switch (layout) {
    case "tree":
      return { ...base, "elk.algorithm": "mrtree", "elk.direction": "RIGHT" };
    case "force":
      return { "elk.algorithm": "force", "elk.spacing.nodeNode": "90" };
    case "concentric":
      return { "elk.algorithm": "radial", "elk.spacing.nodeNode": "60" };
    default:
      return {
        ...base,
        "elk.algorithm": "layered",
        "elk.direction": "RIGHT",
        "elk.edgeRouting": "ORTHOGONAL",
        "elk.layered.nodePlacement.strategy": "NETWORK_SIMPLEX",
        "elk.layered.spacing.nodeNodeBetweenLayers": "96",
        "elk.layered.spacing.edgeNodeBetweenLayers": "24",
        "elk.spacing.edgeEdge": "16",
      };
  }
}

async function runElk(
  vnodes: GVNode[],
  vedges: GVEdge[],
  layout: string,
): Promise<{ nodes: Node<CardData>[]; edges: LaidEdge[] }> {
  const graph = {
    id: "root",
    layoutOptions: elkOptions(layout),
    children: vnodes.map((n) => ({ id: n.id, width: NODE_W, height: NODE_H })),
    edges: vedges.map((e, i) => ({ id: `e${i}`, sources: [e.a], targets: [e.b] })),
  };
  const res = (await elk.layout(graph)) as unknown as ElkResult;
  const nodes: Node<CardData>[] = (res.children ?? []).map((c) => {
    const src = vnodes.find((n) => n.id === c.id)!;
    // Explicit dimensions so React Flow treats the node as measured at once —
    // fixed-size cards, and without this RF leaves them visibility:hidden.
    return {
      id: c.id,
      type: "card",
      position: { x: c.x ?? 0, y: c.y ?? 0 },
      data: src,
      width: NODE_W,
      height: NODE_H,
      measured: { width: NODE_W, height: NODE_H },
      draggable: false,
      selectable: false,
    };
  });
  const edges: LaidEdge[] = (res.edges ?? []).map((e) => {
    const i = Number(e.id.slice(1));
    const ve = vedges[i]!;
    const sec = e.sections?.[0];
    const points = sec ? [sec.startPoint, ...(sec.bendPoints ?? []), sec.endPoint] : [];
    return { id: e.id, a: ve.a, b: ve.b, kind: ve.kind, weight: ve.weight, points };
  });
  return { nodes, edges };
}

// --- custom node + the SVG edge layer ---------------------------------------

function CardNode({ data }: NodeProps<Node<CardData>>) {
  const cls =
    "cgn" +
    (data.cls === "ghost" ? " cgn--ghost" : "") +
    (data.sel ? " cgn--sel" : "") +
    (data.dim ? " cgn--dim" : "");
  const badgeStyle =
    data.cls === "ghost"
      ? { color: data.fg, border: "0.5px solid var(--border)" }
      : { background: data.bg, color: data.fg };
  return (
    <div className={cls}>
      <span className="cgn-ic" dangerouslySetInnerHTML={{ __html: ICON[data.icon] ?? ICON["file"]! }} />
      <span className="cgn-badge" style={badgeStyle}>
        {data.badge}
      </span>
      <span className="cgn-title">{data.label}</span>
      <span className="cgn-cnt">{data.count}</span>
      <span className="cgn-sub">
        <span className="cgn-dot" style={{ background: data.dot }} />
        {data.sub}
      </span>
    </div>
  );
}

const nodeTypes = { card: CardNode };

/** ELK's edge routes as one SVG layer, positioned in flow coordinates by the
    <ViewportPortal> wrapper. Hovering a node lifts its edges out of the faint
    resting layer. */
function EdgesLayer({ edges, hover }: { edges: LaidEdge[]; hover: string | null }) {
  // The denser the graph, the fainter the resting edges — so the cards read
  // over the structure and hovering a folder still lifts its edges clear.
  const rest = edges.length > 200 ? 0.1 : edges.length > 90 ? 0.18 : 0.4;
  return (
    <svg style={{ position: "absolute", overflow: "visible", pointerEvents: "none", left: 0, top: 0 }}>
      <defs>
        <marker
          id="cg-arrow"
          markerWidth="7"
          markerHeight="7"
          refX="6"
          refY="3"
          orient="auto"
          markerUnits="userSpaceOnUse"
        >
          <path d="M0,0 L6,3 L0,6 Z" fill="var(--text-muted)" />
        </marker>
      </defs>
      {edges.map((e) => {
        if (e.points.length < 2) return null;
        const d = e.points.map((p, i) => `${i ? "L" : "M"}${p.x},${p.y}`).join(" ");
        const touches = hover ? e.a === hover || e.b === hover : false;
        const opacity = hover ? (touches ? 0.95 : 0.04) : rest;
        const dash = e.kind === "imports" ? "6 4" : e.kind === "references" ? "1 5" : undefined;
        return (
          <path
            key={e.id}
            d={d}
            fill="none"
            stroke="var(--text-muted)"
            strokeWidth={Math.min(1 + Math.log2(e.weight + 1) * 0.6, 4)}
            strokeDasharray={dash}
            opacity={opacity}
            markerEnd="url(#cg-arrow)"
          />
        );
      })}
    </svg>
  );
}

// --- graph component --------------------------------------------------------

function GraphInner(props: GraphProps) {
  const { vnodes, vedges, layout, selectedId, onDrill, onSelectFile, onHover } = props;
  const [base, setBase] = useState<{ nodes: Node<CardData>[]; edges: LaidEdge[] }>({
    nodes: [],
    edges: [],
  });
  const [hover, setHover] = useState<string | null>(null);
  const rf = useReactFlow();
  const inited = useNodesInitialized();

  const adj = useMemo(() => {
    const m = new Map<string, Set<string>>();
    for (const n of vnodes) m.set(n.id, new Set([n.id]));
    for (const e of vedges) {
      m.get(e.a)?.add(e.b);
      m.get(e.b)?.add(e.a);
    }
    return m;
  }, [vnodes, vedges]);

  useEffect(() => {
    let cancelled = false;
    void runElk(vnodes, vedges, layout).then((laid) => {
      if (!cancelled) setBase(laid);
    });
    return () => {
      cancelled = true;
    };
  }, [vnodes, vedges, layout]);

  // Fit once React Flow has measured the freshly-laid-out nodes. Deferred a
  // frame: fitView run synchronously as the nodes land no-ops (the pane hasn't
  // taken the new node bounds into its store yet), leaving the viewport at 1:1.
  useEffect(() => {
    if (!inited || !base.nodes.length) return;
    const h = requestAnimationFrame(() => rf.fitView({ padding: 0.12, duration: 0 }));
    return () => cancelAnimationFrame(h);
  }, [inited, base.nodes, rf]);

  const nodes = useMemo<Node<CardData>[]>(() => {
    const hood = hover ? adj.get(hover) : null;
    return base.nodes.map((n) => ({
      ...n,
      data: { ...n.data, sel: n.id === selectedId, dim: hood ? !hood.has(n.id) : false },
    }));
  }, [base.nodes, hover, selectedId, adj]);

  return (
    <ReactFlow
      nodes={nodes}
      edges={[]}
      nodeTypes={nodeTypes}
      fitView
      fitViewOptions={{ padding: 0.12 }}
      nodesDraggable={false}
      nodesConnectable={false}
      elementsSelectable={false}
      minZoom={0.05}
      maxZoom={2.5}
      proOptions={{ hideAttribution: true }}
      onNodeClick={(_e, n) => {
        if ((n.data as CardData).cls === "file") onSelectFile(n.id);
        else onDrill(n.id);
      }}
      onNodeMouseEnter={(e, n) => {
        setHover(n.id);
        onHover(n.id, e.clientX, e.clientY, (n.data as CardData).info);
      }}
      onNodeMouseMove={(e, n) => onHover(n.id, e.clientX, e.clientY, (n.data as CardData).info)}
      onNodeMouseLeave={() => {
        setHover(null);
        onHover(null);
      }}
    >
      <ViewportPortal>
        <EdgesLayer edges={base.edges} hover={hover} />
      </ViewportPortal>
      <Background gap={20} color="var(--border)" />
    </ReactFlow>
  );
}

// --- vanilla mount bridge ---------------------------------------------------

export interface GraphHandle {
  update: (props: GraphProps) => void;
  destroy: () => void;
}

export function mountCodegraphGraph(el: HTMLElement): GraphHandle {
  const root: Root = createRoot(el);
  return {
    update(props) {
      root.render(
        <ReactFlowProvider>
          <GraphInner {...props} />
        </ReactFlowProvider>,
      );
    },
    destroy() {
      root.unmount();
    },
  };
}
