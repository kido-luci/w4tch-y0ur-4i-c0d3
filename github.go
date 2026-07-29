package main

// GitHub — the git tab's remote layer: pull requests, open issues, and recent
// CI runs, read through the `gh` CLI (already logged in on this machine) for the
// repos whose origin is github.com. Everything here is read-only and best-effort:
// a non-GitHub remote, a missing/failing gh, or a timeout degrades to an empty
// section, never a page error.
//
// Two operational facts this file is built around:
//   - Under launchd the process PATH has no /opt/homebrew/bin, so `gh` is found
//     by ABSOLUTE path (ghPath), not via PATH. (Auth itself works fine headless —
//     the token comes from the keychain, which a gui-domain agent can read.)
//   - gh calls are network — slow and rate-limited — so results are cached with
//     a short TTL and the handler only ever touches a scope-resolved repo root.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ghPath resolves the gh binary by absolute path: LookPath first (covers `make
// dev`, where the shell PATH has homebrew), then the usual install locations
// (covers launchd, whose PATH doesn't). "" = gh not installed → sections empty.
func ghPath() string {
	if p, err := exec.LookPath("gh"); err == nil {
		return p
	}
	for _, c := range []string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"} {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

var reGitHubRemote = regexp.MustCompile(`github\.com[:/]+([^/]+/[^/]+?)(?:\.git)?/?$`)

// ghSlug parses "owner/repo" from a repo's origin remote, but only when the host
// is github.com; ok=false for any other host (GitLab, self-hosted, no remote).
func ghSlug(root string) (slug string, ok bool) {
	url, ok := runGit(root, "remote", "get-url", "origin")
	if !ok {
		return "", false
	}
	m := reGitHubRemote.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// runGH runs a gh subcommand and returns its stdout. gh is addressed by absolute
// path and the repo by --repo <slug> (so cwd is irrelevant); ok=false on a
// missing gh, non-zero exit, or timeout.
func runGH(args ...string) ([]byte, bool) {
	gh := ghPath()
	if gh == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, args...).Output()
	if err != nil {
		return nil, false
	}
	return out, true
}

// ghCache memoises gh responses per (key) for a short TTL — gh is slow and
// rate-limited, and the UI re-fetches a section every time it's opened.
type ghCache struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]ghCacheEntry
}

type ghCacheEntry struct {
	at  time.Time
	val any
}

func newGHCache(ttl time.Duration) *ghCache {
	return &ghCache{ttl: ttl, m: map[string]ghCacheEntry{}}
}

// get returns a cached value if fresh, else calls build, caches, and returns it.
// A build returning ok=false is NOT cached, so a transient gh failure retries
// next time instead of sticking an empty section for the whole TTL.
func (c *ghCache) get(key string, now time.Time, build func() (any, bool)) (any, bool) {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Sub(e.at) < c.ttl {
		c.mu.Unlock()
		return e.val, true
	}
	c.mu.Unlock()

	val, ok := build()
	if !ok {
		return nil, false
	}
	c.mu.Lock()
	c.m[key] = ghCacheEntry{at: now, val: val}
	c.mu.Unlock()
	return val, true
}

// --- pull requests ------------------------------------------------------------

type ghPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	State     string    `json:"state"` // OPEN / MERGED / CLOSED
	Draft     bool      `json:"draft"`
	Branch    string    `json:"branch"`
	Review    string    `json:"review"` // APPROVED / CHANGES_REQUESTED / REVIEW_REQUIRED / ""
	Checks    string    `json:"checks"` // success / failure / pending / ""
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ghPRRaw mirrors the subset of `gh pr list --json` we ask for; the nested
// author/checks shapes are flattened into ghPR by ghPRs.
type ghPRRaw struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	State             string    `json:"state"`
	IsDraft           bool      `json:"isDraft"`
	HeadRefName       string    `json:"headRefName"`
	ReviewDecision    string    `json:"reviewDecision"`
	URL               string    `json:"url"`
	CreatedAt         time.Time `json:"createdAt"`
	UpdatedAt         time.Time `json:"updatedAt"`
	StatusCheckRollup []struct {
		Status     string `json:"status"`     // COMPLETED / IN_PROGRESS / QUEUED / …
		Conclusion string `json:"conclusion"` // SUCCESS / FAILURE / … (when completed)
		State      string `json:"state"`      // status-context style: SUCCESS / PENDING / …
	} `json:"statusCheckRollup"`
}

// rollupChecks reduces a PR's per-check rollup to one word: failure if any check
// failed, else pending if any is unfinished, else success; "" if there are none.
func rollupChecks(rollup []struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}) string {
	if len(rollup) == 0 {
		return ""
	}
	pending := false
	for _, c := range rollup {
		concl := strings.ToUpper(c.Conclusion)
		state := strings.ToUpper(c.State)
		if concl == "FAILURE" || concl == "TIMED_OUT" || concl == "CANCELLED" || concl == "ACTION_REQUIRED" ||
			state == "FAILURE" || state == "ERROR" {
			return "failure"
		}
		if (c.Status != "" && strings.ToUpper(c.Status) != "COMPLETED") || state == "PENDING" || state == "EXPECTED" {
			pending = true
		}
	}
	if pending {
		return "pending"
	}
	return "success"
}

func parseGHPRs(data []byte) ([]ghPR, bool) {
	var raw []ghPRRaw
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	out := make([]ghPR, 0, len(raw))
	for _, p := range raw {
		review := ""
		if p.ReviewDecision != "" {
			review = strings.ToLower(p.ReviewDecision)
		}
		out = append(out, ghPR{
			Number: p.Number, Title: p.Title, Author: p.Author.Login,
			State: p.State, Draft: p.IsDraft, Branch: p.HeadRefName,
			Review: review, Checks: rollupChecks(p.StatusCheckRollup),
			URL: p.URL, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		})
	}
	return out, true
}

