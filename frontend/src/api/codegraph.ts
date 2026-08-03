import { getJSON, buildQuery } from "./client";

// --- code graph (the `graph` tab of /project/<scope>/code/<folder>) ------------
// Read-only views over each repo's .codegraph/codegraph.db (internal/codegraph).

export interface CGRepo {
  root: string;
  folder: string; // the Claude folder that resolved here
  hasIndex: boolean;
  indexedAt: string;
  files: number;
  nodes: number;
  edges: number;
  commitsSince: number; // commits landed after indexing; -1 = git couldn't say
}

export interface CGFile {
  path: string;
  language: string;
  symbols: number;
  isTest: boolean;
}

export interface CGEdge {
  from: string;
  to: string;
  kind: string; // calls | imports | references | instantiates | extends
  weight: number; // symbol edges collapsed into this file edge
}

export interface CGGraph {
  files: CGFile[];
  edges: CGEdge[];
}

export interface CGResponse {
  repos: CGRepo[];
  active: string; // root whose graph came back; "" when none has an index
  graph: CGGraph | null;
}

export interface CGSymbol {
  id: string;
  name: string;
  kind: string;
  file?: string;
  line: number;
  endLine?: number;
  signature?: string;
  via?: string; // connecting edge kind, on callers/callees
  count?: number; // >1 = that many parallel edges collapsed into the row
}

export interface CGSymbolDetail {
  node: CGSymbol;
  callers: CGSymbol[];
  callees: CGSymbol[];
}

export function getCodegraph(scope?: string, repo?: string): Promise<CGResponse> {
  return getJSON<CGResponse>(`/api/codegraph${buildQuery({ scope, repo })}`);
}

export function getCodegraphFile(repo: string, path: string, scope?: string): Promise<CGSymbol[]> {
  return getJSON<CGSymbol[]>(`/api/codegraph/file${buildQuery({ repo, path, scope })}`);
}

export function searchCodegraphSymbols(repo: string, q: string, scope?: string): Promise<CGSymbol[]> {
  return getJSON<CGSymbol[]>(`/api/codegraph/symbols${buildQuery({ repo, q, scope })}`);
}

export function getCodegraphSymbol(repo: string, id: string, scope?: string): Promise<CGSymbolDetail> {
  return getJSON<CGSymbolDetail>(`/api/codegraph/symbols${buildQuery({ repo, id, scope })}`);
}
