package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestDrawingStoreCRUDAndPersistence(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)

	a, err := ds.Create("login screen", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := ds.Create("settings page", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A new drawing starts as a valid empty .excalidraw scene.
	content, err := ds.Content(a.ID)
	if err != nil {
		t.Fatalf("content: %v", err)
	}
	if !strings.Contains(string(content), `"type": "excalidraw"`) {
		t.Fatalf("new scene should be a standard excalidraw doc, got %s", content)
	}

	scene := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"r1"}],"appState":{},"files":{}}`)
	saved, err := ds.SetContent(a.ID, scene, time.Time{})
	if err != nil {
		t.Fatalf("set content: %v", err)
	}
	if !saved.UpdatedAt.After(saved.CreatedAt) {
		t.Fatalf("save should bump UpdatedAt: %+v", saved)
	}

	// List is most-recently-updated first: a was just saved, so it leads.
	list := ds.List()
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("want [a b] by recency, got %+v", list)
	}

	renamed, err := ds.Rename(b.ID, "  settings v2  ")
	if err != nil || renamed.Name != "settings v2" {
		t.Fatalf("rename should trim + apply, got %q (%v)", renamed.Name, err)
	}

	// Reload: a fresh store over the same db is a restart.
	ds2 := NewDrawingStore(db)
	if got := len(ds2.List()); got != 2 {
		t.Fatalf("want 2 drawings after reload, got %d", got)
	}
	content, err = ds2.Content(a.ID)
	if err != nil || string(content) != string(scene) {
		t.Fatalf("scene should persist, got %s (%v)", content, err)
	}

	// An emptied-out scene degrades to the empty document, not an error (the
	// file store had the same degradation for a hand-deleted scene file).
	if _, err := db.Exec(`UPDATE drawings SET scene='' WHERE id=?`, b.ID); err != nil {
		t.Fatalf("empty scene: %v", err)
	}
	content, err = ds2.Content(b.ID)
	if err != nil || !strings.Contains(string(content), `"elements": []`) {
		t.Fatalf("missing scene should read as empty, got %s (%v)", content, err)
	}

	if err := ds2.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drawings WHERE id=?`, a.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delete should remove the row, n=%d err=%v", n, err)
	}
	if got := len(ds2.List()); got != 1 {
		t.Fatalf("want 1 drawing after delete, got %d", got)
	}
}

