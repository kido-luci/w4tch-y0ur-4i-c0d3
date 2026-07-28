package main

// Todo board (roadmap "coding manager", phase 1): a local backlog whose cards
// link to real sessions. Since v0.45 the board lives in data.db's todos table
// (one row per card, column-explicit writes — see data.go); this server is
// the single writer, so an in-memory slice guarded by a mutex stays the
// serving copy and every mutation writes its one row through. DB-first, like
// the other stores: the row write happens before the serving copy mutates, so
// a failed write surfaces as an error instead of served state that evaporates
// on restart.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Todo is one board card. Status moves backlog → doing → done; Order sorts
// within a column (the client assigns midpoints on drag & drop).
type Todo struct {
	ID        string    `json:"id"`
	Seq       int       `json:"seq"` // stable human-friendly card number (#12)
	Title     string    `json:"title"`
	Note      string    `json:"note,omitempty"`
	Repo      string    `json:"repo,omitempty"` // project name, as the sessions API reports it
	Labels    []string  `json:"labels,omitempty"`
	Status    string    `json:"status"` // backlog | doing | done
	Order     float64   `json:"order"`
	CreatedAt time.Time `json:"createdAt"`
	// LinkedSessionIDs tie the card to real sessions (references, not launchers
	// — the viewer never starts anything), in the order they were linked. A
	// ticket routinely spans several sessions (context fills up, work resumes
	// the next day), and the done snapshot sums all of them.
	LinkedSessionIDs []string `json:"linkedSessionIds,omitempty"`
	// LegacyLinkedSessionID reads the pre-v0.35 single-session link so old
	// boards migrate on load; it is cleared once backfilled and never written
	// back. Drop this field once no todos.json in the wild carries the old key.
	LegacyLinkedSessionID string `json:"linkedSessionId,omitempty"`
	// LinkedDrawingIDs tie the card to wireframes in the design library
	// (`#/design`), in the order they were linked.
	LinkedDrawingIDs []string `json:"linkedDrawingIds,omitempty"`
	// LinkedDocIDs tie the card to pages in the docs wiki (`#/docs`), in the
	// order they were linked.
	LinkedDocIDs []string `json:"linkedDocIds,omitempty"`
	// Snapshot freezes the linked sessions' summed numbers when the card lands
	// in done (taken server-side; cleared when the card leaves done).
	Snapshot *TodoSnapshot `json:"snapshot,omitempty"`

	// --- depth, data.db v12 -------------------------------------------------

	// ParentID nests this card under another ("" = top level). The board nests
	// exactly two levels — epic → story, or story → subtask — which is what
	// checkParent enforces, and what keeps every cycle unrepresentable.
	ParentID string `json:"parentId,omitempty"`
	// Kind is the card's shape (epic | story | task | bug). It drives the
	// icon and the table view's grouping only: the server never rules on
	// which kind may parent which, because real boards break that rule daily.
	Kind string `json:"kind,omitempty"`
	// Priority sorts urgent work up: 0 none, 1 low, 2 medium, 3 high, 4 urgent.
	Priority int `json:"priority,omitempty"`
	// Estimate is story points, not hours — one dev plus an AI has no stable
	// hour, but relative size still holds. 0 = unestimated.
	Estimate float64 `json:"estimate,omitempty"`
	// CycleID puts the card in a sprint ("" = not planned into one).
	CycleID string `json:"cycleId,omitempty"`
	// Rollup counts this card's children and their points. Computed on read
	// and never stored, so it cannot drift from the children it describes;
	// nil for a card with none.
	Rollup *TodoRollup `json:"rollup,omitempty"`
}

// TodoRollup is a parent card's view of its children — the "3/5 done, 8 of 13
// points" line an epic shows instead of pretending to be a leaf.
type TodoRollup struct {
	Children     int     `json:"children"`
	Done         int     `json:"done"`
	Estimate     float64 `json:"estimate"`
	EstimateDone float64 `json:"estimateDone"`
}

