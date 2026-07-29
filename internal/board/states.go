package board

// Custom workflow states — the board's columns. Until data.db v12 the three
// columns were an enum baked into the binary (`todoStatusRank`); a board that
// tracks real work needs its own columns — "in review", "blocked", "shipped" —
// so they live in data.db's todo_states table instead.
//
// The three original ids are SEEDED, not migrated: rows `backlog`, `doing` and
// `done` exist from the first boot on v12, so every card's status column, every
// REST body and every MCP call keeps the exact string it always used, and
// adding a column becomes a new row rather than a schema change. Those three
// are builtin and undeletable — a create with no status still lands in
// `backlog`, and the cost snapshot still needs a done state to freeze against.
//
// CATEGORY, not name, is what the rest of the server reads: `done` freezes the
// snapshot (RefreezeTodo in todos.go) and the burndown counts everything outside
// it as remaining (cycles.go). Rename "Done" to "Shipped" and both keep working.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
)

// TodoState is one board column. Repo scopes it to a single project ("" = every
// scope shows it), Order sorts the columns left to right, and WIPLimit is a cap
// the board renders — nothing server-side refuses a write over it, the way a
// kanban WIP limit is a signal and not a lock.
type TodoState struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Category string  `json:"category"` // todo | started | done
	Order    float64 `json:"order"`
	WIPLimit int     `json:"wipLimit,omitempty"`
	Repo     string  `json:"repo,omitempty"`
	Builtin  bool    `json:"builtin,omitempty"` // derived on read, never stored
}

// BuiltinStates are the three ids the pre-v12 enum used. Undeletable: the
// create-with-no-status default and the snapshot freeze both name them.
var BuiltinStates = map[string]bool{"backlog": true, "doing": true, "done": true}

func validStateCategory(c string) bool {
	return c == "todo" || c == "started" || c == "done"
}

var ErrStateNotFound = errors.New("state not found")

// StateStore persists the board columns to data.db (todo_states). Same write
// model as the other stores: single writer, in-memory slice behind a mutex as
// the serving copy, DB-first so a failed write surfaces as an error instead of
// served state that evaporates on restart.
type StateStore struct {
	db *sql.DB

	mu     sync.Mutex
	states []*TodoState
}

func NewStateStore(db *sql.DB) *StateStore {
	ss := &StateStore{db: db}
	ss.loadDB()
	return ss
}

func (ss *StateStore) loadDB() {
	rows, err := ss.db.Query(`SELECT id,name,category,ord,wip_limit,repo FROM todo_states`)
	if err != nil {
		log.Printf("states: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		s := &TodoState{}
		if err := rows.Scan(&s.ID, &s.Name, &s.Category, &s.Order, &s.WIPLimit, &s.Repo); err != nil {
			log.Printf("states: load row: %v", err)
			continue
		}
		s.Builtin = BuiltinStates[s.ID]
		ss.states = append(ss.states, s)
	}
}

// persist writes one column through. Callers hold ss.mu.
func (ss *StateStore) persist(s *TodoState) error {
	res, err := ss.db.Exec(
		`UPDATE todo_states SET name=?,category=?,ord=?,wip_limit=?,repo=? WHERE id=?`,
		s.Name, s.Category, s.Order, s.WIPLimit, s.Repo, s.ID)
	if err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := ss.db.Exec(
		`INSERT INTO todo_states(id,name,category,ord,wip_limit,repo) VALUES(?,?,?,?,?,?)`,
		s.ID, s.Name, s.Category, s.Order, s.WIPLimit, s.Repo); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	return nil
}

func sortStates(out []TodoState) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].ID < out[j].ID
	})
}

// List returns every column across every scope, left to right.
func (ss *StateStore) List() []TodoState {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]TodoState, 0, len(ss.states))
	for _, s := range ss.states {
		out = append(out, *s)
	}
	sortStates(out)
	return out
}

// ListForScope returns the columns one scope sees: the shared ones plus those
// owned by any project the scope covers.
//
// Takes a resolved ScopeSet, not a label: a label can name a GROUP, and a
// column created under that group must stay visible when the rail narrows to a
// member project. Comparing the label to s.Repo made it disappear instead —
// see scope.go.
func (ss *StateStore) ListForScope(in ScopeSet) []TodoState {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make([]TodoState, 0, len(ss.states))
	for _, s := range ss.states {
		if in.CoversOwner(s.Repo) {
			out = append(out, *s)
		}
	}
	sortStates(out)
	return out
}

// Get returns one column by id.
func (ss *StateStore) Get(id string) (TodoState, bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, s := range ss.states {
		if s.ID == id {
			return *s, true
		}
	}
	return TodoState{}, false
}

// Valid reports whether a card may carry this status. Existence is the whole
// test on purpose: scoping is a rendering concern, and a stricter rule here
// would reject a card whose project was renamed out from under its column.
func (ss *StateStore) Valid(id string) bool {
	_, ok := ss.Get(id)
	return ok
}