func TestDrawingStoreValidation(t *testing.T) {
	ds := NewDrawingStore(newTestDataDB(t))

	if _, err := ds.Create("   ", ""); err == nil {
		t.Fatal("blank name should be rejected")
	}

	d, err := ds.Create("ok", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ds.SetContent(d.ID, []byte("{not json"), time.Time{}); err == nil {
		t.Fatal("invalid JSON content should be rejected")
	}
	if _, err := ds.Rename(d.ID, " "); err == nil {
		t.Fatal("blank rename should be rejected")
	}

	if _, err := ds.Content("nope"); err != errDrawingNotFound {
		t.Fatalf("want errDrawingNotFound, got %v", err)
	}
	if _, err := ds.SetContent("nope", []byte("{}"), time.Time{}); err != errDrawingNotFound {
		t.Fatalf("want errDrawingNotFound, got %v", err)
	}
	if _, err := ds.Rename("nope", "x"); err != errDrawingNotFound {
		t.Fatalf("want errDrawingNotFound, got %v", err)
	}
	if err := ds.Delete("nope"); err != errDrawingNotFound {
		t.Fatalf("want errDrawingNotFound, got %v", err)
	}
}

func TestDrawingStoreDuplicate(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)

	src, err := ds.Create("login screen", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	scene := []byte(`{"type":"excalidraw","version":2,"elements":[{"id":"r1"}],"appState":{},"files":{}}`)
	if _, err := ds.SetContent(src.ID, scene, time.Time{}); err != nil {
		t.Fatalf("set content: %v", err)
	}

	dup, err := ds.Duplicate(src.ID)
	if err != nil {
		t.Fatalf("duplicate: %v", err)
	}
	if dup.ID == src.ID || dup.Name != "login screen (copy)" {
		t.Fatalf("unexpected duplicate metadata: %+v", dup)
	}
	content, err := ds.Content(dup.ID)
	if err != nil || string(content) != string(scene) {
		t.Fatalf("duplicate should copy the scene, got %s (%v)", content, err)
	}

	// The copy is independent: editing it leaves the original alone, and it
	// starts without a fresh thumbnail. Both survive a reload.
	if _, err := ds.SetContent(dup.ID, []byte(`{"v":"fork"}`), time.Time{}); err != nil {
		t.Fatalf("edit duplicate: %v", err)
	}
	orig, _ := ds.Content(src.ID)
	if string(orig) != string(scene) {
		t.Fatalf("editing the copy must not touch the original, got %s", orig)
	}
	if _, err := ds.Thumbnail(dup.ID); err != errThumbnailStale {
		t.Fatalf("duplicate should start without a thumbnail, got %v", err)
	}
	if got := len(NewDrawingStore(db).List()); got != 2 {
		t.Fatalf("want 2 drawings after reload, got %d", got)
	}

	if _, err := ds.Duplicate("nope"); err != errDrawingNotFound {
		t.Fatalf("want errDrawingNotFound, got %v", err)
	}
}

func TestDrawingStoreConditionalWrites(t *testing.T) {
	ds := NewDrawingStore(newTestDataDB(t))
	d, err := ds.Create("wf", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A matching base succeeds and bumps UpdatedAt.
	if _, err := ds.SetContent(d.ID, []byte(`{"v":1}`), d.UpdatedAt); err != nil {
		t.Fatalf("conditional write with fresh base: %v", err)
	}

	// The stale base (pre-bump) now conflicts and leaves the scene untouched.
	if _, err := ds.SetContent(d.ID, []byte(`{"v":2}`), d.UpdatedAt); err != errDrawingConflict {
		t.Fatalf("want errDrawingConflict, got %v", err)
	}
	content, _ := ds.Content(d.ID)
	if string(content) != `{"v":1}` {
		t.Fatalf("conflicted write must not change the scene, got %s", content)
	}

	// A zero base stays unconditional (legacy writers).
	if _, err := ds.SetContent(d.ID, []byte(`{"v":3}`), time.Time{}); err != nil {
		t.Fatalf("unconditional write: %v", err)
	}
}

func TestDrawingStoreSceneBackups(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)
	d, err := ds.Create("wf", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bak := func(n int) string {
		var b []byte
		if err := db.QueryRow(`SELECT content FROM drawing_backups WHERE drawing_id=? AND slot=?`, d.ID, n).Scan(&b); err != nil {
			return ""
		}
		return string(b)
	}

	// Overwrites rotate: slot 1 always holds the immediately-previous scene.
	for i := 1; i <= maxSceneBackups+2; i++ {
		if _, err := ds.SetContent(d.ID, []byte(fmt.Sprintf(`{"v":%d}`, i)), time.Time{}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := bak(1); got != fmt.Sprintf(`{"v":%d}`, maxSceneBackups+1) {
		t.Fatalf("slot 1 should be the previous scene, got %s", got)
	}
	if bak(maxSceneBackups) == "" {
		t.Fatalf("slot %d should exist", maxSceneBackups)
	}
	var over int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drawing_backups WHERE drawing_id=? AND slot>?`, d.ID, maxSceneBackups).Scan(&over); err != nil || over != 0 {
		t.Fatalf("backups must cap at %d, found %d beyond (err=%v)", maxSceneBackups, over, err)
	}

	// Saving identical content must not burn a backup slot.
	before := bak(1)
	last := fmt.Sprintf(`{"v":%d}`, maxSceneBackups+2)
	if _, err := ds.SetContent(d.ID, []byte(last), time.Time{}); err != nil {
		t.Fatalf("identical write: %v", err)
	}
	if bak(1) != before {
		t.Fatal("identical content should not rotate backups")
	}

	// Delete cleans the backups up with the scene.
	if err := ds.Delete(d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM drawing_backups WHERE drawing_id=?`, d.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delete should remove backups, n=%d err=%v", n, err)
	}
}

func TestDrawingStoreThumbnails(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)
	d, err := ds.Create("wf", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No thumbnail rendered yet → stale.
	if _, err := ds.Thumbnail(d.ID); err != errThumbnailStale {
		t.Fatalf("want errThumbnailStale before upload, got %v", err)
	}

	// Uploading one rendered from the current version makes it fresh.
	if _, err := ds.SetThumbnail(d.ID, []byte("png-bytes"), d.UpdatedAt); err != nil {
		t.Fatalf("set thumbnail: %v", err)
	}
	b, err := ds.Thumbnail(d.ID)
	if err != nil || string(b) != "png-bytes" {
		t.Fatalf("fresh thumbnail should serve, got %q (%v)", b, err)
	}

	// A content write bumps UpdatedAt → the cached thumbnail goes stale.
	d2, err := ds.SetContent(d.ID, []byte(`{"v":1}`), time.Time{})
	if err != nil {
		t.Fatalf("set content: %v", err)
	}
	if _, err := ds.Thumbnail(d.ID); err != errThumbnailStale {
		t.Fatalf("want errThumbnailStale after content write, got %v", err)
	}

	// Freshness survives a restart (ThumbUpdatedAt persists in the row).
	if _, err := ds.SetThumbnail(d.ID, []byte("png-2"), d2.UpdatedAt); err != nil {
		t.Fatalf("set thumbnail: %v", err)
	}
	ds2 := NewDrawingStore(db)
	if b, err := ds2.Thumbnail(d.ID); err != nil || string(b) != "png-2" {
		t.Fatalf("thumbnail freshness should persist, got %q (%v)", b, err)
	}

	// A zero base is rejected; delete removes the thumbnail with the row.
	if _, err := ds2.SetThumbnail(d.ID, []byte("x"), time.Time{}); err == nil {
		t.Fatal("zero base should be rejected")
	}
	if err := ds2.Delete(d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := ds2.Thumbnail(d.ID); err != errDrawingNotFound {
		t.Fatalf("deleted drawing's thumbnail should be gone, got %v", err)
	}
}

func TestDrawingStoreGroups(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)

	// Create carries (and trims) the group; the tab is a project or custom label.
	a, err := ds.Create("cart", "  shop  ")
	if err != nil || a.Group != "shop" {
		t.Fatalf("create should trim+store group, got %q (%v)", a.Group, err)
	}
	b, err := ds.Create("loose", "")
	if err != nil || b.Group != "" {
		t.Fatalf("empty group is Ungrouped, got %q (%v)", b.Group, err)
	}

	// SetGroup moves between tabs and back to Ungrouped, without bumping
	// UpdatedAt (metadata-only, like Rename).
	moved, err := ds.SetGroup(b.ID, "misc")
	if err != nil || moved.Group != "misc" {
		t.Fatalf("set group, got %q (%v)", moved.Group, err)
	}
	if !moved.UpdatedAt.Equal(b.UpdatedAt) {
		t.Fatalf("SetGroup must not bump UpdatedAt: %v -> %v", b.UpdatedAt, moved.UpdatedAt)
	}
	if cleared, err := ds.SetGroup(a.ID, "   "); err != nil || cleared.Group != "" {
		t.Fatalf("blank group clears to Ungrouped, got %q (%v)", cleared.Group, err)
	}
	if _, err := ds.SetGroup("nope", "x"); err != errDrawingNotFound {
		t.Fatalf("set group on missing id, got %v", err)
	}

	// Duplicate inherits the source's group.
	dup, err := ds.Duplicate(moved.ID)
	if err != nil || dup.Group != "misc" {
		t.Fatalf("duplicate should carry group, got %q (%v)", dup.Group, err)
	}

	// Groups survive a restart (the group_name column persists).
	ds2 := NewDrawingStore(db)
	byID := map[string]string{}
	for _, d := range ds2.List() {
		byID[d.ID] = d.Group
	}
	if byID[a.ID] != "" || byID[moved.ID] != "misc" || byID[dup.ID] != "misc" {
		t.Fatalf("groups should persist across reload, got %+v", byID)
	}
}

func TestDrawingStoreTopics(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)

	a, err := ds.Create("login", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A new drawing is untagged as JSON [] — never null.
	if a.Topics == nil || len(a.Topics) != 0 {
		t.Fatalf("new drawing should carry empty (non-nil) topics, got %#v", a.Topics)
	}

	// SetTopics trims, drops blanks, collapses duplicates, keeps order — and
	// is metadata-only (no UpdatedAt bump), like SetGroup.
	tagged, err := ds.SetTopics(a.ID, []string{" screens ", "", "auth", "screens", "   "})
	if err != nil {
		t.Fatalf("set topics: %v", err)
	}
	if fmt.Sprintf("%v", tagged.Topics) != "[screens auth]" {
		t.Fatalf("want [screens auth], got %v", tagged.Topics)
	}
	if !tagged.UpdatedAt.Equal(a.UpdatedAt) {
		t.Fatalf("SetTopics must not bump UpdatedAt: %v -> %v", a.UpdatedAt, tagged.UpdatedAt)
	}
	if _, err := ds.SetTopics("nope", []string{"x"}); err != errDrawingNotFound {
		t.Fatalf("set topics on missing id, got %v", err)
	}

	// Duplicate inherits the source's tags; an empty set untags.
	dup, err := ds.Duplicate(a.ID)
	if err != nil || fmt.Sprintf("%v", dup.Topics) != "[screens auth]" {
		t.Fatalf("duplicate should carry topics, got %v (%v)", dup.Topics, err)
	}
	if cleared, err := ds.SetTopics(dup.ID, nil); err != nil || len(cleared.Topics) != 0 {
		t.Fatalf("empty set should untag, got %v (%v)", cleared.Topics, err)
	}

	// Topics survive a restart (the topics column persists).
	ds2 := NewDrawingStore(db)
	byID := map[string]string{}
	for _, d := range ds2.List() {
		byID[d.ID] = fmt.Sprintf("%v", d.Topics)
	}
	if byID[a.ID] != "[screens auth]" || byID[dup.ID] != "[]" {
		t.Fatalf("topics should persist across reload, got %+v", byID)
	}
}

func TestMarkPublished(t *testing.T) {
	ds := NewDrawingStore(newTestDataDB(t))
	d, err := ds.Create("login screen", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := ds.MarkPublished(d.ID, time.Time{}); err == nil {
		t.Error("zero base must be rejected")
	}
	if _, err := ds.MarkPublished("nope", d.UpdatedAt); err == nil {
		t.Error("unknown id must be rejected")
	}

	out, err := ds.MarkPublished(d.ID, d.UpdatedAt)
	if err != nil {
		t.Fatalf("mark published: %v", err)
	}
	if !out.PublishedAt.Equal(d.UpdatedAt) {
		t.Errorf("PublishedAt = %v, want %v", out.PublishedAt, d.UpdatedAt)
	}

	// A later content write bumps UpdatedAt — the published copy goes stale
	// without MarkPublished being touched (the ThumbUpdatedAt idiom).
	time.Sleep(2 * time.Millisecond)
	upd, err := ds.SetContent(d.ID, []byte(`{"elements":[1]}`), time.Time{})
	if err != nil {
		t.Fatalf("set content: %v", err)
	}
	if upd.PublishedAt.Equal(upd.UpdatedAt) {
		t.Error("publish must go stale after a content write")
	}
}
