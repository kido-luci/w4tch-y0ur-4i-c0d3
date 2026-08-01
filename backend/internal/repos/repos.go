package repos

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"watch-your-ai-code/internal/index"
)

// Repo is one repo a scope resolves to: where it lives, whether it carries
// an index, and — when it does — the index's size and age plus how many
// commits the repo has seen since it was written (-1 = git couldn't answer).
type Repo struct {
	Root   string `json:"root"`
	Folder string `json:"folder"` // the Claude folder that led here
	// Guessed marks a root the NAME fallback found rather than a session cwd —
	// see the fallback in Repos. It is a directory that merely shares the
	// folder's name, so nothing about it proves the folder belongs to it;
	// anything deriving ownership from a repo must exclude these, and the UI
	// renders them as unverified.
	Guessed      bool      `json:"guessed,omitempty"`
	HasIndex     bool      `json:"hasIndex"`
	IndexedAt    time.Time `json:"indexedAt"`
	Files        int       `json:"files"`
	Nodes        int       `json:"nodes"`
	Edges        int       `json:"edges"`
	CommitsSince int       `json:"commitsSince"`
}

func DBPath(root string) string {
	return filepath.Join(root, ".codegraph", "codegraph.db")
}

// Sessions is the slice of the session index that repo resolution reads:
// the working directories sessions ran in, and when. Taking this rather than
// the whole *Index is what lets resolution live outside the index's package.
type Sessions interface {
	Snapshot() []*index.Session
}

// Binding is one repo a scope's projects are BOUND to — the project page's own
// answer to "which repos are these", stated in the registry rather than
// inferred from the directories sessions happened to run in.
type Binding struct {
	Root    string
	Project string // the project that declared it — what the UI names the repo
}

// Bindings answers that for a scope label. main supplies it (it is the only
// place that holds both the registry and the group store); nil leaves Bound
// empty, which is the honest answer for a resolver with no registry behind it.
type Bindings func(scope string) []Binding

// Resolver maps a scope to on-disk repo roots, two different ways for two
// different questions. Bound answers from the project registry — the git tab
// and the code graph go through it, so the two always list the same repos, and
// every drill-down validates its ?repo against BoundRoot, which is why those
// endpoints are safe to expose at all. Repos answers from the session index
// instead, which is what the binding PICKER and the registry's opening offer
// need: where has work actually happened.
type Resolver struct {
	sessions Sessions
	bindings Bindings
}

func New(ss Sessions) *Resolver { return &Resolver{sessions: ss} }

// UseBindings wires the project registry in. Separate from New so the resolver
// keeps working (session-derived only) for callers that have no registry.
func (rr *Resolver) UseBindings(fn Bindings) { rr.bindings = fn }

// Bound is the scope's repos as its projects declare them. A binding whose path
// has gone missing is kept rather than filtered: the git tab showing a repo it
// cannot read is how you find out, whereas dropping it silently shortens the
// list and explains nothing.
func (rr *Resolver) Bound(scope string) []Repo {
	out := []Repo{}
	if rr.bindings == nil {
		return out
	}
	seen := map[string]bool{}
	for _, b := range rr.bindings(scope) {
		if b.Root == "" || seen[b.Root] {
			continue
		}
		seen[b.Root] = true
		_, err := os.Stat(DBPath(b.Root))
		out = append(out, Repo{Root: b.Root, Folder: b.Project, HasIndex: err == nil, CommitsSince: -1})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].HasIndex != out[j].HasIndex {
			return out[i].HasIndex
		}
		return out[i].Folder < out[j].Folder
	})
	return out
}

// BoundRoot reports whether root is one of the scope's bound repos — the
// whitelist every git / GitHub / code-graph drill-down checks its ?repo
// against, so an arbitrary path is a 404 rather than a filesystem read.
func (rr *Resolver) BoundRoot(scope, root string) bool {
	if root == "" {
		return false
	}
	for _, r := range rr.Bound(scope) {
		if r.Root == root {
			return true
		}
	}
	return false
}

