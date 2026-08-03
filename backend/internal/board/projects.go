package board

// Project registry: the nav's global scope, decoupled from the raw ~/.claude
// scan. Where the sessions index reports one project per session cwd-basename,
// the registry lets you OWN those folders under a name you choose — rename,
// hide, or merge several folders into one project — so the scope taxonomy is
// yours, not whatever directories Claude Code happened to create. Symmetric
// with GroupStore: durable in data.db (it cannot be rebuilt from transcripts),
// this server is the single writer, an in-memory slice behind a mutex is the
// serving copy, and every mutation writes through an explicit column list.
//
// A project "owns" a set of folders — the s.Project values (session cwd
// basenames) it stands for. Ownership is EXCLUSIVE: a folder belongs to at most
// one project, or a session would be double-counted and the reverse
// folder→project map (used to label sessions) would be ambiguous. Upsert
// enforces that by stripping claimed folders from every other row.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Project is one user-owned project: a display name, the Claude folders (session
// cwd-basenames) it represents, a hidden flag (kept off the rail without losing
// its data), and an explicit rail order.
type Project struct {
	Name    string   `json:"name"`
	Folders []string `json:"folders"`
	Hidden  bool     `json:"hidden"`
	// Private mirrors the project's GitHub repo visibility (private repo, no
	// GitHub remote, or no resolvable repo → true) — derived by the sync loop
	// in main, never set by hand. Presentation mode hides private projects
	// app-wide. Orthogonal to Hidden, which is "off the rail always".
	Private bool `json:"private"`
	// RepoRoot is which repo this project IS — the one binding /project needs,
	// and the user's to set. RepoSlug and LinkKind are derived from it by the
	// sync loop in main and say only what the filesystem could confirm.
	// Deliberately NOT resolved from the Claude folders below: the project page
	// is its own taxonomy, and a binding nobody stated is a guess.
	RepoRoot string `json:"repoRoot,omitempty"`
	// RepoRoots is every local checkout of that one repo — a worktree, a second
	// clone on another branch, a copy made to try something. RepoRoot above is
	// the first of these, kept in sync as a projection: the API requires all of
	// them to be the same repo, so whichever one answers a slug answers for all.
	RepoRoots []string `json:"repoRoots,omitempty"`
	RepoSlug  string   `json:"repoSlug,omitempty"`
	LinkKind  string   `json:"linkKind"`
	Ord       int      `json:"ord"`
	// Parent is the name of the project this one nests under in the rail tree,
	// "" for a top-level project. It is a display-only edge (the scope of a
	// parent covers its subtree); a folder still belongs to exactly one project.
	Parent string `json:"parent"`
	// LogoVersion is the ms timestamp of the last logo write, 0 when there is
	// none. It doubles as a has-logo flag and a cache-buster the client appends
	// to the logo URL; the bytes themselves never ride the list payload.
	LogoVersion int64 `json:"logoVersion"`
}

func (p *Project) clone() Project {
	// Start from an empty (non-nil) slice so a project with no folders marshals
	// as [] rather than null — the client types folders as string[] and derefs
	// it (folders.length), and a JSON null would blow up the manager.
	return Project{
		Name:        p.Name,
		Folders:     append([]string{}, p.Folders...),
		Hidden:      p.Hidden,
		Private:     p.Private,
		RepoRoot:    p.RepoRoot,
		RepoRoots:   append([]string{}, p.RepoRoots...),
		RepoSlug:    p.RepoSlug,
		LinkKind:    p.LinkKind,
		Ord:         p.Ord,
		Parent:      p.Parent,
		LogoVersion: p.LogoVersion,
	}
}

