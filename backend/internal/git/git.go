package git

// Git — a read-only dashboard over each scope's repos: current branch,
// working-tree state, upstream ahead/behind, and the recent commits. Like the
// code graph, it only ever touches a repo root the scope RESOLVED to (via
// cgRepos → session cwds), never an arbitrary ?path, so the endpoint can't be
// steered at a filesystem it wasn't already given. Every git shell-out is
// best-effort and timeout-guarded: a missing repo / missing git / slow disk
// degrades to a blank field, never a page error. wyac shells `git log` /
// `status` / `branch` — read-only plumbing, nothing that mutates a repo.

import (
	"context"
	"github.com/go-chi/chi/v5"
	"net/http"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/repos"
)

// gitCommit is one row of the recent-commits list.
type gitCommit struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	When    time.Time `json:"when"`
}

// gitRepo is one repo's snapshot. IsRepo=false means the resolved root isn't a
// git work tree (or git couldn't answer) — the card still renders, marked so.
type gitRepo struct {
	Root        string      `json:"root"`
	Folder      string      `json:"folder"` // the Claude folder that led here
	IsRepo      bool        `json:"isRepo"`
	Branch      string      `json:"branch"`
	Detached    bool        `json:"detached"` // HEAD is a bare commit, Branch holds its short hash
	Staged      int         `json:"staged"`
	Unstaged    int         `json:"unstaged"`
	Untracked   int         `json:"untracked"`
	HasUpstream bool        `json:"hasUpstream"`
	Ahead       int         `json:"ahead"`
	Behind      int         `json:"behind"`
	Commits     []gitCommit `json:"commits"`
}

const (
	gitLogLimit   = 20
	gitLogPageMax = 100        // ceiling on one "load more" page, so a crafted limit can't ask for the whole history
	gitBranchMax  = 60         // branches we spend an ahead/behind call on
	gitDiffMax    = 256 * 1024 // patch byte cap, so one huge commit can't bloat a response
)

