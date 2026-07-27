package main

import (
	"fmt"
	"testing"
	"time"
)

func TestDocStoreCRUDAndPersistence(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDocStore(db)

	home, err := ds.Create("Home", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	guide, err := ds.Create("Guide", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A new page starts with an empty body.
	body, err := ds.Content(home.ID)
	if err != nil || body != "" {
		t.Fatalf("new page should have an empty body, got %q (%v)", body, err)
	}

	saved, err := ds.SetContent(home.ID, "# Home\n\nwelcome", time.Time{})
	if err != nil {
		t.Fatalf("set content: %v", err)
	}
	if !saved.UpdatedAt.After(saved.CreatedAt) {
		t.Fatalf("save should bump UpdatedAt: %+v", saved)
	}

	// List is Order-sorted (Home=1, Guide=2); a body write doesn't reorder.
	list := ds.List()
	if len(list) != 2 || list[0].ID != home.ID || list[1].ID != guide.ID {
		t.Fatalf("want [home guide] by order, got %+v", list)
	}

	renamed, err := ds.Update(guide.ID, docPatch{Title: strptr("  Guide v2  ")})
	if err != nil || renamed.Title != "Guide v2" {
		t.Fatalf("rename should trim + apply, got %q (%v)", renamed.Title, err)
	}

	// Reload: a fresh store over the same db is a restart.
	ds2 := NewDocStore(db)
	if got := len(ds2.List()); got != 2 {
		t.Fatalf("want 2 docs after reload, got %d", got)
	}
	body, err = ds2.Content(home.ID)
	if err != nil || body != "# Home\n\nwelcome" {
		t.Fatalf("body should persist, got %q (%v)", body, err)
	}

	if err := ds2.Delete(home.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM docs WHERE id=?`, home.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delete should remove the row, n=%d err=%v", n, err)
	}
	if got := len(ds2.List()); got != 1 {
		t.Fatalf("want 1 doc after delete, got %d", got)
	}
}

func TestDocStoreValidation(t *testing.T) {
	ds := NewDocStore(newTestDataDB(t))

	if _, err := ds.Create("   ", "", ""); err == nil {
		t.Fatal("blank title should be rejected")
	}
	if _, err := ds.Create("orphan", "nope", ""); err == nil {
		t.Fatal("creating under a missing parent should be rejected")
	}

	d, err := ds.Create("ok", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := ds.Update(d.ID, docPatch{Title: strptr(" ")}); err == nil {
		t.Fatal("blank rename should be rejected")
	}

	if _, err := ds.Content("nope"); err != errDocNotFound {
		t.Fatalf("want errDocNotFound, got %v", err)
	}
	if _, err := ds.SetContent("nope", "x", time.Time{}); err != errDocNotFound {
		t.Fatalf("want errDocNotFound, got %v", err)
	}
	if _, err := ds.Update("nope", docPatch{Title: strptr("x")}); err != errDocNotFound {
		t.Fatalf("want errDocNotFound, got %v", err)
	}
	if err := ds.Delete("nope"); err != errDocNotFound {
		t.Fatalf("want errDocNotFound, got %v", err)
	}
}

// A rename / move must NOT bump UpdatedAt: it is the body's conflict base, and
// a metadata edit racing an in-flight save mustn't invalidate it.
func TestDocStoreMetadataKeepsBodyVersion(t *testing.T) {
	ds := NewDocStore(newTestDataDB(t))
	d, err := ds.Create("page", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	base := d.UpdatedAt
	renamed, err := ds.Update(d.ID, docPatch{Title: strptr("renamed")})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if !renamed.UpdatedAt.Equal(base) {
		t.Fatalf("rename must not move UpdatedAt: was %v, now %v", base, renamed.UpdatedAt)
	}
	// And the original base still writes cleanly afterwards.
	if _, err := ds.SetContent(d.ID, "body", base); err != nil {
		t.Fatalf("body write against the pre-rename base should still work: %v", err)
	}
}

// Group is a metadata-only patch field: trimmed, persisted, clearable via "",
// and — like rename — never moves UpdatedAt (the body's conflict base).
func TestDocStoreGroup(t *testing.T) {
	ds := NewDocStore(newTestDataDB(t))
	d, err := ds.Create("page", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if d.Group != "" {
		t.Fatalf("new page should start unscoped, got %q", d.Group)
	}
	base := d.UpdatedAt

	moved, err := ds.Update(d.ID, docPatch{Group: strptr("  shop  ")})
	if err != nil || moved.Group != "shop" {
		t.Fatalf("group should trim + apply, got %q (%v)", moved.Group, err)
	}
	if !moved.UpdatedAt.Equal(base) {
		t.Fatalf("group move must not move UpdatedAt: was %v, now %v", base, moved.UpdatedAt)
	}
	if got := NewDocStore(ds.db).List()[0].Group; got != "shop" {
		t.Fatalf("group should persist across reload, got %q", got)
	}

	// A patch without group leaves it alone; "" is a real clear.
	renamed, err := ds.Update(d.ID, docPatch{Title: strptr("renamed")})
	if err != nil || renamed.Group != "shop" {
		t.Fatalf("groupless patch should keep the group, got %q (%v)", renamed.Group, err)
	}
	cleared, err := ds.Update(d.ID, docPatch{Group: strptr("")})
	if err != nil || cleared.Group != "" {
		t.Fatalf("empty group should clear, got %q (%v)", cleared.Group, err)
	}

	// Create can scope a page directly (trimmed like the patch path), and it
	// persists across a reload.
	scoped, err := ds.Create("scoped", "", "  blog  ")
	if err != nil || scoped.Group != "blog" {
		t.Fatalf("create with group should trim + apply, got %q (%v)", scoped.Group, err)
	}
	if got, err := NewDocStore(ds.db).Get(scoped.ID); err != nil || got.Group != "blog" {
		t.Fatalf("created group should persist across reload, got %q (%v)", got.Group, err)
	}
}

func TestDocStoreConditionalWrites(t *testing.T) {
	ds := NewDocStore(newTestDataDB(t))
	d, err := ds.Create("page", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// A matching base succeeds and bumps UpdatedAt.
	if _, err := ds.SetContent(d.ID, "v1", d.UpdatedAt); err != nil {
		t.Fatalf("conditional write with fresh base: %v", err)
	}

	// The stale base (pre-bump) now conflicts and leaves the body untouched.
	if _, err := ds.SetContent(d.ID, "v2", d.UpdatedAt); err != errDocConflict {
		t.Fatalf("want errDocConflict, got %v", err)
	}
	if body, _ := ds.Content(d.ID); body != "v1" {
		t.Fatalf("conflicted write must not change the body, got %q", body)
	}

	// A zero base stays unconditional.
	if _, err := ds.SetContent(d.ID, "v3", time.Time{}); err != nil {
		t.Fatalf("unconditional write: %v", err)
	}
}

func TestDocStoreBodyBackups(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDocStore(db)
	d, err := ds.Create("page", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	bak := func(n int) string {
		var b string
		if err := db.QueryRow(`SELECT content FROM doc_backups WHERE doc_id=? AND slot=?`, d.ID, n).Scan(&b); err != nil {
			return ""
		}
		return b
	}

	// Overwrites rotate: slot 1 always holds the immediately-previous body.
	for i := 1; i <= maxDocBackups+2; i++ {
		if _, err := ds.SetContent(d.ID, fmt.Sprintf("v%d", i), time.Time{}); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	if got := bak(1); got != fmt.Sprintf("v%d", maxDocBackups+1) {
		t.Fatalf("slot 1 should be the previous body, got %q", got)
	}
	if bak(maxDocBackups) == "" {
		t.Fatalf("slot %d should exist", maxDocBackups)
	}
	var over int
	if err := db.QueryRow(`SELECT COUNT(*) FROM doc_backups WHERE doc_id=? AND slot>?`, d.ID, maxDocBackups).Scan(&over); err != nil || over != 0 {
		t.Fatalf("backups must cap at %d, found %d beyond (err=%v)", maxDocBackups, over, err)
	}

	// Saving identical content must not burn a backup slot.
	before := bak(1)
	last := fmt.Sprintf("v%d", maxDocBackups+2)
	if _, err := ds.SetContent(d.ID, last, time.Time{}); err != nil {
		t.Fatalf("identical write: %v", err)
	}
	if bak(1) != before {
		t.Fatal("identical content should not rotate backups")
	}

	// Delete cleans the backups up with the page.
	if err := ds.Delete(d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM doc_backups WHERE doc_id=?`, d.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delete should remove backups, n=%d err=%v", n, err)
	}
}

func TestDocStoreTreeNestingAndCycles(t *testing.T) {
	ds := NewDocStore(newTestDataDB(t))

	parent, err := ds.Create("Parent", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	child, err := ds.Create("Child", parent.ID, "")
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("child should nest under parent, got %q", child.ParentID)
	}

	// Moving a page under itself is a cycle.
	if _, err := ds.Update(parent.ID, docPatch{ParentID: strptr(parent.ID)}); err != errDocCycle {
		t.Fatalf("self-parent should be errDocCycle, got %v", err)
	}
	// Moving a page under its own descendant is a cycle, and changes nothing.
	if _, err := ds.Update(parent.ID, docPatch{ParentID: strptr(child.ID)}); err != errDocCycle {
		t.Fatalf("moving under a descendant should be errDocCycle, got %v", err)
	}
	if got, _ := ds.Get(parent.ID); got.ParentID != "" {
		t.Fatalf("cycle-rejected move must not change the parent, got %q", got.ParentID)
	}

	// A legitimate move to a fresh root parent lands it at the end of the new
	// siblings' order.
	other, err := ds.Create("Other", "", "")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	moved, err := ds.Update(child.ID, docPatch{ParentID: strptr(other.ID)})
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if moved.ParentID != other.ID || moved.Order != 1 {
		t.Fatalf("moved child should reparent and append (order 1), got parent=%q order=%v", moved.ParentID, moved.Order)
	}
}

func TestDocStoreDeletePromotesChildren(t *testing.T) {
	db := newTestDataDB(t)
	ds := NewDocStore(db)

	root, _ := ds.Create("Root", "", "")
	mid, _ := ds.Create("Mid", root.ID, "")
	leaf, _ := ds.Create("Leaf", mid.ID, "")

	// Deleting the middle page promotes its child to the grandparent, not oblivion.
	if err := ds.Delete(mid.ID); err != nil {
		t.Fatalf("delete mid: %v", err)
	}
	got, err := ds.Get(leaf.ID)
	if err != nil {
		t.Fatalf("leaf should survive its parent's deletion: %v", err)
	}
	if got.ParentID != root.ID {
		t.Fatalf("leaf should be promoted to root, got parent %q", got.ParentID)
	}

	// The promotion persists across a reload.
	if got, _ := NewDocStore(db).Get(leaf.ID); got.ParentID != root.ID {
		t.Fatalf("promotion should persist, got parent %q", got.ParentID)
	}

	// Deleting a root page promotes its child to the top level.
	if err := ds.Delete(root.ID); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	if got, _ := ds.Get(leaf.ID); got.ParentID != "" {
		t.Fatalf("leaf should become a root page, got parent %q", got.ParentID)
	}
}

func strptr(s string) *string { return &s }