// What the sync could confirm about a project's binding. LinkNone is not bound
// (and, unlike the empty string a fresh row carries, it means the user said
// so — see the migration); LinkMissing is bound to a path that is no longer
// there, which must read differently from unbound or a moved checkout silently
// looks abandoned; LinkLocal is a real repo with no GitHub remote; LinkLinked
// has a remote, so RepoSlug is filled and visibility is knowable.
const (
	LinkUnset   = ""
	LinkNone    = "none"
	LinkMissing = "missing"
	LinkLocal   = "local"
	LinkLinked  = "linked"
)

var (
	ErrProjectNotFound = errors.New("project not found")
	errNoLogo          = errors.New("project has no logo")
)

// ProjectStore persists the project registry to data.db (projects).
type ProjectStore struct {
	db *sql.DB

	mu       sync.Mutex
	projects []*Project
}

// NewProjectStore opens the project registry over data.db.
func NewProjectStore(db *sql.DB) *ProjectStore {
	ps := &ProjectStore{db: db}
	ps.loadDB()
	ps.loadRepoRoots() // must follow loadDB, not run inside it — see its comment
	return ps
}

func (ps *ProjectStore) loadDB() {
	// The logo blob itself stays on disk; only its version rides in memory.
	rows, err := ps.db.Query(`SELECT name, folders, hidden, private, repo_root, repo_slug, link_kind, ord, parent, logo_updated_at FROM projects`)
	if err != nil {
		log.Printf("projects: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		p := &Project{}
		var folders string
		var hidden, private, ord int
		if err := rows.Scan(&p.Name, &folders, &hidden, &private,
			&p.RepoRoot, &p.RepoSlug, &p.LinkKind, &ord, &p.Parent, &p.LogoVersion); err != nil {
			log.Printf("projects: load row: %v", err)
			continue
		}
		_ = json.Unmarshal([]byte(folders), &p.Folders)
		p.Hidden = hidden != 0
		p.Private = private != 0
		p.Ord = ord
		ps.projects = append(ps.projects, p)
	}
}

// loadRepoRoots fills RepoRoots from project_repos. Kept separate from the row
// scan above rather than joined into it: a project with three roots would come
// back as three rows, and every field but the root would be repeated — the join
// would have to be de-duplicated back into what two plain queries already give.
//
// Called AFTER loadDB has returned, never from inside it: its rows are still
// open until that function's deferred Close runs, and a second query on the same
// connection while they are silently returns nothing. That failure mode is the
// worst kind — every project simply has no roots, and nothing errors.
func (ps *ProjectStore) loadRepoRoots() {
	rows, err := ps.db.Query(`SELECT project, root FROM project_repos ORDER BY project, ord, root`)
	if err != nil {
		log.Printf("projects: load repo roots: %v", err)
		return
	}
	defer rows.Close()
	byName := map[string][]string{}
	for rows.Next() {
		var project, root string
		if err := rows.Scan(&project, &root); err != nil {
			log.Printf("projects: load repo root row: %v", err)
			continue
		}
		byName[project] = append(byName[project], root)
	}
	for _, p := range ps.projects {
		p.RepoRoots = byName[p.Name]
	}
}

// find returns the entry for name, or nil. Callers hold ps.mu.
func (ps *ProjectStore) find(name string) *Project {
	for _, p := range ps.projects {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// List returns every project, ordered by ord then case-insensitive name.
func (ps *ProjectStore) List() []Project {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := make([]Project, 0, len(ps.projects))
	for _, p := range ps.projects {
		out = append(out, p.clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ord != out[j].Ord {
			return out[i].Ord < out[j].Ord
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Upsert creates or replaces one project. The name doubles as the scope label
// and an URL path segment, so "/" is rejected. Claimed folders are stripped
// from every other project so ownership stays exclusive — the merge/reassign
// and the strip run in one transaction, and the in-memory copy is only touched
// after the DB commit.
func (ps *ProjectStore) Upsert(name string, folders []string, hidden bool, ord int, parent string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Project{}, fmt.Errorf("name is required")
	}
	if strings.Contains(name, "/") {
		return Project{}, fmt.Errorf("name cannot contain %q", "/")
	}
	parent = strings.TrimSpace(parent)
	if parent == name {
		return Project{}, fmt.Errorf("a project cannot be its own parent")
	}
	cleaned := cleanProjects(folders) // trims, drops empties, dedupes, keeps order
	raw, err := json.Marshal(cleaned)
	if err != nil {
		return Project{}, fmt.Errorf("encode folders: %w", err)
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()

	// Reject a parent that would loop the tree back onto this project (directly
	// or up a longer chain). A parent that names no known project is allowed —
	// it just renders top-level until that project exists.
	if parent != "" && ps.wouldCycle(name, parent) {
		return Project{}, fmt.Errorf("that parent would create a cycle")
	}

	tx, err := ps.db.Begin()
	if err != nil {
		return Project{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback()

	// `private` and the repo-link columns are absent from the UPDATE side: the
	// sync loop deriving them is their only writer, so a manager save can never
	// clobber what the sync found. A brand-new row does write `private`, and
	// writes it TRUE — the column defaults to public, which is the wrong answer
	// for a row nothing has looked at yet. Presentation mode's whole premise is
	// that what cannot be shown to be public stays hidden, and a row born public
	// is shown for the up-to-five minutes before the first tick reaches it.
	if _, err := tx.Exec(
		`INSERT INTO projects(name, folders, hidden, ord, parent, private) VALUES(?, ?, ?, ?, ?, 1)
		 ON CONFLICT(name) DO UPDATE SET folders=excluded.folders, hidden=excluded.hidden, ord=excluded.ord, parent=excluded.parent`,
		name, string(raw), boolToInt(hidden), ord, parent); err != nil {
		return Project{}, fmt.Errorf("write project: %w", err)
	}

	// Exclusive ownership: strip the newly-claimed folders off every other row.
	claimed := make(map[string]bool, len(cleaned))
	for _, f := range cleaned {
		claimed[f] = true
	}
	type change struct {
		p       *Project
		folders []string
	}
	var changes []change
	for _, p := range ps.projects {
		if p.Name == name {
			continue
		}
		kept := make([]string, 0, len(p.Folders))
		stripped := false
		for _, f := range p.Folders {
			if claimed[f] {
				stripped = true
				continue
			}
			kept = append(kept, f)
		}
		if stripped {
			changes = append(changes, change{p, kept})
		}
	}
	for _, c := range changes {
		craw, err := json.Marshal(c.folders)
		if err != nil {
			return Project{}, fmt.Errorf("encode folders: %w", err)
		}
		if _, err := tx.Exec(`UPDATE projects SET folders=? WHERE name=?`, string(craw), c.p.Name); err != nil {
			return Project{}, fmt.Errorf("reassign folders: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("commit: %w", err)
	}

	for _, c := range changes {
		c.p.Folders = c.folders
	}
	p := ps.find(name)
	if p == nil {
		// LinkKind is left at LinkUnset ("") to match the column's default:
		// a brand-new row has never been resolved, which is the state that
		// lets the sync make its opening binding offer exactly once. Private
		// mirrors what the INSERT above writes, so the row this call returns
		// and the row on disk cannot disagree about whether it may be shown.
		p = &Project{Name: name, Private: true}
		ps.projects = append(ps.projects, p)
	}
	p.Folders, p.Hidden, p.Ord, p.Parent = cleaned, hidden, ord, parent
	return p.clone(), nil
}

// SetPrivate records a project's derived GitHub visibility — the sync loop in
// main is the only writer (Upsert deliberately leaves the column alone).
// Returns whether the stored value changed.
func (ps *ProjectStore) SetPrivate(name string, private bool) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil || p.Private == private {
		return false
	}
	if _, err := ps.db.Exec(`UPDATE projects SET private=? WHERE name=?`, boolToInt(private), name); err != nil {
		log.Printf("projects: set private: %v", err)
		return false
	}
	p.Private = private
	return true
}

// SetRepoRoots binds a project to every local checkout of its repo, or clears
// the binding with an empty list. This one is the USER's write — the manager's
// repo picker — which is what makes the project page independent of the session
// index: the binding is stated, not inferred. Clearing it stamps LinkNone rather
// than the empty "never resolved" state, so the sync's initial offer is not made
// a second time.
//
// The caller validates the roots (a real checkout, all of them the same repo);
// this stores what it is given, in the order given, and mirrors the first into
// projects.repo_root.
//
// Returns the list as STORED — blanks and repeats dropped. Canonicalising two
// worktrees of one repo yields the same root twice, so the caller's slice and
// the stored one legitimately differ; returning it is what stops a reply from
// advertising a duplicate the database does not hold.
func (ps *ProjectStore) SetRepoRoots(name string, roots []string) ([]string, error) {
	clean := make([]string, 0, len(roots))
	seen := map[string]bool{}
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		clean = append(clean, r)
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil {
		return nil, ErrProjectNotFound
	}
	if slices.Equal(p.RepoRoots, clean) {
		return clean, nil
	}
	first := ""
	if len(clean) > 0 {
		first = clean[0]
	}
	kind, slug := LinkNone, ""
	if first != "" {
		// The sync fills in what the filesystem says on its next pass; until
		// then the row states the binding without claiming more than that.
		kind = LinkUnset
	}
	tx, err := ps.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("set repo roots: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op
	if _, err := tx.Exec(`DELETE FROM project_repos WHERE project=?`, name); err != nil {
		return nil, fmt.Errorf("set repo roots: %w", err)
	}
	for i, r := range clean {
		if _, err := tx.Exec(
			`INSERT INTO project_repos(project, root, ord) VALUES(?,?,?)`, name, r, i); err != nil {
			return nil, fmt.Errorf("set repo roots: %w", err)
		}
	}
	if _, err := tx.Exec(
		`UPDATE projects SET repo_root=?, repo_slug=?, link_kind=? WHERE name=?`,
		first, slug, kind, name); err != nil {
		return nil, fmt.Errorf("set repo roots: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("set repo roots: %w", err)
	}
	p.RepoRoots = clean
	p.RepoRoot, p.RepoSlug, p.LinkKind = first, slug, kind
	return clean, nil
}

// SetRepoDerived records what the sync could confirm about the binding — the
// slug and the kind, never the root itself, which belongs to the user. Reports
// whether anything changed, so a tick that finds nothing new stays silent
// instead of broadcasting.
func (ps *ProjectStore) SetRepoDerived(name, slug, kind string) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil || (p.RepoSlug == slug && p.LinkKind == kind) {
		return false
	}
	if _, err := ps.db.Exec(
		`UPDATE projects SET repo_slug=?, link_kind=? WHERE name=?`, slug, kind, name); err != nil {
		log.Printf("projects: set repo derived: %v", err)
		return false
	}
	p.RepoSlug, p.LinkKind = slug, kind
	return true
}

// AdoptRepoRoot offers a project its FIRST binding — the one place the project
// page still learns anything from the session index, and only for a row whose
// link has never been resolved (LinkUnset). Once the user has bound or cleared
// it the row is theirs, so a cleared binding is never re-offered. Reports
// whether it took.
func (ps *ProjectStore) AdoptRepoRoot(name, root string) bool {
	root = strings.TrimSpace(root)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil || root == "" || p.RepoRoot != "" || p.LinkKind != LinkUnset {
		return false
	}
	tx, err := ps.db.Begin()
	if err != nil {
		log.Printf("projects: adopt repo root: %v", err)
		return false
	}
	defer tx.Rollback() //nolint:errcheck // a committed tx rolls back to a no-op
	if _, err := tx.Exec(`UPDATE projects SET repo_root=? WHERE name=?`, root, name); err != nil {
		log.Printf("projects: adopt repo root: %v", err)
		return false
	}
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO project_repos(project, root, ord) VALUES(?,?,0)`, name, root); err != nil {
		log.Printf("projects: adopt repo root: %v", err)
		return false
	}
	if err := tx.Commit(); err != nil {
		log.Printf("projects: adopt repo root: %v", err)
		return false
	}
	p.RepoRoot = root
	p.RepoRoots = []string{root}
	return true
}

// PrivateNames returns the private projects' names — the label subtraction the
// board family and /api/scopes apply while presentation mode is on. Non-nil and
// freshly built, safe for the caller to hold.
func (ps *ProjectStore) PrivateNames() map[string]bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	names := map[string]bool{}
	for _, p := range ps.projects {
		if p.Private {
			names[p.Name] = true
		}
	}
	return names
}

// wouldCycle reports whether making parent the parent of name would create a
// loop — parent is name, or name sits somewhere up parent's existing chain.
// Caller holds ps.mu. The seen guard also stops a pre-existing cycle from
// spinning forever.
func (ps *ProjectStore) wouldCycle(name, parent string) bool {
	seen := map[string]bool{}
	for cur := parent; cur != ""; {
		if cur == name || seen[cur] {
			return true
		}
		seen[cur] = true
		p := ps.find(cur)
		if p == nil {
			return false
		}
		cur = p.Parent
	}
	return false
}

// Rename changes a project's display name in place, keeping its folders, hidden
// flag and order. The name is the label items carry, so the caller must cascade
// it across the label stores (todos/docs/drawings/groups). Rejects "/" and a
// name another project already holds; a collision with a group name is the
// endpoint's check. ErrProjectNotFound if old isn't a project.
func (ps *ProjectStore) Rename(old, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("name is required")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("name cannot contain %q", "/")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(old)
	if p == nil {
		return ErrProjectNotFound
	}
	if name == old {
		return nil
	}
	if ps.find(name) != nil {
		return fmt.Errorf("a project named %q already exists", name)
	}
	if _, err := ps.db.Exec(`UPDATE projects SET name=? WHERE name=?`, name, old); err != nil {
		return fmt.Errorf("rename project: %w", err)
	}
	// Children point at the old name — repoint them so the subtree survives.
	if _, err := ps.db.Exec(`UPDATE projects SET parent=? WHERE parent=?`, name, old); err != nil {
		return fmt.Errorf("rename project children: %w", err)
	}
	p.Name = name
	for _, c := range ps.projects {
		if c.Parent == old {
			c.Parent = name
		}
	}
	return nil
}

// Delete removes one project. Its folders fall back to unmapped (the sessions
// index still reports them); a label stored on a card/page keeps working as a
// plain string, it just loses its rail row until re-seeded or re-created.
func (ps *ProjectStore) Delete(name string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.find(name) == nil {
		return ErrProjectNotFound
	}
	if _, err := ps.db.Exec(`DELETE FROM projects WHERE name=?`, name); err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	// Children of the gone project fall back to top-level rather than dangling.
	if _, err := ps.db.Exec(`UPDATE projects SET parent='' WHERE parent=?`, name); err != nil {
		return fmt.Errorf("orphan project children: %w", err)
	}
	kept := ps.projects[:0]
	for _, p := range ps.projects {
		if p.Name == name {
			continue
		}
		if p.Parent == name {
			p.Parent = ""
		}
		kept = append(kept, p)
	}
	ps.projects = kept
	return nil
}

// SetLogo stores a project's logo (bytes + content-type) and stamps its
// version (a ms timestamp the caller supplies). ErrProjectNotFound if the
// project is gone.
func (ps *ProjectStore) SetLogo(name string, data []byte, contentType string, ts int64) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil {
		return ErrProjectNotFound
	}
	if _, err := ps.db.Exec(
		`UPDATE projects SET logo=?, logo_type=?, logo_updated_at=? WHERE name=?`,
		data, contentType, ts, name); err != nil {
		return fmt.Errorf("write logo: %w", err)
	}
	p.LogoVersion = ts
	return nil
}

// Logo returns a project's stored logo bytes and content-type, or errNoLogo
// when none is set (also ErrProjectNotFound if the project is gone).
func (ps *ProjectStore) Logo(name string) ([]byte, string, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.find(name) == nil {
		return nil, "", ErrProjectNotFound
	}
	var data []byte
	var ct string
	if err := ps.db.QueryRow(`SELECT logo, logo_type FROM projects WHERE name=?`, name).Scan(&data, &ct); err != nil {
		return nil, "", fmt.Errorf("read logo: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errNoLogo
	}
	return data, ct, nil
}

// DeleteLogo clears a project's logo. ErrProjectNotFound if the project is gone.
func (ps *ProjectStore) DeleteLogo(name string) error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.find(name)
	if p == nil {
		return ErrProjectNotFound
	}
	if _, err := ps.db.Exec(
		`UPDATE projects SET logo=NULL, logo_type='', logo_updated_at=0 WHERE name=?`, name); err != nil {
		return fmt.Errorf("clear logo: %w", err)
	}
	p.LogoVersion = 0
	return nil
}

// Seed ensures every name has a registry entry that owns the same-named folder:
// add-only and idempotent, existing rows are never touched. Names come from the
// labels the content actually carries (see SeedProjects), so a fresh registry
// MIRRORS today's taxonomy exactly (seed keeps names — no rewrite). New entries
// are appended after the current tail so a custom order survives. Returns how
// many were added.
func (ps *ProjectStore) Seed(names []string) int {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	next := 0
	for _, p := range ps.projects {
		if p.Ord >= next {
			next = p.Ord + 1
		}
	}

	sorted := append([]string(nil), names...)
	sort.Strings(sorted) // deterministic ord assignment across runs

	added := 0
	for _, name := range sorted {
		name = strings.TrimSpace(name)
		if name == "" || ps.find(name) != nil {
			continue
		}
		folders, err := json.Marshal([]string{name})
		if err != nil {
			continue
		}
		// private=1 for the same reason as Upsert: unproven is hidden.
		if _, err := ps.db.Exec(
			`INSERT INTO projects(name, folders, hidden, ord, private) VALUES(?, ?, 0, ?, 1)
			 ON CONFLICT(name) DO NOTHING`,
			name, string(folders), next); err != nil {
			log.Printf("projects: seed %q: %v", name, err)
			continue
		}
		ps.projects = append(ps.projects, &Project{Name: name, Folders: []string{name}, Private: true, Ord: next})
		next++
		added++
	}
	return added
}

// SeedProjects mirrors the CONTENT taxonomy into the registry: every label
// actually carried by a board card, wiki page or drawing becomes a registry
// entry owning its same-named Claude folder (if one exists — harmless
// otherwise). The session scan is deliberately NOT a source: which folders
// Claude Code happened to run in must not invent projects — sessions reach the
// scope through the folders a project owns (or the same-name fallback), and
// group members resolve by name without needing a row of their own. Add-only,
// so it also runs on the periodic tick to adopt a freshly-labelled item.
// Returns how many entries it added.
func SeedProjects(ps *ProjectStore, groups *GroupStore, todos *TodoStore, docs *DocStore, drawings *DrawingStore) int {
	set := map[string]bool{}
	for _, t := range todos.List() {
		if t.Repo != "" {
			set[t.Repo] = true
		}
	}
	for _, d := range docs.List() {
		if d.Group != "" {
			set[d.Group] = true
		}
	}
	for _, dr := range drawings.List() {
		if dr.Group != "" {
			set[dr.Group] = true
		}
	}
	// Projects and groups share the rail's namespace but are disjoint: a card or
	// page can be scoped to a GROUP name (resolved group-first), so drop any
	// label that is already a group — it must not also become a project row.
	for _, g := range groups.List() {
		delete(set, g.Name)
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	return ps.Seed(names)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
