import { getJSON, buildQuery } from "./client";

// --- git ----------------------------------------------------------------------

export interface GitCommit {
  hash: string;
  subject: string;
  author: string;
  when: string; // ISO-8601; "" / zero when git's date was unparseable
}

export interface GitRepo {
  root: string;
  folder: string;
  isRepo: boolean;
  branch: string;
  detached: boolean; // branch holds a short hash, not a branch name
  staged: number;
  unstaged: number;
  untracked: number;
  hasUpstream: boolean;
  ahead: number;
  behind: number;
  commits: GitCommit[] | null; // null when a repo has no commits / log failed
}

export interface GitResponse {
  repos: GitRepo[];
}

export function getGit(project?: string): Promise<GitResponse> {
  return getJSON<GitResponse>(`/api/git${buildQuery({ project })}`);
}

// --- git drill-down: branches / commit diff / working-tree diff ---------------

export interface GitFileChange {
  path: string;
  add: number; // -1 = binary
  del: number;
}

export interface GitBranch {
  name: string;
  isRemote: boolean;
  isCurrent: boolean;
  subject: string;
  when: string;
  ahead: number;
  behind: number;
  merged: boolean;
}

export interface GitCommitDetail {
  hash: string;
  subject: string;
  body: string;
  author: string;
  email: string;
  when: string;
  files: GitFileChange[] | null;
  diff: string;
  truncated: boolean;
}

export interface GitDiff {
  files: GitFileChange[] | null;
  untracked: string[] | null;
  diff: string;
  truncated: boolean;
}

export function getGitBranches(repo: string, project?: string): Promise<{ branches: GitBranch[] | null }> {
  return getJSON(`/api/git/branches${buildQuery({ repo, project })}`);
}

/** Commit-list filters. Applied server-side (`git log` flags) rather than in the
    browser, because the list is paged: filtering only the loaded rows would read
    as "no results" when the match is simply further back. */
export interface GitLogFilter {
  nomerges?: boolean;
  q?: string;
  author?: string;
}

/** One page of history — the detail view's list and its "load more". `authors`
    comes back on the first page only, to fill the author picker. */
export function getGitCommits(
  repo: string,
  skip: number,
  limit: number,
  project?: string,
  filter?: GitLogFilter,
): Promise<{ commits: GitCommit[] | null; authors?: string[] | null }> {
  return getJSON(
    `/api/git/commits${buildQuery({
      repo,
      skip,
      limit,
      project,
      nomerges: filter?.nomerges ? 1 : undefined,
      q: filter?.q || undefined,
      author: filter?.author || undefined,
    })}`,
  );
}

export function getGitCommit(repo: string, hash: string, project?: string): Promise<GitCommitDetail> {
  return getJSON<GitCommitDetail>(`/api/git/commit${buildQuery({ repo, hash, project })}`);
}

export function getGitDiff(repo: string, project?: string): Promise<GitDiff> {
  return getJSON<GitDiff>(`/api/git/diff${buildQuery({ repo, project })}`);
}

// --- git drill-down: GitHub (PRs / issues / CI runs) --------------------------

export interface GitPR {
  number: number;
  title: string;
  author: string;
  state: string; // OPEN / MERGED / CLOSED
  draft: boolean;
  branch: string;
  review: string; // approved / changes_requested / review_required / ""
  checks: string; // success / failure / pending / ""
  url: string;
  createdAt: string;
  updatedAt: string;
}

export interface GitIssue {
  number: number;
  title: string;
  author: string;
  labels: string[] | null;
  url: string;
  updatedAt: string;
}

export interface GitRun {
  title: string;
  workflow: string;
  status: string; // completed / in_progress / queued
  conclusion: string; // success / failure / cancelled / ""
  branch: string;
  url: string;
  createdAt: string;
}

export function getGitPRs(repo: string, project?: string): Promise<{ supported: boolean; prs: GitPR[] | null }> {
  return getJSON(`/api/git/prs${buildQuery({ repo, project })}`);
}

export function getGitActivity(
  repo: string,
  project?: string,
): Promise<{ supported: boolean; issues: GitIssue[] | null; runs: GitRun[] | null }> {
  return getJSON(`/api/git/activity${buildQuery({ repo, project })}`);
}