// validTodoKind gates the card shapes. Unset reads as "task".
func validTodoKind(k string) bool {
	return k == "epic" || k == "story" || k == "task" || k == "bug"
}

// TodoSnapshot is the linked sessions' total cost at the moment a todo was
// done, summed across every session linked to the card.
type TodoSnapshot struct {
	Tokens     int64     `json:"tokens"` // main + agents
	CostUSD    float64   `json:"costUsd"`
	Agents     int       `json:"agents"`
	DurationMs int64     `json:"durationMs"`
	Sessions   int       `json:"sessions"` // how many linked sessions it covers
	TakenAt    time.Time `json:"takenAt"`
}

// todoStatusRank is the pre-v12 column order. Since the columns moved into
// data.db's todo_states table (states.go) it survives only as the fallback for
// a store with no StateStore attached — the one-time todos.json import, and
// unit tests that exercise nothing but the three builtin columns. With states
// attached the order comes from the table, so a renamed or inserted column
// sorts where the user put it.
var todoStatusRank = map[string]int{"backlog": 0, "doing": 1, "done": 2}

func validTodoStatus(s string) bool {
	_, ok := todoStatusRank[s]
	return ok
}

// todoPatch is a partial update; nil fields stay untouched.
type todoPatch struct {
	Title            *string   `json:"title"`
	Note             *string   `json:"note"`
	Repo             *string   `json:"repo"`
	Labels           *[]string `json:"labels"`
	Status           *string   `json:"status"`
	Order            *float64  `json:"order"`
	LinkedSessionIDs *[]string `json:"linkedSessionIds"` // [] unlinks all
	LinkedDrawingIDs *[]string `json:"linkedDrawingIds"` // [] unlinks all
	LinkedDocIDs     *[]string `json:"linkedDocIds"`     // [] unlinks all
	ParentID         *string   `json:"parentId"`         // "" un-nests
	Kind             *string   `json:"kind"`
	Priority         *int      `json:"priority"`
	Estimate         *float64  `json:"estimate"`
	CycleID          *string   `json:"cycleId"` // "" removes from its cycle
}