// Repos maps the scope's folders to on-disk repo roots. For each folder the
// candidates are its sessions' cwds, newest first; the first one that still
// exists wins, preferring one that carries an index — so a leftover worktree
// path or a since-moved checkout can't shadow the real repo. A folder with no
// live session cwd of its own then falls back to finding an indexed directory
// of that name near a known workspace cwd (see findIndexedDir), so an infra
// sub-repo you've indexed surfaces without ever opening Claude in it. Roots are
// deduped across folders, indexed repos sort first.
func (rr *Resolver) Repos(project string) []Repo {
	var want map[string]bool
	if list := index.SplitProjects(project); list != nil {
		want = make(map[string]bool, len(list))
		for _, p := range list {
			want[p] = true
		}
	}
	type cand struct {
		cwd string
		at  time.Time
	}
	byFolder := map[string][]cand{}
	for _, s := range rr.sessions.Snapshot() {
		if s.CWD == "" || (want != nil && !want[s.Project]) {
			continue
		}
		byFolder[s.Project] = append(byFolder[s.Project], cand{s.CWD, s.EndedAt})
	}

	out := []Repo{}
	seen := map[string]bool{}
	for folder, cands := range byFolder {
		sort.Slice(cands, func(i, j int) bool { return cands[i].at.After(cands[j].at) })
		root, hasIndex := "", false
		for _, c := range cands {
			fi, err := os.Stat(c.cwd)
			if err != nil || !fi.IsDir() {
				continue
			}
			if _, err := os.Stat(DBPath(c.cwd)); err == nil {
				root, hasIndex = c.cwd, true
				break
			}
			if root == "" {
				root = c.cwd
			}
		}
		if root == "" || seen[root] {
			continue
		}
		seen[root] = true
		out = append(out, Repo{Root: root, Folder: folder, HasIndex: hasIndex, CommitsSince: -1})
	}

	// Fallback for a scoped folder that resolved to nothing above — an infra
	// sub-repo you indexed but never opened Claude in, so it has no session cwd
	// of its own. Locate its OWN repo dir by matching the folder name against
	// the children and siblings of every known session cwd, and keep it only if
	// it carries a .codegraph. This never borrows a parent's index — it finds
	// the sub-repo's own — so a nested checkout surfaces without a session there.
	if want != nil {
		resolved := make(map[string]bool, len(out))
		for _, r := range out {
			resolved[r.Folder] = true
		}
		var missing []string
		for folder := range want {
			if !resolved[folder] && folder != "" && !strings.ContainsAny(folder, `/\`) && folder != ".." {
				missing = append(missing, folder)
			}
		}
		if len(missing) > 0 {
			anchors := rr.Dirs()
			for _, folder := range missing {
				root := findIndexedDir(folder, anchors)
				if root != "" && !seen[root] {
					seen[root] = true
					out = append(out, Repo{Root: root, Folder: folder, Guessed: true, HasIndex: true, CommitsSince: -1})
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].HasIndex != out[j].HasIndex {
			return out[i].HasIndex
		}
		return out[i].Root < out[j].Root
	})
	return out
}

// Dirs is the distinct set of session working directories that still exist on
// disk — the anchors the fallback resolution searches from.
func (rr *Resolver) Dirs() []string {
	uniq := map[string]bool{}
	for _, s := range rr.sessions.Snapshot() {
		if s.CWD != "" {
			uniq[s.CWD] = true
		}
	}
	out := make([]string, 0, len(uniq))
	for d := range uniq {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// findIndexedDir looks for a directory named `folder` that carries its own
// .codegraph, sitting as a child or sibling of one of the anchor cwds (or being
// one). "" if none — the fallback only surfaces a sub-repo that's actually been
// indexed, never a bare directory.
func findIndexedDir(folder string, anchors []string) string {
	indexed := func(d string) string {
		if _, err := os.Stat(DBPath(d)); err != nil {
			return ""
		}
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return ""
		}
		return d
	}
	for _, a := range anchors {
		if r := indexed(filepath.Join(a, folder)); r != "" { // child of a workspace root
			return r
		}
		if r := indexed(filepath.Join(filepath.Dir(a), folder)); r != "" { // sibling of a subdir
			return r
		}
		if filepath.Base(a) == folder {
			if r := indexed(a); r != "" {
				return r
			}
		}
	}
	return ""
}
