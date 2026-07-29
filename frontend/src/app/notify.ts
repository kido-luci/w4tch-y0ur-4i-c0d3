// Browser (OS) notifications for background sessions — 100% client-side, so the
// server stays read-only/local (no process spawn, no egress). Fires on:
//   • attention — Claude's Notification hook (needs input / permission), instant
//   • finished  — a session's Stop hook, debounced: only once it's been quiet
//                 for FINISH_QUIET_MS (so an active back-and-forth doesn't spam)
// Requires the viewer tab to be open and notifications enabled by the user.

import { subscribeRawEvents } from "../api";
import type { Session } from "../api";
import { navigate } from "../scope";

const STORAGE_KEY = "wyac-notify";
const FINISH_QUIET_MS = 60_000;

let started = false;
const pending = new Map<string, { title: string; handle: number }>();

export function notifySupported(): boolean {
  return typeof Notification !== "undefined";
}

export function isNotifyOn(): boolean {
  return (
    notifySupported() &&
    localStorage.getItem(STORAGE_KEY) === "1" &&
    Notification.permission === "granted"
  );
}

/** Flip the enabled flag, requesting permission when turning on. Returns new state. */
export async function toggleNotify(): Promise<boolean> {
  if (!notifySupported()) return false;
  if (isNotifyOn()) {
    localStorage.setItem(STORAGE_KEY, "0");
    return false;
  }
  let perm = Notification.permission;
  if (perm === "default") perm = await Notification.requestPermission();
  if (perm === "granted") {
    localStorage.setItem(STORAGE_KEY, "1");
    return true;
  }
  return false; // denied
}

function fire(title: string, body: string, sessionId: string): void {
  if (!isNotifyOn()) return;
  try {
    const n = new Notification(title, { body, tag: sessionId });
    n.onclick = () => {
      window.focus();
      navigate(`/claude/session/${encodeURIComponent(sessionId)}`);
      n.close();
    };
  } catch {
    /* Notification construction can throw in some contexts; ignore. */
  }
}

function scheduleFinish(id: string, title: string): void {
  const existing = pending.get(id);
  if (existing) clearTimeout(existing.handle);
  const handle = window.setTimeout(() => {
    pending.delete(id);
    fire("✓ Session finished", title || "untitled session", id);
  }, FINISH_QUIET_MS);
  pending.set(id, { title, handle });
}

/** Any activity for a session pushes its pending "finished" notification out. */
function bumpActivity(id: string): void {
  const p = pending.get(id);
  if (p) scheduleFinish(id, p.title);
}

/** One app-lifetime listener on the shared SSE transport. Call once at startup. */
export function initNotifications(): void {
  if (started) return;
  started = true;
  subscribeRawEvents((type, data) => {
    switch (type) {
      case "session-updated":
        bumpActivity((data as Session).id);
        break;
      case "session-stopped": {
        const s = data as { id: string; title: string };
        scheduleFinish(s.id, s.title);
        break;
      }
      case "session-attention": {
        const a = data as { id: string; title: string; message: string };
        const body = a.message ? `${a.title || "session"} — ${a.message}` : a.title || "session";
        fire("⚠ Needs your input", body, a.id);
        break;
      }
    }
  });
}