// runGit runs a git subcommand in root, best-effort with a short timeout.
// ok=false on any failure (not a repo, no git, timeout, non-zero exit).
func runGit(root string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	full := append([]string{"-C", root}, args...)
	out, err := exec.CommandContext(ctx, "git", full...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// RemoteURL reads a remote's configured URL. This is the only piece of git
// the GitHub package needs, and it is exported deliberately narrow rather
// than exposing the general shell-out: every git call in this package is
// read-only plumbing, and that stays a property of the surface instead of a
// convention a future caller could quietly break by reaching for runGit.
func RemoteURL(root, remote string) (string, bool) {
	return runGit(root, "remote", "get-url", remote)
}

// RepoName is what the repo calls itself: the last segment of its origin URL,
// which is the name on the host rather than whatever the checkout's directory
// happens to be called — the two differ often enough to matter (a directory
// named luci_web_blog-frontend cloned from a repo named luci_dev). Falls back
// to the directory name when there is no remote to ask.
func RepoName(root string) string {
	if url, ok := RemoteURL(root, "origin"); ok {
		if n := repoNameFromURL(url); n != "" {
			return n
		}
	}
	return filepath.Base(root)
}

// repoNameFromURL takes the last path segment of a clone URL, in any of the
// shapes git accepts: https://host/owner/name.git, git@host:owner/name.git,
// ssh://host/owner/name, with or without the .git and a trailing slash.
func repoNameFromURL(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// IsRepo reports whether dir is inside a git work tree — the check a stored
// binding needs before anything downstream trusts it, since a bound path can
// be moved or deleted long after it was chosen.
func IsRepo(dir string) bool {
	out, ok := runGit(dir, "rev-parse", "--is-inside-work-tree")
	return ok && out == "true"
}

// CanonicalRoot answers which REPO a directory belongs to, which is not the
// same question as which directory you are standing in: a linked worktree has
// its own path and its own top-level, so two cwds in one repo look like two
// repos to anything that compares paths. The common dir is shared by every
// worktree, so stripping its trailing "/.git" yields the one checkout they all
// belong to. Exported as narrowly as RemoteURL above, and read-only likewise.
//
// Not hypothetical here: an agent worktree under .claude/worktrees was the
// newest session cwd for a folder, so the folder resolved to a path no other
// folder could ever match. Falls back to dir when git cannot answer (not a
// repo, or a git too old for --path-format).
func CanonicalRoot(dir string) string {
	// The common dir points INSIDE the repo's storage, and only a plain
	// checkout keeps that at "<root>/.git". A submodule keeps it at
	// "<super>/.git/modules/<name>", so trimming blindly hands back a path that
	// is not a working tree at all — which is what the binding picker offered
	// until this checked. Trim only the shape it can trim; otherwise ask where
	// the working tree actually starts.
	if out, ok := runGit(dir, "rev-parse", "--path-format=absolute", "--git-common-dir"); ok {
		if root, cut := strings.CutSuffix(out, "/.git"); cut && root != "" {
			return root
		}
	}
	if top, ok := runGit(dir, "rev-parse", "--show-toplevel"); ok && top != "" {
		return top
	}
	return dir
}

// gitSnapshot fills one repo from its root. The work-tree check gates the rest;
// branch / status / upstream / log are then each independent and best-effort,
// so a single failing command leaves only its own field blank.
func gitSnapshot(root, folder string) gitRepo {
	r := gitRepo{Root: root, Folder: folder}
	if inside, ok := runGit(root, "rev-parse", "--is-inside-work-tree"); !ok || inside != "true" {
		return r
	}
	r.IsRepo = true

	if br, ok := runGit(root, "rev-parse", "--abbrev-ref", "HEAD"); ok {
		if br == "HEAD" {
			r.Detached = true
			if sh, ok := runGit(root, "rev-parse", "--short", "HEAD"); ok {
				r.Branch = sh
			}
		} else {
			r.Branch = br
		}
	}

	if st, ok := runGit(root, "status", "--porcelain"); ok {
		r.Staged, r.Unstaged, r.Untracked = parseGitStatus(st)
	}

	// --left-right --count over the symmetric difference prints "<behind>\t<ahead>";
	// no upstream configured makes git exit non-zero, so HasUpstream stays false.
	if ab, ok := runGit(root, "rev-list", "--left-right", "--count", "@{upstream}...HEAD"); ok {
		if behind, ahead, ok := parseAheadBehind(ab); ok {
			r.HasUpstream = true
			r.Behind, r.Ahead = behind, ahead
		}
	}

	r.Commits = gitLogPage(root, 0, gitLogLimit, gitLogFilter{})
	return r
}

// parseGitStatus counts `git status --porcelain` (v1) lines. Column X is the
// index (staged) state, Y the work-tree state; "??" is untracked. A line can be
// both staged and unstaged (e.g. "MM"), so the two are counted independently.
func parseGitStatus(s string) (staged, unstaged, untracked int) {
	if s == "" {
		return 0, 0, 0
	}
	for _, line := range strings.Split(s, "\n") {
		if len(line) < 2 {
			continue
		}
		x, y := line[0], line[1]
		if x == '?' && y == '?' {
			untracked++
			continue
		}
		if x != ' ' && x != '?' {
			staged++
		}
		if y != ' ' && y != '?' {
			unstaged++
		}
	}
	return
}

// parseAheadBehind parses the "<behind>\t<ahead>" line from
// `git rev-list --left-right --count @{upstream}...HEAD`. ok=false if the shape
// isn't two integers.
func parseAheadBehind(s string) (behind, ahead int, ok bool) {
	f := strings.Fields(s)
	if len(f) != 2 {
		return 0, 0, false
	}
	b, err1 := strconv.Atoi(f[0])
	a, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return b, a, true
}

// gitLogFormat is the unit-separator layout parseGitLog expects — kept in one
// place so the snapshot and the paged reader can't drift apart.
const gitLogFormat = "--pretty=format:%h%x1f%s%x1f%an%x1f%cI"

// gitLogPage reads one page of history (the detail view's "load more"): the same
// commit shape as the snapshot's recent list, just offset by skip.
// gitLogFilter narrows a history page. The commits tab filters HERE rather than
// in the browser because the list is paged: filtering only what's loaded would
// read as "no results" when the match is simply further back.
type gitLogFilter struct {
	NoMerges bool
	Grep     string // matched against the subject/body, literal and case-insensitive
	Author   string
}

// gitFilterMax caps a needle's length — anything longer is a mistake, not a query.
const gitFilterMax = 200

func clampFilter(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > gitFilterMax {
		s = s[:gitFilterMax]
	}
	return s
}

// args renders the filter as git flags. Each value goes in a SINGLE `--flag=value`
// token, so a needle starting with "-" stays a needle instead of being read as
// another flag; there's no shell in the path, so nothing else needs escaping.
// --fixed-strings keeps a search literal — typing "feat(web):" shouldn't be a regex.
func (f gitLogFilter) args() []string {
	var a []string
	if f.NoMerges {
		a = append(a, "--no-merges")
	}
	if g := clampFilter(f.Grep); g != "" {
		a = append(a, "--grep="+g, "--fixed-strings", "--regexp-ignore-case")
	}
	if au := clampFilter(f.Author); au != "" {
		a = append(a, "--author="+au)
	}
	return a
}

func gitLogPage(root string, skip, limit int, f gitLogFilter) []gitCommit {
	args := append([]string{"log", "--skip=" + strconv.Itoa(skip), "-n", strconv.Itoa(limit), gitLogFormat}, f.args()...)
	out, ok := runGit(root, args...)
	if !ok {
		return nil
	}
	return parseGitLog(out)
}

// gitAuthorScan bounds how far back the author picker looks — enough to cover a
// team repo's active contributors without walking a whole history.
const gitAuthorScan = 500

// gitAuthors lists the distinct commit authors in that window, sorted.
func gitAuthors(root string) []string {
	out, ok := runGit(root, "log", "-n", strconv.Itoa(gitAuthorScan), "--pretty=%an")
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	list := []string{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l == "" || seen[l] {
			continue
		}
		seen[l] = true
		list = append(list, l)
	}
	sort.Strings(list)
	return list
}

// parseGitLog parses the unit-separator (\x1f) delimited log lines emitted by
// gitLogFormat: hash, subject, author, ISO-8601 date.
// Malformed lines are skipped; an unparseable date leaves a zero When.
func parseGitLog(s string) []gitCommit {
	if s == "" {
		return nil
	}
	var out []gitCommit
	for _, line := range strings.Split(s, "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 4 {
			continue
		}
		c := gitCommit{Hash: f[0], Subject: f[1], Author: f[2]}
		if t, err := time.Parse(time.RFC3339, f[3]); err == nil {
			c.When = t
		}
		out = append(out, c)
	}
	return out
}

// gitRepos snapshots every repo the scope resolves to. Resolution is shared
// with the code graph (cgRepos), so the two tabs list the same repo set. The
// snapshots run concurrently — the unscoped case can resolve to many repos, and
// each carries up to a handful of ×3s-timeout git calls — bounded so a large
// scope doesn't fork one goroutine per repo unbounded.
func gitRepos(rr *repos.Resolver, scope string) []gitRepo {
	roots := rr.Bound(scope)
	out := make([]gitRepo, len(roots))
	const workers = 6
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, rp := range roots {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, root, folder string) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = gitSnapshot(root, folder)
		}(i, rp.Root, rp.Folder)
	}
	wg.Wait()

	// Real repos first, then by folder name — a resolved-but-not-a-repo root
	// sinks to the bottom rather than jostling the useful cards.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsRepo != out[j].IsRepo {
			return out[i].IsRepo
		}
		return out[i].Folder < out[j].Folder
	})
	return out
}

