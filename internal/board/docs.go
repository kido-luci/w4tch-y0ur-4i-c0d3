package board

// Docs wiki (route `#/docs`): a tree of markdown pages, Confluence-style. Lives
// in data.db (docs + doc_backups) with the same write model as the drawing and
// todo stores: this server is the single writer, an in-memory slice guarded by
// a mutex is the serving copy, and every mutation writes its one row through an
// explicit column list. Like drawings, the page body is read/written only
// through Content/SetContent (kept out of the List payload so the tree stays
// light), body overwrites rotate a .bak backup, and a non-zero base makes the
// write a compare-and-swap so a human edit and an MCP write don't clobber each
// other.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Doc is one wiki page's metadata; the markdown body is only read/written
// through Content/SetContent. ParentID "" is a root page; Order sorts siblings.
type Doc struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	ParentID string `json:"parentId"`
	// Group is the project scope this page belongs to (the nav's global
	// project switcher): a project name or free-text label. "" inherits —
	// the page follows its nearest claimed ancestor (an unclaimed ROOT is
	// unscoped, visible only under "all projects"); a child's own group
	// overrides, lifting it to the top of that scope's tree. Metadata-only,
	// so changing it never bumps UpdatedAt (the body-write conflict base).
	Group     string    `json:"group"`
	Order     float64   `json:"order"`
	CreatedAt time.Time `json:"createdAt"`
	// UpdatedAt is the BODY version: a content write bumps it, metadata edits
	// (title / parent / order) do not. It is the conflict base for body writes,
	// so a rename mustn't move it out from under an in-flight save.
	UpdatedAt time.Time `json:"updatedAt"`
}

var ErrDocNotFound = errors.New("doc not found")

// ErrDocConflict means a conditional body write's base version no longer
// matches: someone else (the editor, another tab, an MCP client) saved since.
var ErrDocConflict = errors.New("doc changed since base version")

// ErrDocCycle means a move would make a page its own ancestor.
var ErrDocCycle = errors.New("a page cannot be moved under itself or its descendants")

// maxDocBackups is how many previous body versions are kept per page (slot 1 =
// newest). Every overwrite rotates them, so a bad write — human or MCP — is
// recoverable, exactly like the drawing scene backups.
const maxDocBackups = 5

// DocStore persists the wiki to data.db (docs + doc_backups).
type DocStore struct {
	db *sql.DB

	mu   sync.Mutex
	docs []*Doc
}

// NewDocStore opens the wiki over data.db.
func NewDocStore(db *sql.DB) *DocStore {
	ds := &DocStore{db: db}
	ds.loadDB()
	return ds
}