// cleanStrings trims each entry and drops empties/duplicates, keeping order
// (labels, linked session ids, linked drawing ids).
func cleanStrings(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range in {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

var errTodoNotFound = errors.New("todo not found")

// TodoStore persists the board to data.db's todos table; the in-memory slice
// is the serving copy.
type TodoStore struct {
	db *sql.DB
	// states is the workflow-column table (states.go). Optional so the
	// file-import path and the older unit tests can build a bare store; when
	// it is nil every status question falls back to the builtin trio.
	states *StateStore
	// events is the append-only history (events.go), also optional: a store
	// without one simply records nothing, which is what the import path wants.
	events *EventStore

	mu    sync.Mutex
	todos []*Todo
}

// NewTodoStore opens the board over data.db (import from the pre-v0.45
// todos.json has already run by then — see importDataOnce).
func NewTodoStore(db *sql.DB) *TodoStore {
	ts := &TodoStore{db: db}
	ts.loadDB()
	return ts
}

// UseStates points the board at the workflow columns. Injected rather than
// constructed here on purpose: the StateStore keeps its own serving copy of
// todo_states, and a second instance would be a second writer over one table —
// the failure mode data.go's header warns about.
func (ts *TodoStore) UseStates(ss *StateStore) { ts.states = ss }

// UseEvents points the board at the history log, same reasoning as UseStates.
func (ts *TodoStore) UseEvents(es *EventStore) { ts.events = es }

// log appends one history row, best-effort: the card has already been written,
// and losing a history row must not fail the move that produced it. Callers
// hold ts.mu.
func (ts *TodoStore) log(todoID, kind, from, to string) {
	if ts.events == nil {
		return
	}
	ts.events.Append(todoID, kind, from, to)
}

// validStatus reports whether a card may carry this status.
func (ts *TodoStore) validStatus(s string) bool {
	if ts.states == nil {
		return validTodoStatus(s)
	}
	return ts.states.Valid(s)
}

// IsDoneStatus reports whether landing in this status should freeze a card's
// cost snapshot. It is the column's CATEGORY that decides, not its name, so a
// workflow whose last column is called "Shipped" still freezes — and a board
// with two done-ish columns ("Merged", "Released") freezes in both.
func (ts *TodoStore) IsDoneStatus(status string) bool {
	if ts.states == nil {
		return status == "done"
	}
	return ts.states.IsDone(status)
}

// ranks snapshots column → position once per sort, instead of locking the
// state store inside the comparator.
func (ts *TodoStore) ranks() map[string]float64 {
	out := map[string]float64{}
	if ts.states == nil {
		for id, r := range todoStatusRank {
			out[id] = float64(r)
		}
		return out
	}
	for _, s := range ts.states.List() {
		out[s.ID] = s.Order
	}
	return out
}

func (ts *TodoStore) loadDB() {
	rows, err := ts.db.Query(`SELECT id,seq,title,note,repo,labels,status,ord,created_at,linked_sessions,linked_drawings,linked_docs,snapshot,parent_id,kind,priority,estimate,cycle_id FROM todos`)
	if err != nil {
		log.Printf("todos: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		t := &Todo{}
		var labels, linkedS, linkedD, linkedDoc, snap string
		var created int64
		if err := rows.Scan(&t.ID, &t.Seq, &t.Title, &t.Note, &t.Repo, &labels, &t.Status,
			&t.Order, &created, &linkedS, &linkedD, &linkedDoc, &snap,
			&t.ParentID, &t.Kind, &t.Priority, &t.Estimate, &t.CycleID); err != nil {
			log.Printf("todos: load row: %v", err)
			continue
		}
		t.CreatedAt = nanoToTime(created)
		_ = json.Unmarshal([]byte(labels), &t.Labels)
		_ = json.Unmarshal([]byte(linkedS), &t.LinkedSessionIDs)
		_ = json.Unmarshal([]byte(linkedD), &t.LinkedDrawingIDs)
		_ = json.Unmarshal([]byte(linkedDoc), &t.LinkedDocIDs)
		if snap != "" {
			var s TodoSnapshot
			if json.Unmarshal([]byte(snap), &s) == nil {
				t.Snapshot = &s
			}
		}
		ts.todos = append(ts.todos, t)
	}
}

// loadFromFile reads a pre-v0.45 todos.json. Import path only (data.go): it
// carries the pre-v0.35 backfills and the corrupt-file set-aside, so the
// import sees exactly what the old binary would have served — and it never
// writes the file back.
func (ts *TodoStore) loadFromFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return // no file — empty board
	}
	if err := json.Unmarshal(b, &ts.todos); err != nil {
		// User-created data: never clobber a corrupt file, set it aside instead.
		bad := path + ".corrupt"
		_ = os.Rename(path, bad)
		log.Printf("todos: %s is corrupt (%v), moved to %s", path, err, bad)
		ts.todos = nil
	}
	// Backfill card numbers for todos created before Seq existed.
	maxSeq := 0
	for _, t := range ts.todos {
		if t.Seq > maxSeq {
			maxSeq = t.Seq
		}
	}
	for _, t := range ts.todos {
		if t.Seq == 0 {
			maxSeq++
			t.Seq = maxSeq
		}
		// Pre-v0.35 boards carry one linkedSessionId; fold it into the list.
		if t.LegacyLinkedSessionID != "" {
			if len(t.LinkedSessionIDs) == 0 {
				t.LinkedSessionIDs = []string{t.LegacyLinkedSessionID}
			}
			t.LegacyLinkedSessionID = ""
		}
		// A snapshot frozen before v0.35 predates the Sessions count. Zero can
		// only mean "legacy": the old model froze exactly one session, and
		// refreezeTodo never writes a snapshot covering none.
		if t.Snapshot != nil && t.Snapshot.Sessions == 0 {
			t.Snapshot.Sessions = 1
		}
	}
}

// jsonList serializes a string list for its TEXT column ("[]" when empty, so
// the column never holds SQL NULL).
func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// sqlExecer is the slice of *sql.DB / *sql.Tx that writes need.
type sqlExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// upsertTodoTx writes one card with an explicit column list — never
// DELETE-and-reinsert — so a binary that predates a column leaves that
// column alone. Shared by the store and the one-time import.
func upsertTodoTx(q sqlExecer, t *Todo) error {
	snap := ""
	if t.Snapshot != nil {
		if b, err := json.Marshal(t.Snapshot); err == nil {
			snap = string(b)
		}
	}
	res, err := q.Exec(
		`UPDATE todos SET seq=?,title=?,note=?,repo=?,labels=?,status=?,ord=?,created_at=?,linked_sessions=?,linked_drawings=?,linked_docs=?,snapshot=?,parent_id=?,kind=?,priority=?,estimate=?,cycle_id=? WHERE id=?`,
		t.Seq, t.Title, t.Note, t.Repo, jsonList(t.Labels), t.Status, t.Order,
		timeToNano(t.CreatedAt), jsonList(t.LinkedSessionIDs), jsonList(t.LinkedDrawingIDs), jsonList(t.LinkedDocIDs), snap,
		t.ParentID, todoKindOr(t.Kind), t.Priority, t.Estimate, t.CycleID, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = q.Exec(
		`INSERT INTO todos(id,seq,title,note,repo,labels,status,ord,created_at,linked_sessions,linked_drawings,linked_docs,snapshot,parent_id,kind,priority,estimate,cycle_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Seq, t.Title, t.Note, t.Repo, jsonList(t.Labels), t.Status, t.Order,
		timeToNano(t.CreatedAt), jsonList(t.LinkedSessionIDs), jsonList(t.LinkedDrawingIDs), jsonList(t.LinkedDocIDs), snap,
		t.ParentID, todoKindOr(t.Kind), t.Priority, t.Estimate, t.CycleID)
	return err
}

// todoKindOr keeps the kind column non-empty ("task" is the default a card
// created before v12 reads back), so the UI never has to special-case "".
func todoKindOr(k string) string {
	if k == "" {
		return "task"
	}
	return k
}

// persist writes one card through, returning the failure so callers refuse
// the mutation instead of serving state data.db never accepted — the DB-first
// rule the other stores follow. Callers hold ts.mu.
func (ts *TodoStore) persist(t *Todo) error {
	if err := upsertTodoTx(ts.db, t); err != nil {
		return fmt.Errorf("persist todo: %w", err)
	}
	return nil
}

// removeRow deletes one card's row. Callers hold ts.mu.
func (ts *TodoStore) removeRow(id string) error {
	if _, err := ts.db.Exec(`DELETE FROM todos WHERE id=?`, id); err != nil {
		return fmt.Errorf("delete todo: %w", err)
	}
	return nil
}

// randomID is shared by the todo and drawing stores.
func randomID() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// sortTodos orders the board column by column, then by Order within one. A
// card whose column no longer exists ranks +Inf, so it surfaces at the end of
// the board rather than jumping to the front of it.
func (ts *TodoStore) sortTodos(todos []Todo) {
	rank := ts.ranks()
	rankOf := func(status string) float64 {
		if r, ok := rank[status]; ok {
			return r
		}
		return math.Inf(1)
	}
	sort.SliceStable(todos, func(i, j int) bool {
		if a, b := rankOf(todos[i].Status), rankOf(todos[j].Status); a != b {
			return a < b
		}
		if todos[i].Order != todos[j].Order {
			return todos[i].Order < todos[j].Order
		}
		return todos[i].CreatedAt.Before(todos[j].CreatedAt)
	})
}

// List returns every todo, column-ordered (the workflow's column order, then
// Order within one).
func (ts *TodoStore) List() []Todo {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]Todo, 0, len(ts.todos))
	for _, t := range ts.todos {
		out = append(out, *t)
	}
	ts.sortTodos(out)
	ts.fillRollups(out)
	return out
}

// fillRollups hangs a child count and a points total on every card that has
// children. Computed here rather than stored because a stored copy would need
// updating from six different mutations and would be wrong the first time one
// was missed. Callers hold ts.mu.
func (ts *TodoStore) fillRollups(out []Todo) {
	kids := map[string]*TodoRollup{}
	for _, t := range ts.todos {
		if t.ParentID == "" {
			continue
		}
		r := kids[t.ParentID]
		if r == nil {
			r = &TodoRollup{}
			kids[t.ParentID] = r
		}
		r.Children++
		r.Estimate += t.Estimate
		if ts.IsDoneStatus(t.Status) {
			r.Done++
			r.EstimateDone += t.Estimate
		}
	}
	for i := range out {
		if r, ok := kids[out[i].ID]; ok {
			c := *r
			out[i].Rollup = &c
		}
	}
}

// todoCreate is the full new-card input. Create keeps the original four-string
// signature for the callers that only ever set those.
type todoCreate struct {
	Title    string  `json:"title"`
	Note     string  `json:"note"`
	Repo     string  `json:"repo"`
	Status   string  `json:"status"`
	Kind     string  `json:"kind"`
	ParentID string  `json:"parentId"`
	Priority int     `json:"priority"`
	Estimate float64 `json:"estimate"`
	CycleID  string  `json:"cycleId"`
}

// Create appends a new todo at the bottom of the given column
// (default backlog).
func (ts *TodoStore) Create(title, note, repo, status string) (Todo, error) {
	return ts.CreateFull(todoCreate{Title: title, Note: note, Repo: repo, Status: status})
}

// CreateFull appends a new card, with the depth fields set up front so an epic
// arrives as an epic rather than as a task that is immediately patched.
func (ts *TodoStore) CreateFull(in todoCreate) (Todo, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Todo{}, fmt.Errorf("title is required")
	}
	status := in.Status
	if status == "" {
		status = "backlog"
	}
	if !ts.validStatus(status) {
		return Todo{}, fmt.Errorf("unknown status %q — see GET /api/board/states", status)
	}
	kind := todoKindOr(in.Kind)
	if !validTodoKind(kind) {
		return Todo{}, fmt.Errorf("kind must be epic, story, task or bug")
	}
	if in.Priority < 0 || in.Priority > 4 {
		return Todo{}, fmt.Errorf("priority must be 0-4")
	}
	if in.Estimate < 0 {
		return Todo{}, fmt.Errorf("estimate cannot be negative")
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	maxOrder, maxSeq := 0.0, 0
	for _, t := range ts.todos {
		if t.Status == status && t.Order > maxOrder {
			maxOrder = t.Order
		}
		if t.Seq > maxSeq {
			maxSeq = t.Seq
		}
	}
	if err := ts.checkParent("", in.ParentID); err != nil {
		return Todo{}, err
	}
	todo := &Todo{
		ID:        randomID(),
		Seq:       maxSeq + 1,
		Title:     title,
		Note:      strings.TrimSpace(in.Note),
		Repo:      strings.TrimSpace(in.Repo),
		Status:    status,
		Order:     maxOrder + 1,
		CreatedAt: time.Now(),
		ParentID:  in.ParentID,
		Kind:      kind,
		Priority:  in.Priority,
		Estimate:  in.Estimate,
		CycleID:   in.CycleID,
	}
	if err := ts.persist(todo); err != nil {
		return Todo{}, err
	}
	ts.todos = append(ts.todos, todo)
	ts.log(todo.ID, "created", "", status)
	return *todo, nil
}

// checkParent rejects a nesting the board cannot draw. The two rules together
// make a cycle unrepresentable: a parent must itself be top level, and a card
// that already has children cannot become someone's child. id is "" when the
// card being parented does not exist yet. Callers hold ts.mu.
func (ts *TodoStore) checkParent(id, parentID string) error {
	if parentID == "" {
		return nil
	}
	if parentID == id {
		return fmt.Errorf("a card cannot be its own parent")
	}
	var parent *Todo
	for _, t := range ts.todos {
		if t.ID == parentID {
			parent = t
			break
		}
	}
	if parent == nil {
		return fmt.Errorf("unknown parent id %q", parentID)
	}
	if parent.ParentID != "" {
		return fmt.Errorf("%q is already nested — the board goes two levels deep", parentID)
	}
	if id != "" {
		for _, t := range ts.todos {
			if t.ParentID == id {
				return fmt.Errorf("this card has children; nesting it would go three levels deep")
			}
		}
	}
	return nil
}

// Update applies the non-nil patch fields to one todo.
func (ts *TodoStore) Update(id string, p todoPatch) (Todo, error) {
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return Todo{}, fmt.Errorf("title is required")
	}
	if p.Status != nil && !ts.validStatus(*p.Status) {
		return Todo{}, fmt.Errorf("unknown status %q — see GET /api/board/states", *p.Status)
	}
	if p.Kind != nil && !validTodoKind(todoKindOr(*p.Kind)) {
		return Todo{}, fmt.Errorf("kind must be epic, story, task or bug")
	}
	if p.Priority != nil && (*p.Priority < 0 || *p.Priority > 4) {
		return Todo{}, fmt.Errorf("priority must be 0-4")
	}
	if p.Estimate != nil && *p.Estimate < 0 {
		return Todo{}, fmt.Errorf("estimate cannot be negative")
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.todos {
		if t.ID != id {
			continue
		}
		// Patch a copy and commit it to memory only after the row write
		// succeeds, so a failed write can't leave the served board diverged
		// from data.db.
		next := *t
		if p.Title != nil {
			next.Title = strings.TrimSpace(*p.Title)
		}
		if p.Note != nil {
			next.Note = strings.TrimSpace(*p.Note)
		}
		if p.Repo != nil {
			next.Repo = strings.TrimSpace(*p.Repo)
		}
		if p.Labels != nil {
			next.Labels = cleanStrings(*p.Labels)
		}
		if p.Status != nil {
			next.Status = *p.Status
		}
		if p.Order != nil {
			next.Order = *p.Order
		}
		if p.LinkedSessionIDs != nil {
			next.LinkedSessionIDs = cleanStrings(*p.LinkedSessionIDs)
		}
		if p.LinkedDrawingIDs != nil {
			next.LinkedDrawingIDs = cleanStrings(*p.LinkedDrawingIDs)
		}
		if p.LinkedDocIDs != nil {
			next.LinkedDocIDs = cleanStrings(*p.LinkedDocIDs)
		}
		if p.ParentID != nil {
			if err := ts.checkParent(t.ID, *p.ParentID); err != nil {
				return Todo{}, err
			}
			next.ParentID = *p.ParentID
		}
		if p.Kind != nil {
			next.Kind = todoKindOr(*p.Kind)
		}
		if p.Priority != nil {
			next.Priority = *p.Priority
		}
		if p.Estimate != nil {
			next.Estimate = *p.Estimate
		}
		if p.CycleID != nil {
			next.CycleID = *p.CycleID
		}
		if err := ts.persist(&next); err != nil {
			return Todo{}, err
		}
		// The event log is written only after the row lands, so history never
		// records a move data.db refused. Only the fields a burndown or an
		// activity feed reads are logged — a retitled card is not history.
		if p.Status != nil && next.Status != t.Status {
			ts.log(t.ID, "status", t.Status, next.Status)
		}
		if p.Estimate != nil && next.Estimate != t.Estimate {
			ts.log(t.ID, "estimate", formatPoints(t.Estimate), formatPoints(next.Estimate))
		}
		if p.CycleID != nil && next.CycleID != t.CycleID {
			ts.log(t.ID, "cycle", t.CycleID, next.CycleID)
		}
		if p.ParentID != nil && next.ParentID != t.ParentID {
			ts.log(t.ID, "parent", t.ParentID, next.ParentID)
		}
		if p.Priority != nil && next.Priority != t.Priority {
			ts.log(t.ID, "priority", strconv.Itoa(t.Priority), strconv.Itoa(next.Priority))
		}
		*t = next
		return next, nil
	}
	return Todo{}, errTodoNotFound
}

// formatPoints renders an estimate the way the event log stores it — trimmed,
// so 3 is "3" and not "3.000000".
func formatPoints(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// SetSnapshot stores (or clears, with nil) a todo's frozen session numbers.
func (ts *TodoStore) SetSnapshot(id string, snap *TodoSnapshot) (Todo, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for _, t := range ts.todos {
		if t.ID == id {
			next := *t
			next.Snapshot = snap
			if err := ts.persist(&next); err != nil {
				return Todo{}, err
			}
			*t = next
			return next, nil
		}
	}
	return Todo{}, errTodoNotFound
}

// UnlinkDrawing removes a (deleted) drawing's id from every card, reporting
// whether anything changed.
func (ts *TodoStore) UnlinkDrawing(drawingID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	changed := false
	for _, t := range ts.todos {
		kept := t.LinkedDrawingIDs[:0:0]
		for _, id := range t.LinkedDrawingIDs {
			if id != drawingID {
				kept = append(kept, id)
			}
		}
		if len(kept) != len(t.LinkedDrawingIDs) {
			next := *t
			next.LinkedDrawingIDs = kept
			// Best-effort cleanup of a dangling reference — one card's failed
			// write must not sink the others.
			if err := ts.persist(&next); err != nil {
				log.Printf("todos: unlink drawing %s: %v", drawingID, err)
				continue
			}
			*t = next
			changed = true
		}
	}
	return changed
}

// UnlinkDoc removes a (deleted) doc's id from every card, reporting whether
// anything changed.
func (ts *TodoStore) UnlinkDoc(docID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	changed := false
	for _, t := range ts.todos {
		kept := t.LinkedDocIDs[:0:0]
		for _, id := range t.LinkedDocIDs {
			if id != docID {
				kept = append(kept, id)
			}
		}
		if len(kept) != len(t.LinkedDocIDs) {
			next := *t
			next.LinkedDocIDs = kept
			// Best-effort cleanup of a dangling reference — one card's failed
			// write must not sink the others.
			if err := ts.persist(&next); err != nil {
				log.Printf("todos: unlink doc %s: %v", docID, err)
				continue
			}
			*t = next
			changed = true
		}
	}
	return changed
}

// RenameRepo relabels every card carrying the old project name — the board's
// half of a project rename. Returns how many cards changed.
func (ts *TodoStore) RenameRepo(old, name string) int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if _, err := ts.db.Exec(`UPDATE todos SET repo=? WHERE repo=?`, name, old); err != nil {
		log.Printf("todos: rename repo: %v", err)
		return 0
	}
	n := 0
	for _, t := range ts.todos {
		if t.Repo == old {
			t.Repo = name
			n++
		}
	}
	return n
}

// UnlinkCycle clears a (deleted) cycle from every card that was planned into
// it, reporting whether anything changed — the cycles analogue of UnlinkDoc.
func (ts *TodoStore) UnlinkCycle(cycleID string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	changed := false
	for _, t := range ts.todos {
		if t.CycleID != cycleID {
			continue
		}
		next := *t
		next.CycleID = ""
		if err := ts.persist(&next); err != nil {
			log.Printf("todos: unlink cycle %s: %v", cycleID, err)
			continue
		}
		*t = next
		changed = true
	}
	return changed
}

// Delete removes one card. Its children are PROMOTED to top level rather than
// deleted with it: cascading would destroy user data the delete never named,
// and refusing outright would strand an epic you can no longer remove.
func (ts *TodoStore) Delete(id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, t := range ts.todos {
		if t.ID != id {
			continue
		}
		if err := ts.removeRow(id); err != nil {
			return err
		}
		ts.todos = append(ts.todos[:i], ts.todos[i+1:]...)
		for _, c := range ts.todos {
			if c.ParentID != id {
				continue
			}
			next := *c
			next.ParentID = ""
			if err := ts.persist(&next); err != nil {
				log.Printf("todos: promote child %s: %v", c.ID, err)
				continue
			}
			*c = next
			ts.log(c.ID, "parent", id, "")
		}
		return nil
	}
	return errTodoNotFound
}