// capDiff limits a patch to gitDiffMax so a huge commit can't bloat the
// response; truncated=true tells the UI to say the diff was cut.
func capDiff(s string) (string, bool) {
	if len(s) <= gitDiffMax {
		return s, false
	}
	return s[:gitDiffMax], true
}

// gitFileChange is one changed file's line delta. Add/Del of -1 means binary
// (git prints "-" for a binary file's numstat).
type gitFileChange struct {
	Path string `json:"path"`
	Add  int    `json:"add"`
	Del  int    `json:"del"`
}

// parseNumstat parses `git … --numstat` rows ("<add>\t<del>\t<path>"; a binary
// file is "-\t-\t<path>"). Malformed rows are skipped.
func parseNumstat(s string) []gitFileChange {
	var out []gitFileChange
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		fc := gitFileChange{Path: f[2]}
		if f[0] == "-" {
			fc.Add = -1
		} else {
			fc.Add, _ = strconv.Atoi(f[0])
		}
		if f[1] == "-" {
			fc.Del = -1
		} else {
			fc.Del, _ = strconv.Atoi(f[1])
		}
		out = append(out, fc)
	}
	return out
}

// gitCommitDetail is the drill-down for one commit: full message, author, the
// per-file line deltas, and the (capped) patch.
type gitCommitDetail struct {
	Hash      string          `json:"hash"`
	Subject   string          `json:"subject"`
	Body      string          `json:"body"`
	Author    string          `json:"author"`
	Email     string          `json:"email"`
	When      time.Time       `json:"when"`
	Files     []gitFileChange `json:"files"`
	Diff      string          `json:"diff"`
	Truncated bool            `json:"truncated"`
}

