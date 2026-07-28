package main

// Saved board views — a named filter plus the shape it renders in (board,
// table or timeline). "My bugs this cycle" stops being something you re-type
// and becomes something you click.
//
// Query is stored as opaque JSON and never parsed server-side. The filter
// vocabulary belongs to the client that renders it, and freezing it into a Go
// struct would mean a migration every time the board grows a chip. The server
// only guarantees the blob comes back byte-identical.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
)

// BoardView is one saved view. Repo scopes it ("" = every scope).
type BoardView struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Repo  string          `json:"repo,omitempty"`
	Kind  string          `json:"kind"` // board | table | timeline
	Query json.RawMessage `json:"query"`
	Order float64         `json:"order"`
}

func validViewKind(k string) bool {
	return k == "board" || k == "table" || k == "timeline"
}

var errViewNotFound = errors.New("view not found")

// ViewStore persists the saved views to data.db (board_views).
type ViewStore struct {
	db *sql.DB

	mu    sync.Mutex
	views []*BoardView
}

func NewViewStore(db *sql.DB) *ViewStore {
	vs := &ViewStore{db: db}
	vs.loadDB()
	return vs
}

func (vs *ViewStore) loadDB() {
	rows, err := vs.db.Query(`SELECT id,name,repo,kind,query,ord FROM board_views`)
	if err != nil {
		log.Printf("views: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		v := &BoardView{}
		var q string
		if err := rows.Scan(&v.ID, &v.Name, &v.Repo, &v.Kind, &q, &v.Order); err != nil {
			log.Printf("views: load row: %v", err)
			continue
		}
		v.Query = json.RawMessage(q)
		vs.views = append(vs.views, v)
	}
}

// persist writes one view through. Callers hold vs.mu.
func (vs *ViewStore) persist(v *BoardView) error {
	q := string(v.Query)
	if q == "" {
		q = "{}"
	}
	res, err := vs.db.Exec(`UPDATE board_views SET name=?,repo=?,kind=?,query=?,ord=? WHERE id=?`,
		v.Name, v.Repo, v.Kind, q, v.Order, v.ID)
	if err != nil {
		return fmt.Errorf("persist view: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := vs.db.Exec(`INSERT INTO board_views(id,name,repo,kind,query,ord) VALUES(?,?,?,?,?,?)`,
		v.ID, v.Name, v.Repo, v.Kind, q, v.Order); err != nil {
		return fmt.Errorf("persist view: %w", err)
	}
	return nil
}

func sortViews(out []BoardView) {
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
}

// List returns every saved view.
func (vs *ViewStore) List() []BoardView {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	out := make([]BoardView, 0, len(vs.views))
	for _, v := range vs.views {
		out = append(out, *v)
	}
	sortViews(out)
	return out
}

// ListFor returns the views one scope sees: shared plus that project's own.
func (vs *ViewStore) ListFor(repo string) []BoardView {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	out := make([]BoardView, 0, len(vs.views))
	for _, v := range vs.views {
		if v.Repo == "" || v.Repo == repo {
			out = append(out, *v)
		}
	}
	sortViews(out)
	return out
}

// Create saves a new view at the end of the list.
func (vs *ViewStore) Create(name, repo, kind string, query json.RawMessage) (BoardView, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return BoardView{}, fmt.Errorf("name is required")
	}
	if kind == "" {
		kind = "board"
	}
	if !validViewKind(kind) {
		return BoardView{}, fmt.Errorf("kind must be board, table or timeline")
	}
	if len(query) > 0 && !json.Valid(query) {
		return BoardView{}, fmt.Errorf("query must be valid JSON")
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	maxOrder := 0.0
	for _, v := range vs.views {
		if v.Order > maxOrder {
			maxOrder = v.Order
		}
	}
	v := &BoardView{
		ID:    randomID(),
		Name:  name,
		Repo:  strings.TrimSpace(repo),
		Kind:  kind,
		Query: query,
		Order: maxOrder + 1,
	}
	if err := vs.persist(v); err != nil {
		return BoardView{}, err
	}
	vs.views = append(vs.views, v)
	return *v, nil
}

// viewPatch is a partial saved-view update; nil fields stay untouched.
type viewPatch struct {
	Name  *string          `json:"name"`
	Kind  *string          `json:"kind"`
	Query *json.RawMessage `json:"query"`
	Order *float64         `json:"order"`
}

func (vs *ViewStore) Update(id string, p viewPatch) (BoardView, error) {
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		return BoardView{}, fmt.Errorf("name is required")
	}
	if p.Kind != nil && !validViewKind(*p.Kind) {
		return BoardView{}, fmt.Errorf("kind must be board, table or timeline")
	}
	if p.Query != nil && len(*p.Query) > 0 && !json.Valid(*p.Query) {
		return BoardView{}, fmt.Errorf("query must be valid JSON")
	}
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for _, v := range vs.views {
		if v.ID != id {
			continue
		}
		next := *v
		if p.Name != nil {
			next.Name = strings.TrimSpace(*p.Name)
		}
		if p.Kind != nil {
			next.Kind = *p.Kind
		}
		if p.Query != nil {
			next.Query = *p.Query
		}
		if p.Order != nil {
			next.Order = *p.Order
		}
		if err := vs.persist(&next); err != nil {
			return BoardView{}, err
		}
		*v = next
		return next, nil
	}
	return BoardView{}, errViewNotFound
}

func (vs *ViewStore) Delete(id string) error {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	for i, v := range vs.views {
		if v.ID != id {
			continue
		}
		if _, err := vs.db.Exec(`DELETE FROM board_views WHERE id=?`, id); err != nil {
			return fmt.Errorf("delete view: %w", err)
		}
		vs.views = append(vs.views[:i], vs.views[i+1:]...)
		return nil
	}
	return errViewNotFound
}

// RenameRepo re-points a project's saved views at its new name.
func (vs *ViewStore) RenameRepo(old, name string) int {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if _, err := vs.db.Exec(`UPDATE board_views SET repo=? WHERE repo=?`, name, old); err != nil {
		log.Printf("views: rename repo: %v", err)
		return 0
	}
	n := 0
	for _, v := range vs.views {
		if v.Repo == old {
			v.Repo = name
			n++
		}
	}
	return n
}
