import type { SessionDetail } from "./sessions";

// --- shared SSE transport -----------------------------------------------------
// Browsers cap HTTP/1.1 connections at ~6 per host, and an EventSource holds
// one forever — a handful of viewer tabs (each previously opening TWO streams:
// view + notifications) starved the pool and new tabs hung blank. Now exactly
// ONE tab per browser holds the real /api/events connection: tabs race for a
// Web Lock, the winner ("leader") opens the EventSource and re-publishes every
// event on a BroadcastChannel; the rest just listen. When the leader tab dies
// the lock auto-releases and the next waiter takes over. Browsers without
// those APIs fall back to one connection per tab (the old behavior).

type RawHandler = (type: string, data: unknown) => void;

const SSE_TYPES = [
  "session-updated",
  "session-attention",
  "session-stopped",
  "todos-updated",
  "board-states-updated",
  "board-views-updated",
  "cycles-updated",
  "drawings-updated",
  "docs-updated",
  "groups-updated",
  "projects-updated",
  "presentation-updated",
  "ship-recorded",
];

const rawHandlers = new Set<RawHandler>();
let transportStarted = false;
const bc = typeof BroadcastChannel !== "undefined" ? new BroadcastChannel("wyac-events") : null;

function dispatch(type: string, data: unknown): void {
  for (const h of [...rawHandlers]) h(type, data);
}

// Open the real SSE connection (leader tab only). EventSource auto-reconnects
// on its own; errors are just logged.
function openSource(): void {
  const src = new EventSource("/api/events");
  for (const t of SSE_TYPES) {
    src.addEventListener(t, (evt) => {
      let data: unknown;
      try {
        data = JSON.parse((evt as MessageEvent<string>).data);
      } catch {
        return;
      }
      dispatch(t, data); // BroadcastChannel skips the sender, so dispatch locally
      bc?.postMessage({ type: t, data });
    });
  }
  src.onerror = (err) => {
    console.warn("SSE connection error (will auto-retry)", err);
  };
}

function startTransport(): void {
  if (transportStarted) return;
  transportStarted = true;
  if (bc && typeof navigator !== "undefined" && "locks" in navigator) {
    bc.onmessage = (evt) => {
      const msg = evt.data as { type?: string; data?: unknown } | null;
      if (msg?.type) dispatch(msg.type, msg.data);
    };
    void navigator.locks.request("wyac-sse-leader", () => {
      openSource();
      return new Promise<never>(() => {}); // hold leadership until the tab dies
    });
  } else {
    openSource();
  }
}

/**
 * Subscribe to the shared /api/events stream (one real connection per browser,
 * see above). The handler gets every event type raw; returns an unsubscribe.
 */
export function subscribeRawEvents(handler: RawHandler): () => void {
  rawHandlers.add(handler);
  startTransport();
  return () => {
    rawHandlers.delete(handler);
  };
}

/**
 * Subscribe to `session-updated` events with the full SessionDetail payload.
 * Returns an unsubscribe function.
 */
export function subscribeSessionEvents(
  onUpdate: (session: SessionDetail) => void,
): () => void {
  return subscribeRawEvents((type, data) => {
    if (type !== "session-updated") return;
    const session = data as SessionDetail;
    session.agents ??= [];
    onUpdate(session);
  });
}