// gitShow builds the detail for one commit. The ref is resolved to a real commit
// SHA first (rev-parse --verify), so the later show calls can't be steered by a
// crafted ref; ok=false if the ref doesn't name a commit.
func gitShow(root, ref string) (gitCommitDetail, bool) {
	full, ok := runGit(root, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if !ok || full == "" {
		return gitCommitDetail{}, false
	}
	d := gitCommitDetail{Hash: full}
	if meta, ok := runGit(root, "show", "-s", "--format=%s%x1f%b%x1f%an%x1f%ae%x1f%cI", full); ok {
		f := strings.SplitN(meta, "\x1f", 5)
		if len(f) == 5 {
			d.Subject, d.Body, d.Author, d.Email = f[0], strings.TrimRight(f[1], "\n"), f[2], f[3]
			if t, err := time.Parse(time.RFC3339, strings.TrimSpace(f[4])); err == nil {
				d.When = t
			}
		}
	}
	// --first-parent so a MERGE commit shows the diff it brought in (against its
	// first parent) instead of git's default empty merge diff; on a normal commit
	// it's a no-op (byte-identical).
	if ns, ok := runGit(root, "show", "--numstat", "--format=", "--first-parent", full); ok {
		d.Files = parseNumstat(ns)
	}
	if patch, ok := runGit(root, "show", "--format=", "-p", "--first-parent", full); ok {
		d.Diff, d.Truncated = capDiff(patch)
	}
	return d, true
}

// gitDiff is the working-tree drill-down: tracked changes vs HEAD (numstat +
// capped patch) plus the untracked-file list.
type gitDiff struct {
	Files     []gitFileChange `json:"files"`
	Untracked []string        `json:"untracked"`
	Diff      string          `json:"diff"`
	Truncated bool            `json:"truncated"`
}

func gitWorktreeDiff(root string) gitDiff {
	var d gitDiff
	if ns, ok := runGit(root, "diff", "--numstat", "HEAD"); ok {
		d.Files = parseNumstat(ns)
	}
	if patch, ok := runGit(root, "diff", "HEAD"); ok {
		d.Diff, d.Truncated = capDiff(patch)
	}
	if un, ok := runGit(root, "ls-files", "--others", "--exclude-standard"); ok && un != "" {
		d.Untracked = strings.Split(un, "\n")
	}
	return d
}

// gitBranch is one branch: where its tip sits vs the default branch, whether
// it's merged, and its last commit.
type gitBranch struct {
	Name      string    `json:"name"`
	IsRemote  bool      `json:"isRemote"`
	IsCurrent bool      `json:"isCurrent"`
	Subject   string    `json:"subject"`
	When      time.Time `json:"when"`
	Ahead     int       `json:"ahead"`
	Behind    int       `json:"behind"`
	Merged    bool      `json:"merged"`
}

// parseBranchRefs parses for-each-ref rows over refs/heads + refs/remotes:
// "<short>\x1f<HEAD marker>\x1f<subject>\x1f<iso date>\x1f<full refname>". The
// refs/remotes/ prefix marks a remote branch; "*" in the HEAD marker column
// (only on refs/heads) marks the current branch; the origin/HEAD alias is dropped.
func parseBranchRefs(s string) []gitBranch {
	var out []gitBranch
	for _, line := range strings.Split(s, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x1f", 5)
		if len(f) != 5 {
			continue
		}
		name, head, subject, date, full := f[0], f[1], f[2], f[3], f[4]
		// refs/remotes/origin/HEAD (the remote's default-branch alias) shortens
		// to just "origin", so test the FULL refname, not the short name.
		if strings.HasSuffix(full, "/HEAD") {
			continue
		}
		b := gitBranch{
			Name:      name,
			Subject:   subject,
			IsRemote:  strings.HasPrefix(full, "refs/remotes/"),
			IsCurrent: strings.TrimSpace(head) == "*",
		}
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(date)); err == nil {
			b.When = t
		}
		out = append(out, b)
	}
	return out
}