func ghPRs(slug string) ([]ghPR, bool) {
	data, ok := runGH("pr", "list", "--repo", slug, "--state", "all", "--limit", "30",
		"--json", "number,title,author,state,isDraft,headRefName,reviewDecision,url,createdAt,updatedAt,statusCheckRollup")
	if !ok {
		return nil, false
	}
	return parseGHPRs(data)
}

// --- issues + CI runs ---------------------------------------------------------

type ghIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	URL       string    `json:"url"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type ghRun struct {
	Title      string    `json:"title"`
	Workflow   string    `json:"workflow"`
	Status     string    `json:"status"`     // completed / in_progress / queued
	Conclusion string    `json:"conclusion"` // success / failure / cancelled / ""
	Branch     string    `json:"branch"`
	URL        string    `json:"url"`
	CreatedAt  time.Time `json:"createdAt"`
}

type ghActivity struct {
	Issues []ghIssue `json:"issues"`
	Runs   []ghRun   `json:"runs"`
}

func parseGHIssues(data []byte) ([]ghIssue, bool) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Author struct {
			Login string `json:"login"`
		} `json:"author"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		URL       string    `json:"url"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	out := make([]ghIssue, 0, len(raw))
	for _, is := range raw {
		labels := make([]string, 0, len(is.Labels))
		for _, l := range is.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, ghIssue{
			Number: is.Number, Title: is.Title, Author: is.Author.Login,
			Labels: labels, URL: is.URL, UpdatedAt: is.UpdatedAt,
		})
	}
	return out, true
}

func parseGHRuns(data []byte) ([]ghRun, bool) {
	var raw []struct {
		DisplayTitle string    `json:"displayTitle"`
		WorkflowName string    `json:"workflowName"`
		Status       string    `json:"status"`
		Conclusion   string    `json:"conclusion"`
		HeadBranch   string    `json:"headBranch"`
		URL          string    `json:"url"`
		CreatedAt    time.Time `json:"createdAt"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, false
	}
	out := make([]ghRun, 0, len(raw))
	for _, r := range raw {
		out = append(out, ghRun{
			Title: r.DisplayTitle, Workflow: r.WorkflowName, Status: r.Status,
			Conclusion: r.Conclusion, Branch: r.HeadBranch, URL: r.URL, CreatedAt: r.CreatedAt,
		})
	}
	return out, true
}

// ghActivityFor fetches issues + runs concurrently; either half failing just
// leaves its slice empty (the section still renders the other half).
func ghActivityFor(slug string) (ghActivity, bool) {
	var act ghActivity
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if data, ok := runGH("issue", "list", "--repo", slug, "--state", "open", "--limit", "20",
			"--json", "number,title,author,labels,url,updatedAt"); ok {
			if issues, ok := parseGHIssues(data); ok {
				act.Issues = issues
			}
		}
	}()
	go func() {
		defer wg.Done()
		if data, ok := runGH("run", "list", "--repo", slug, "--limit", "15",
			"--json", "displayTitle,workflowName,status,conclusion,headBranch,url,createdAt"); ok {
			if runs, ok := parseGHRuns(data); ok {
				act.Runs = runs
			}
		}
	}()
	wg.Wait()
	return act, true
}

// registerGitHubAPI mounts the GitHub sections of the git tab. Each handler
// validates ?repo against the scope's resolved roots, derives the github slug
// from its origin, and returns {supported:false} for a non-GitHub repo so the UI
// shows an honest "no GitHub remote" state instead of an error.
func registerGitHubAPI(mux *http.ServeMux, rr *repoResolver) {
	prCache := newGHCache(60 * time.Second)
	actCache := newGHCache(60 * time.Second)

	slugFor := func(w http.ResponseWriter, r *http.Request) (string, bool, bool) {
		root := r.URL.Query().Get("repo")
		if !rr.ResolveRoot(r.URL.Query().Get("project"), root) {
			writeJSONError(w, http.StatusNotFound, "unknown repo for this scope")
			return "", false, false
		}
		slug, ok := ghSlug(root)
		return slug, ok, true
	}

	mux.HandleFunc("GET /api/git/prs", func(w http.ResponseWriter, r *http.Request) {
		slug, isGH, valid := slugFor(w, r)
		if !valid {
			return
		}
		if !isGH {
			writeJSON(w, struct {
				Supported bool   `json:"supported"`
				PRs       []ghPR `json:"prs"`
			}{Supported: false})
			return
		}
		v, ok := prCache.get(slug, time.Now(), func() (any, bool) { return ghPRs(slug) })
		if !ok {
			writeJSONError(w, http.StatusBadGateway, "gh unavailable")
			return
		}
		writeJSON(w, struct {
			Supported bool   `json:"supported"`
			PRs       []ghPR `json:"prs"`
		}{Supported: true, PRs: v.([]ghPR)})
	})

	mux.HandleFunc("GET /api/git/activity", func(w http.ResponseWriter, r *http.Request) {
		slug, isGH, valid := slugFor(w, r)
		if !valid {
			return
		}
		if !isGH {
			writeJSON(w, struct {
				Supported bool `json:"supported"`
				ghActivity
			}{Supported: false})
			return
		}
		v, ok := actCache.get(slug, time.Now(), func() (any, bool) { return ghActivityFor(slug) })
		if !ok {
			writeJSONError(w, http.StatusBadGateway, "gh unavailable")
			return
		}
		act := v.(ghActivity)
		writeJSON(w, struct {
			Supported bool `json:"supported"`
			ghActivity
		}{Supported: true, ghActivity: act})
	})
}
