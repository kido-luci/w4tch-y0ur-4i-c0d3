package board

// Design library (route `#/design`): local Excalidraw wireframes. Since v0.45
// everything lives in data.db — metadata, scene, thumbnail and the rotated
// scene backups — so a scene write and its index update are one transaction
// (they were two files that could drift). Same write model as the todo store:
// this server is the single writer, in-memory metadata guarded by a mutex,
// every mutation writes its own row through with explicit columns. Scenes are
// standard .excalidraw JSON; export goes through Content/the API now that
// there is no per-drawing file to grab.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Drawing is one library entry's metadata; the scene itself is only
// read/written through Content/SetContent.
type Drawing struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Group is the tab this drawing belongs to (route `#/design`): a project
	// name or a free-text custom label. "" is the Ungrouped tab. Metadata-only,
	// so setting it does not bump UpdatedAt or invalidate the thumbnail.
	Group string `json:"group"`
	// Topics are free-text tags — many-to-many, unlike Group's one tab: the
	// scoped grid renders one section per topic and a drawing appears under
	// each of its tags. Never nil (JSON []), and metadata-only like Group.
	Topics    []string  `json:"topics"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// ThumbUpdatedAt is the UpdatedAt the cached thumbnail was rendered from.
	// The thumbnail is fresh iff it equals UpdatedAt — a content write bumps
	// UpdatedAt and thereby marks the thumbnail stale without touching it.
	ThumbUpdatedAt time.Time `json:"thumbUpdatedAt"`
	// PublishedAt is the UpdatedAt the last successful publish sent to the
	// review backend (same freshness idiom as ThumbUpdatedAt: the published
	// copy is current iff it equals UpdatedAt; zero = never published).
	PublishedAt time.Time `json:"publishedAt"`
}

var ErrDrawingNotFound = errors.New("drawing not found")

// ErrDrawingConflict means a conditional write's base version no longer
// matches: someone else (the editor, another tab, an MCP client) saved since
// the caller last read the drawing.
var ErrDrawingConflict = errors.New("drawing changed since base version")

// maxSceneBackups is how many previous scene versions are kept per drawing
// (slot 1 = newest). Every content overwrite rotates them, so a bad write —
// human or MCP — is recoverable.
const maxSceneBackups = 5

// emptyScene is the standard empty .excalidraw document a new drawing starts
// with (the same shape the editor saves back).
const emptyScene = `{
  "type": "excalidraw",
  "version": 2,
  "source": "watch-your-ai-code",
  "elements": [],
  "appState": {},
  "files": {}
}
`

// DrawingStore persists the library to data.db (drawings + drawing_backups).
type DrawingStore struct {
	db *sql.DB

	mu       sync.Mutex
	drawings []*Drawing
}

// NewDrawingStore opens the design library over data.db (import from the
// pre-v0.45 files has already run by then — see ImportOnce).
func NewDrawingStore(db *sql.DB) *DrawingStore {
	ds := &DrawingStore{db: db}
	ds.loadDB()
	return ds
}

func (ds *DrawingStore) loadDB() {
	rows, err := ds.db.Query(`SELECT id,name,group_name,topics,created_at,updated_at,thumb_updated_at,published_at FROM drawings`)
	if err != nil {
		log.Printf("drawings: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		d := &Drawing{}
		var topics string
		var created, updated, thumbAt, publishedAt int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Group, &topics, &created, &updated, &thumbAt, &publishedAt); err != nil {
			log.Printf("drawings: load row: %v", err)
			continue
		}
		if err := json.Unmarshal([]byte(topics), &d.Topics); err != nil || d.Topics == nil {
			d.Topics = []string{}
		}
		d.CreatedAt = nanoToTime(created)
		d.UpdatedAt = nanoToTime(updated)
		d.ThumbUpdatedAt = nanoToTime(thumbAt)
		d.PublishedAt = nanoToTime(publishedAt)
		ds.drawings = append(ds.drawings, d)
	}
}

// find returns the entry for id, or nil. Callers hold ds.mu.
func (ds *DrawingStore) find(id string) *Drawing {
	for _, d := range ds.drawings {
		if d.ID == id {
			return d
		}
	}
	return nil
}

// List returns every drawing, most recently updated first.
func (ds *DrawingStore) List() []Drawing {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	out := make([]Drawing, 0, len(ds.drawings))
	for _, d := range ds.drawings {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].UpdatedAt.After(out[j].UpdatedAt)
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

// Create adds a new drawing with an empty scene, in the given group tab (""
// for Ungrouped).
func (ds *DrawingStore) Create(name, group string) (Drawing, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Drawing{}, fmt.Errorf("name is required")
	}
	group = strings.TrimSpace(group)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	now := time.Now()
	d := &Drawing{ID: randomID(), Name: name, Group: group, Topics: []string{}, CreatedAt: now, UpdatedAt: now}
	if _, err := ds.db.Exec(
		`INSERT INTO drawings(id,name,group_name,created_at,updated_at,thumb_updated_at,scene,thumb) VALUES(?,?,?,?,?,0,?,NULL)`,
		d.ID, d.Name, d.Group, d.CreatedAt.UnixNano(), d.UpdatedAt.UnixNano(), []byte(emptyScene)); err != nil {
		return Drawing{}, fmt.Errorf("write scene: %w", err)
	}
	ds.drawings = append(ds.drawings, d)
	return *d, nil
}

// Get returns one drawing's metadata.
func (ds *DrawingStore) Get(id string) (Drawing, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	return *d, nil
}

// Duplicate creates a new drawing carrying a copy of id's scene — a cheap
// fork for iterating on variants. The copy starts without a thumbnail; the
// grid renders one on its next view.
func (ds *DrawingStore) Duplicate(id string) (Drawing, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	src := ds.find(id)
	if src == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	content := ds.sceneLocked(id)
	now := time.Now()
	d := &Drawing{ID: randomID(), Name: src.Name + " (copy)", Group: src.Group,
		Topics: append([]string{}, src.Topics...), CreatedAt: now, UpdatedAt: now}
	topics, err := json.Marshal(d.Topics)
	if err != nil {
		return Drawing{}, fmt.Errorf("marshal topics: %w", err)
	}
	if _, err := ds.db.Exec(
		`INSERT INTO drawings(id,name,group_name,topics,created_at,updated_at,thumb_updated_at,scene,thumb) VALUES(?,?,?,?,?,?,0,?,NULL)`,
		d.ID, d.Name, d.Group, string(topics), d.CreatedAt.UnixNano(), d.UpdatedAt.UnixNano(), content); err != nil {
		return Drawing{}, fmt.Errorf("write scene: %w", err)
	}
	ds.drawings = append(ds.drawings, d)
	return *d, nil
}

// sceneLocked reads one scene blob, degrading to the empty scene the same way
// the file store did for a vanished file. Callers hold ds.mu.
func (ds *DrawingStore) sceneLocked(id string) []byte {
	var b []byte
	if err := ds.db.QueryRow(`SELECT scene FROM drawings WHERE id=?`, id).Scan(&b); err != nil || len(b) == 0 {
		return []byte(emptyScene)
	}
	return b
}

// Content returns one drawing's raw .excalidraw JSON.
func (ds *DrawingStore) Content(id string) ([]byte, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if ds.find(id) == nil {
		return nil, ErrDrawingNotFound
	}
	return ds.sceneLocked(id), nil
}

// errThumbnailStale means the cached thumbnail predates the current scene (or
// was never rendered) — clients regenerate and re-upload.
var errThumbnailStale = errors.New("thumbnail missing or stale")

// SetThumbnail stores a client-rendered thumbnail for the scene version
// `base` (the drawing's UpdatedAt the client rendered from). A base that is
// already stale is stored anyway — it just stays flagged stale and the next
// grid view re-renders it.
func (ds *DrawingStore) SetThumbnail(id string, data []byte, base time.Time) (Drawing, error) {
	if base.IsZero() {
		return Drawing{}, fmt.Errorf("base version is required")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if _, err := ds.db.Exec(`UPDATE drawings SET thumb=?, thumb_updated_at=? WHERE id=?`,
		data, timeToNano(base), id); err != nil {
		return Drawing{}, fmt.Errorf("write thumbnail: %w", err)
	}
	d.ThumbUpdatedAt = base
	return *d, nil
}

// Thumbnail returns the cached thumbnail PNG, or errThumbnailStale when it
// was never rendered or predates the current scene.
func (ds *DrawingStore) Thumbnail(id string) ([]byte, error) {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return nil, ErrDrawingNotFound
	}
	if d.ThumbUpdatedAt.IsZero() || !d.ThumbUpdatedAt.Equal(d.UpdatedAt) {
		return nil, errThumbnailStale
	}
	var b []byte
	if err := ds.db.QueryRow(`SELECT thumb FROM drawings WHERE id=?`, id).Scan(&b); err != nil || len(b) == 0 {
		return nil, errThumbnailStale
	}
	return b, nil
}

// rotateBackupsTx shifts id's backups one slot down (1 → 2, …) inside the
// caller's transaction and writes prev as the new slot 1, dropping the oldest.
func rotateBackupsTx(tx *sql.Tx, id string, prev []byte) error {
	if _, err := tx.Exec(`DELETE FROM drawing_backups WHERE drawing_id=? AND slot>=?`, id, maxSceneBackups); err != nil {
		return err
	}
	for n := maxSceneBackups - 1; n >= 1; n-- {
		if _, err := tx.Exec(`UPDATE drawing_backups SET slot=? WHERE drawing_id=? AND slot=?`, n+1, id, n); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`INSERT INTO drawing_backups(drawing_id,slot,content) VALUES(?,1,?)`, id, prev)
	return err
}

// SetContent replaces one drawing's scene, keeping the previous scene as a
// rotated backup — scene, backup rotation and the UpdatedAt bump are one
// transaction (in the file store these were separate writes that could
// drift). A non-zero base makes the write conditional: it fails with
// ErrDrawingConflict unless base still equals the drawing's UpdatedAt
// (optimistic concurrency for the editor and MCP writers).
func (ds *DrawingStore) SetContent(id string, content []byte, base time.Time) (Drawing, error) {
	if !json.Valid(content) {
		return Drawing{}, fmt.Errorf("content must be valid JSON")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if !base.IsZero() && !base.Equal(d.UpdatedAt) {
		return Drawing{}, ErrDrawingConflict
	}
	tx, err := ds.db.Begin()
	if err != nil {
		return Drawing{}, fmt.Errorf("write scene: %w", err)
	}
	prev := ds.sceneLocked(id)
	if !bytes.Equal(prev, content) {
		if err := rotateBackupsTx(tx, id, prev); err != nil {
			tx.Rollback()
			return Drawing{}, fmt.Errorf("write backup: %w", err)
		}
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE drawings SET scene=?, updated_at=? WHERE id=?`,
		content, now.UnixNano(), id); err != nil {
		tx.Rollback()
		return Drawing{}, fmt.Errorf("write scene: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Drawing{}, fmt.Errorf("write scene: %w", err)
	}
	d.UpdatedAt = now
	return *d, nil
}