// gitBranches lists a repo's branches (local + remote-tracking), newest-tip
// first, each with merged state and — for the first gitBranchMax — ahead/behind
// vs the default branch. ahead/behind is capped because it costs a git call per
// branch and a repo can carry hundreds of remote refs.
func gitBranches(root string) []gitBranch {
	raw, ok := runGit(root, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)%1f%(HEAD)%1f%(contents:subject)%1f%(committerdate:iso-strict)%1f%(refname)",
		"refs/heads", "refs/remotes")
	if !ok {
		return nil
	}
	branches := parseBranchRefs(raw)

	def := ""
	if d, ok := runGit(root, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ok {
		def = d // e.g. origin/main
	}
	merged := map[string]bool{}
	if def != "" {
		if m, ok := runGit(root, "branch", "--format=%(refname:short)", "--merged", def); ok {
			for _, b := range strings.Split(m, "\n") {
				if b != "" {
					merged[b] = true
				}
			}
		}
	}
	for i := range branches {
		branches[i].Merged = merged[branches[i].Name]
		if def == "" || i >= gitBranchMax || branches[i].Name == def {
			continue
		}
		if ab, ok := runGit(root, "rev-list", "--left-right", "--count", def+"..."+branches[i].Name); ok {
			if behind, ahead, ok := parseAheadBehind(ab); ok {
				branches[i].Behind, branches[i].Ahead = behind, ahead
			}
		}
	}
	return branches
}

func Register(router chi.Router, rr *repos.Resolver) {
	// scoped validates ?repo against the scope's resolved roots and returns the
	// root; on a miss it writes the 404 and returns ok=false.
	scoped := func(w http.ResponseWriter, r *http.Request) (string, bool) {
		root := r.URL.Query().Get("repo")
		if !rr.BoundRoot(r.URL.Query().Get("scope"), root) {
			httpx.WriteJSONError(w, http.StatusNotFound, "unknown repo for this scope")
			return "", false
		}
		return root, true
	}

	router.Get("/api/git", func(w http.ResponseWriter, r *http.Request) {
		httpx.WriteJSON(w, struct {
			Repos []gitRepo `json:"repos"`
		}{Repos: gitRepos(rr, r.URL.Query().Get("scope"))})
	})

	router.Get("/api/git/commit", func(w http.ResponseWriter, r *http.Request) {
		root, ok := scoped(w, r)
		if !ok {
			return
		}
		d, ok := gitShow(root, r.URL.Query().Get("hash"))
		if !ok {
			httpx.WriteJSONError(w, http.StatusNotFound, "unknown commit")
			return
		}
		httpx.WriteJSON(w, d)
	})

	router.Get("/api/git/diff", func(w http.ResponseWriter, r *http.Request) {
		root, ok := scoped(w, r)
		if !ok {
			return
		}
		httpx.WriteJSON(w, gitWorktreeDiff(root))
	})

	// One page of history past the snapshot's first gitLogLimit commits — the
	// detail view's "load more". skip/limit are clamped, so a crafted request
	// can't ask for an unbounded slice of a huge repo.
	router.Get("/api/git/commits", func(w http.ResponseWriter, r *http.Request) {
		root, ok := scoped(w, r)
		if !ok {
			return
		}
		skip, _ := strconv.Atoi(r.URL.Query().Get("skip"))
		if skip < 0 {
			skip = 0
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = gitLogLimit
		}
		if limit > gitLogPageMax {
			limit = gitLogPageMax
		}
		f := gitLogFilter{
			NoMerges: r.URL.Query().Get("nomerges") == "1",
			Grep:     r.URL.Query().Get("q"),
			Author:   r.URL.Query().Get("author"),
		}
		resp := struct {
			Commits []gitCommit `json:"commits"`
			Authors []string    `json:"authors,omitempty"`
		}{Commits: gitLogPage(root, skip, limit, f)}
		// The picker only needs filling on the first page of a view.
		if skip == 0 {
			resp.Authors = gitAuthors(root)
		}
		httpx.WriteJSON(w, resp)
	})

	router.Get("/api/git/branches", func(w http.ResponseWriter, r *http.Request) {
		root, ok := scoped(w, r)
		if !ok {
			return
		}
		httpx.WriteJSON(w, struct {
			Branches []gitBranch `json:"branches"`
		}{Branches: gitBranches(root)})
	})
}