// IsDone reports whether landing in this state should freeze a card's cost
// snapshot — the category test that replaced `status == "done"`.
func (ss *StateStore) IsDone(id string) bool {
	s, ok := ss.Get(id)
	return ok && s.Category == "done"
}

// Rank is a card's column position for sorting. An unknown state sinks to the
// end rather than to the front, so a card orphaned by a deleted column is
// visible at the edge of the board instead of jumping to the top of it.
func (ss *StateStore) Rank(id string) float64 {
	s, ok := ss.Get(id)
	if !ok {
		return math.Inf(1)
	}
	return s.Order
}

// StatusNames maps id → display name, for anything that renders a state
// without loading the whole store (the MCP tool descriptions, mostly).
func (ss *StateStore) StatusNames() map[string]string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	out := make(map[string]string, len(ss.states))
	for _, s := range ss.states {
		out[s.ID] = s.Name
	}
	return out
}

// slugState turns a column name into a stable, readable id ("In review" →
// "in-review"). The id is what REST bodies and the MCP tools carry — a Claude
// session sets a card's status by that string — so it has to read like one.
// Names with nothing slug-able (CJK, emoji) fall back to a random id.
func slugState(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		return "state-" + randomID()
	}
	return s
}

// Create appends a column to the right of the existing ones.
func (ss *StateStore) Create(name, category, repo string, wip int) (TodoState, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TodoState{}, fmt.Errorf("name is required")
	}
	if category == "" {
		category = "started"
	}
	if !validStateCategory(category) {
		return TodoState{}, fmt.Errorf("category must be todo, started or done")
	}
	if wip < 0 {
		return TodoState{}, fmt.Errorf("wipLimit cannot be negative")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	taken := map[string]bool{}
	maxOrder := 0.0
	for _, s := range ss.states {
		taken[s.ID] = true
		if s.Order > maxOrder {
			maxOrder = s.Order
		}
	}
	id := slugState(name)
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", slugState(name), n)
	}
	st := &TodoState{
		ID:       id,
		Name:     name,
		Category: category,
		Order:    maxOrder + 1,
		WIPLimit: wip,
		Repo:     strings.TrimSpace(repo),
	}
	if err := ss.persist(st); err != nil {
		return TodoState{}, err
	}
	ss.states = append(ss.states, st)
	return *st, nil
}

// StatePatch is a partial column update; nil fields stay untouched.
type StatePatch struct {
	Name     *string  `json:"name"`
	Category *string  `json:"category"`
	Order    *float64 `json:"order"`
	WIPLimit *int     `json:"wipLimit"`
}

// Update renames, recategorises, reorders or re-caps one column. A builtin's
// name and position are editable — only its id and its existence are fixed.
func (ss *StateStore) Update(id string, p StatePatch) (TodoState, error) {
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		return TodoState{}, fmt.Errorf("name is required")
	}
	if p.Category != nil && !validStateCategory(*p.Category) {
		return TodoState{}, fmt.Errorf("category must be todo, started or done")
	}
	if p.WIPLimit != nil && *p.WIPLimit < 0 {
		return TodoState{}, fmt.Errorf("wipLimit cannot be negative")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for _, s := range ss.states {
		if s.ID != id {
			continue
		}
		next := *s
		if p.Name != nil {
			next.Name = strings.TrimSpace(*p.Name)
		}
		if p.Category != nil {
			next.Category = *p.Category
		}
		if p.Order != nil {
			next.Order = *p.Order
		}
		if p.WIPLimit != nil {
			next.WIPLimit = *p.WIPLimit
		}
		if err := ss.persist(&next); err != nil {
			return TodoState{}, err
		}
		*s = next
		return next, nil
	}
	return TodoState{}, ErrStateNotFound
}

// Delete removes one column. Builtins are refused outright; a column still
// holding cards is refused by the caller, which is the side that can see them.
func (ss *StateStore) Delete(id string) error {
	if BuiltinStates[id] {
		return fmt.Errorf("%q is a builtin column and cannot be deleted", id)
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	for i, s := range ss.states {
		if s.ID != id {
			continue
		}
		if _, err := ss.db.Exec(`DELETE FROM todo_states WHERE id=?`, id); err != nil {
			return fmt.Errorf("delete state: %w", err)
		}
		ss.states = append(ss.states[:i], ss.states[i+1:]...)
		return nil
	}
	return ErrStateNotFound
}

// RenameRepo re-points a project's own columns at its new name — the states
// half of a project rename, alongside TodoStore.RenameRepo.
func (ss *StateStore) RenameRepo(old, name string) int {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if _, err := ss.db.Exec(`UPDATE todo_states SET repo=? WHERE repo=?`, name, old); err != nil {
		log.Printf("states: rename repo: %v", err)
		return 0
	}
	n := 0
	for _, s := range ss.states {
		if s.Repo == old {
			s.Repo = name
			n++
		}
	}
	return n
}