// MarkPublished records that scene version `base` (the drawing's UpdatedAt at
// publish time) was pushed to the review backend. Mirrors the thumbnail
// freshness contract: if the drawing changed while the publish was in flight,
// PublishedAt simply stays != UpdatedAt and the UI shows "shared (stale)".
func (ds *DrawingStore) MarkPublished(id string, base time.Time) (Drawing, error) {
	if base.IsZero() {
		return Drawing{}, fmt.Errorf("base version is required")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if _, err := ds.db.Exec(`UPDATE drawings SET published_at=? WHERE id=?`, base.UnixNano(), id); err != nil {
		return Drawing{}, fmt.Errorf("mark published: %w", err)
	}
	d.PublishedAt = base
	return *d, nil
}

// Rename changes one drawing's display name.
func (ds *DrawingStore) Rename(id, name string) (Drawing, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Drawing{}, fmt.Errorf("name is required")
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if _, err := ds.db.Exec(`UPDATE drawings SET name=? WHERE id=?`, name, id); err != nil {
		return Drawing{}, fmt.Errorf("rename: %w", err)
	}
	d.Name = name
	return *d, nil
}

// SetGroup moves one drawing to a group tab: a project name or a custom label,
// or "" to return it to Ungrouped. Metadata-only, so — like Rename — it does
// not bump UpdatedAt or invalidate the cached thumbnail.
func (ds *DrawingStore) SetGroup(id, group string) (Drawing, error) {
	group = strings.TrimSpace(group)
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if _, err := ds.db.Exec(`UPDATE drawings SET group_name=? WHERE id=?`, group, id); err != nil {
		return Drawing{}, fmt.Errorf("set group: %w", err)
	}
	d.Group = group
	return *d, nil
}

// SetTopics replaces one drawing's topic tags (the full new set, not a
// delta; empty untags it). Trimmed, blanks dropped, duplicates collapsed.
// Metadata-only, so — like SetGroup — it does not bump UpdatedAt or
// invalidate the cached thumbnail.
func (ds *DrawingStore) SetTopics(id string, topics []string) (Drawing, error) {
	clean := make([]string, 0, len(topics))
	seen := map[string]bool{}
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		clean = append(clean, t)
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return Drawing{}, fmt.Errorf("marshal topics: %w", err)
	}
	ds.mu.Lock()
	defer ds.mu.Unlock()
	d := ds.find(id)
	if d == nil {
		return Drawing{}, ErrDrawingNotFound
	}
	if _, err := ds.db.Exec(`UPDATE drawings SET topics=? WHERE id=?`, string(raw), id); err != nil {
		return Drawing{}, fmt.Errorf("set topics: %w", err)
	}
	d.Topics = clean
	return *d, nil
}

// Delete removes one drawing, its scene, thumbnail and backups — one
// transaction where the file store had four separate removes.
// RenameGroup relabels every drawing whose tab is the old project name — the
// design library's half of a project rename. Returns how many changed.
func (ds *DrawingStore) RenameGroup(old, name string) int {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	if _, err := ds.db.Exec(`UPDATE drawings SET group_name=? WHERE group_name=?`, name, old); err != nil {
		log.Printf("drawings: rename group: %v", err)
		return 0
	}
	n := 0
	for _, d := range ds.drawings {
		if d.Group == old {
			d.Group = name
			n++
		}
	}
	return n
}

func (ds *DrawingStore) Delete(id string) error {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	for i, d := range ds.drawings {
		if d.ID == id {
			tx, err := ds.db.Begin()
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM drawings WHERE id=?`, id); err != nil {
				tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`DELETE FROM drawing_backups WHERE drawing_id=?`, id); err != nil {
				tx.Rollback()
				return err
			}
			if err := tx.Commit(); err != nil {
				return err
			}
			ds.drawings = append(ds.drawings[:i], ds.drawings[i+1:]...)
			return nil
		}
	}
	return ErrDrawingNotFound
}
