import { getJSON, buildQuery } from "./client";

// --- ship history ------------------------------------------------------------

// One recorded `make check` / `make release` run, dropped into ~/.wyac/ships
// by scripts/wyac-ship and indexed by the server (see internal/ships). `log` rides
// along only when the query asks (withLog) — it is the payload's whole weight.
export interface ShipRecord {
  file: string;
  project: string;
  kind: string; // check | release
  version?: string;
  sha?: string;
  exit: number;
  durationMs: number;
  ts: string;
  log?: string;
  // The session running the project when the run happened, if the server
  // could match one — absent when none did.
  sessionId?: string;
  sessionTitle?: string;
}

// A capped list that never reads as the whole answer: `total` counts
// everything the filter matched, `ships` carries at most `limit`.
export interface ShipsResult {
  ships: ShipRecord[];
  total: number;
}

export function getShips(
  days: number,
  project?: string,
  limit?: number,
  withLog?: boolean,
): Promise<ShipsResult> {
  return getJSON<ShipsResult>(
    `/api/ships${buildQuery({ days, project, limit, log: withLog ? 1 : undefined })}`,
  );
}
