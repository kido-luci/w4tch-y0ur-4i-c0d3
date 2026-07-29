import { getJSON, buildQuery } from "./client";

// --- transcript search -------------------------------------------------------

// One match inside one session's transcript. Snippets are cut server-side to a
// fixed width — enough to recognise the moment, never the whole message.
export interface SearchHit {
  sessionId: string;
  title: string;
  project: string;
  ts: string;
  role: string; // user | assistant
  snippet: string;
}

// A query-time grep — nothing is indexed. `matched` counts every hit found;
// `hits` carries at most `limit` of them, and `truncated` says so.
export interface SearchResult {
  hits: SearchHit[];
  matched: number;
  files: number; // transcripts opened
  truncated: boolean;
  tookMs: number;
}

export function search(
  q: string,
  days: number,
  project?: string,
  limit?: number,
): Promise<SearchResult> {
  return getJSON<SearchResult>(`/api/search${buildQuery({ q, days, project, limit })}`);
}
