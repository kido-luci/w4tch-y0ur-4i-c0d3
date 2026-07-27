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
	"os"
	"sort"
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

func (ts *TodoStore) loadDB() {
	rows, err := ts.db.Query(`SELECT id,seq,title,note,repo,labels,status,ord,created_at,linked_sessions,linked_drawings,linked_docs,snapshot FROM todos`)
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
			&t.Order, &created, &linkedS, &linkedD, &linkedDoc, &snap); err != nil {
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
		`UPDATE todos SET seq=?,title=?,note=?,repo=?,labels=?,status=?,ord=?,created_at=?,linked_sessions=?,linked_drawings=?,linked_docs=?,snapshot=? WHERE id=?`,
		t.Seq, t.Title, t.Note, t.Repo, jsonList(t.Labels), t.Status, t.Order,
		timeToNano(t.CreatedAt), jsonList(t.LinkedSessionIDs), jsonList(t.LinkedDrawingIDs), jsonList(t.LinkedDocIDs), snap, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = q.Exec(
		`INSERT INTO todos(id,seq,title,note,repo,labels,status,ord,created_at,linked_sessions,linked_drawings,linked_docs,snapshot) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Seq, t.Title, t.Note, t.Repo, jsonList(t.Labels), t.Status, t.Order,
		timeToNano(t.CreatedAt), jsonList(t.LinkedSessionIDs), jsonList(t.LinkedDrawingIDs), jsonList(t.LinkedDocIDs), snap)
	return err
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

func sortTodos(todos []Todo) {
	sort.SliceStable(todos, func(i, j int) bool {
		if a, b := todoStatusRank[todos[i].Status], todoStatusRank[todos[j].Status]; a != b {
			return a < b
		}
		if todos[i].Order != todos[j].Order {
			return todos[i].Order < todos[j].Order
		}
		return todos[i].CreatedAt.Before(todos[j].CreatedAt)
	})
}

// List returns every todo, column-ordered (backlog, doing, done; Order within).
func (ts *TodoStore) List() []Todo {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]Todo, 0, len(ts.todos))
	for _, t := range ts.todos {
		out = append(out, *t)
	}
	sortTodos(out)
	return out
}

// Create appends a new todo at the bottom of the given column
// (default backlog).
func (ts *TodoStore) Create(title, note, repo, status string) (Todo, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Todo{}, fmt.Errorf("title is required")
	}
	if status == "" {
		status = "backlog"
	}
	if !validTodoStatus(status) {
		return Todo{}, fmt.Errorf("status must be backlog, doing or done")
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
	todo := &Todo{
		ID:        randomID(),
		Seq:       maxSeq + 1,
		Title:     title,
		Note:      strings.TrimSpace(note),
		Repo:      strings.TrimSpace(repo),
		Status:    status,
		Order:     maxOrder + 1,
		CreatedAt: time.Now(),
	}
	if err := ts.persist(todo); err != nil {
		return Todo{}, err
	}
	ts.todos = append(ts.todos, todo)
	return *todo, nil
}

// Update applies the non-nil patch fields to one todo.
func (ts *TodoStore) Update(id string, p todoPatch) (Todo, error) {
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return Todo{}, fmt.Errorf("title is required")
	}
	if p.Status != nil && !validTodoStatus(*p.Status) {
		return Todo{}, fmt.Errorf("status must be backlog, doing or done")
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
		if err := ts.persist(&next); err != nil {
			return Todo{}, err
		}
		*t = next
		return next, nil
	}
	return Todo{}, errTodoNotFound
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

// Delete removes one todo.
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

func (ts *TodoStore) Delete(id string) error {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for i, t := range ts.todos {
		if t.ID == id {
			if err := ts.removeRow(id); err != nil {
				return err
			}
			ts.todos = append(ts.todos[:i], ts.todos[i+1:]...)
			return nil
		}
	}
	return errTodoNotFound
}