func (ds *DocStore) loadDB() {
	rows, err := ds.db.Query(`SELECT id,title,parent_id,group_name,ord,created_at,updated_at FROM docs`)
	if err != nil {
		log.Printf("docs: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		d := &Doc{}
		var created, updated int64
		if err := rows.Scan(&d.ID, &d.Title, &d.ParentID, &d.Group, &d.Order, &created, &updated); err != nil {
			log.Printf("docs: load row: %v", err)
			continue
		}
		d.CreatedAt = nanoToTime(created)
		d.UpdatedAt = nanoToTime(updated)
		ds.docs = append(ds.docs, d)
	}
}

// find returns the entry for id, or nil. Callers hold ds.mu.
func (ds *DocStore) find(id string) *Doc {
	for _, d := range ds.docs {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// List returns every page, sorted by Order then CreatedAt. The client groups by
// ParentID to build the tree; grouping preserves each sibling set's order.
func (ds *DocStore) List() []Doc {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]Doc, 0, len(ds.docs))
	for _, d := range ds.docs {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// maxSiblingOrder returns the largest Order among children of parentID (0 when
// there are none). Callers hold ds.mu.
func (ds *DocStore) maxSiblingOrder(parentID string) float64 {
	max := 0.0
	for _, d := range ds.docs {
		if d.ParentID == parentID && d.Order > max {
			max = d.Order
		}
	}
	return max
}

// Create adds a new page under parentID (""=root) with an empty body, appended
// after its siblings, in the given project scope ("" = inherit from the parent
// tree; on a root, "" means unscoped).
func (ds *DocStore) Create(title, parentID, group string) (Doc, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Doc{}, fmt.Errorf("title is required")
	}
	parentID = strings.TrimSpace(parentID)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if parentID != "" && ds.find(parentID) == nil {
		return Doc{}, fmt.Errorf("parent %q does not exist", parentID)
	}
	now := time.Now()
	d := &Doc{
		ID:        randomID(),
		Title:     title,
		ParentID:  parentID,
		Group:     strings.TrimSpace(group),
		Order:     ds.maxSiblingOrder(parentID) + 1,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if _, err := ds.db.Exec(
		`INSERT INTO docs(id,title,parent_id,group_name,ord,created_at,updated_at,body) VALUES(?,?,?,?,?,?,?,'')`,
		d.ID, d.Title, d.ParentID, d.Group, d.Order, d.CreatedAt.UnixNano(), d.UpdatedAt.UnixNano()); err != nil {
		return Doc{}, fmt.Errorf("write doc: %w", err)
	}
	ds.docs = append(ds.docs, d)
	return *d, nil
}

// Get returns one page's metadata.
func (ds *DocStore) Get(id string) (Doc, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Doc{}, ErrDocNotFound
	}
	return *d, nil
}

// bodyLocked reads one page's markdown body (empty string when the row is
// missing its body). Callers hold ds.mu.
func (ds *DocStore) bodyLocked(id string) string {
	var b string
	if err := ds.db.QueryRow(`SELECT body FROM docs WHERE id=?`, id).Scan(&b); err != nil {
		return ""
	}
	return b
}

// Content returns one page's raw markdown body.
func (ds *DocStore) Content(id string) (string, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.find(id) == nil {
		return "", ErrDocNotFound
	}
	return ds.bodyLocked(id), nil
}

// rotateDocBackupsTx shifts id's backups one slot down (1 → 2, …) inside the
// caller's transaction and writes prev as the new slot 1, dropping the oldest.
func rotateDocBackupsTx(tx *sql.Tx, id, prev string) error {
	if _, err := tx.Exec(`DELETE FROM doc_backups WHERE doc_id=? AND slot>=?`, id, maxDocBackups); err != nil {
		return err
	}
	for n := maxDocBackups - 1; n >= 1; n-- {
		if _, err := tx.Exec(`UPDATE doc_backups SET slot=? WHERE doc_id=? AND slot=?`, n+1, id, n); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO doc_backups(doc_id,slot,content) VALUES(?,1,?)`, id, prev)
	return err
}

// SetContent replaces one page's markdown body, keeping the previous body as a
// rotated backup — body, backup rotation and the UpdatedAt bump are one
// transaction. A non-zero base makes the write conditional: it fails with
// ErrDocConflict unless base still equals the page's UpdatedAt (optimistic
// concurrency for the editor and MCP writers).
func (ds *DocStore) SetContent(id, body string, base time.Time) (Doc, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Doc{}, ErrDocNotFound
	}
	if !base.IsZero() && !base.Equal(d.UpdatedAt) {
		return Doc{}, ErrDocConflict
	}
	tx, err := ds.db.Begin()
	if err != nil {
		return Doc{}, fmt.Errorf("write doc: %w", err)
	}
	prev := ds.bodyLocked(id)
	if prev != body {
		if err := rotateDocBackupsTx(tx, id, prev); err != nil {
			tx.Rollback()
			return Doc{}, fmt.Errorf("write backup: %w", err)
		}
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE docs SET body=?, updated_at=? WHERE id=?`, body, now.UnixNano(), id); err != nil {
		tx.Rollback()
		return Doc{}, fmt.Errorf("write doc: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Doc{}, fmt.Errorf("write doc: %w", err)
	}
	d.UpdatedAt = now
	return *d, nil
}

// DocPatch is a partial metadata update; nil fields stay untouched. Body is not
// here — it goes through SetContent so metadata edits never move the conflict
// base a body save depends on.
type DocPatch struct {
	Title    *string  `json:"title"`
	ParentID *string  `json:"parentId"`
	Group    *string  `json:"group"` // "" is a real value — back to unscoped — distinct from "not provided"
	Order    *float64 `json:"order"`
}

// isDescendantLocked reports whether nodeID is ancestorID or sits anywhere
// beneath it, walking up via ParentID. Callers hold ds.mu.
func (ds *DocStore) isDescendantLocked(ancestorID, nodeID string) bool {
	for cur := nodeID; cur != ""; {
		if cur == ancestorID {
			return true
		}
		d := ds.find(cur)
		if d == nil {
			return false
		}
		cur = d.ParentID
	}
	return false
}

// Update applies the non-nil metadata fields to one page. Reparenting is
// cycle-checked (a page can't move under itself or a descendant) and, unless an
// explicit Order comes with it, drops the page at the end of its new siblings.
func (ds *DocStore) Update(id string, p DocPatch) (Doc, error) {
	if p.Title != nil && strings.TrimSpace(*p.Title) == "" {
		return Doc{}, fmt.Errorf("title is required")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Doc{}, ErrDocNotFound
	}
	// Compute the new values into locals and mutate the in-memory doc only after
	// the DB write succeeds — the DB-first rule the other mutators follow, so a
	// failed write can't leave the served tree diverged from data.db.
	newTitle, newParent, newGroup, newOrder := d.Title, d.ParentID, d.Group, d.Order
	if p.Title != nil {
		newTitle = strings.TrimSpace(*p.Title)
	}
	if p.Group != nil {
		newGroup = strings.TrimSpace(*p.Group)
	}
	if p.ParentID != nil {
		np := strings.TrimSpace(*p.ParentID)
		if np != d.ParentID {
			if np != "" && ds.find(np) == nil {
				return Doc{}, fmt.Errorf("parent %q does not exist", np)
			}
			if ds.isDescendantLocked(id, np) {
				return Doc{}, ErrDocCycle
			}
			newParent = np
			// d still sits under its old parent here, so maxSiblingOrder can't
			// count it among np's children.
			if p.Order == nil {
				newOrder = ds.maxSiblingOrder(np) + 1
			}
		}
	}
	if p.Order != nil {
		newOrder = *p.Order
	}
	if _, err := ds.db.Exec(`UPDATE docs SET title=?, parent_id=?, group_name=?, ord=? WHERE id=?`,
		newTitle, newParent, newGroup, newOrder, id); err != nil {
		return Doc{}, fmt.Errorf("update doc: %w", err)
	}
	d.Title, d.ParentID, d.Group, d.Order = newTitle, newParent, newGroup, newOrder
	return *d, nil
}

// Delete removes one page. Its children are promoted to the deleted page's own
// parent rather than cascade-deleted, so no page is ever lost with its parent —
// row, backups and the child re-parenting are one transaction.
// RenameGroup relabels every page whose project scope is the old name — the
// wiki's half of a project rename. Only directly-labelled pages change; a child
// that inherits its scope follows its (relabelled) root. Returns how many changed.
func (ds *DocStore) RenameGroup(old, name string) int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if _, err := ds.db.Exec(`UPDATE docs SET group_name=? WHERE group_name=?`, name, old); err != nil {
		log.Printf("docs: rename group: %v", err)
		return 0
	}
	n := 0
	for _, d := range ds.docs {
		if d.Group == old {
			d.Group = name
			n++
		}
	}
	return n
}

func (ds *DocStore) Delete(id string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	victim := ds.find(id)
	if victim == nil {
		return ErrDocNotFound
	}
	tx, err := ds.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE docs SET parent_id=? WHERE parent_id=?`, victim.ParentID, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM docs WHERE id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM doc_backups WHERE doc_id=?`, id); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	newDocs := ds.docs[:0]
	for _, d := range ds.docs {
		if d.ID == id {
			continue
		}
		if d.ParentID == id {
			d.ParentID = victim.ParentID
		}
		newDocs = append(newDocs, d)
	}
	ds.docs = newDocs
	return nil
}
