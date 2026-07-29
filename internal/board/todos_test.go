package board

import (
	"testing"
)

// A failed row write must surface as an error and leave the serving copy
// untouched — a success whose state exists only in memory would evaporate on
// restart (the DB-first rule the other stores follow).
func TestTodoWriteFailureLeavesServingCopyUntouched(t *testing.T) {
	db, err := OpenDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := NewTodoStore(db)
	card, err := ts.Create("card", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db.Close() // every write from here on fails

	title := "renamed"
	if _, err := ts.Update(card.ID, TodoPatch{Title: &title}); err == nil {
		t.Fatal("update over a dead db must error, not report success")
	}
	if got := ts.List()[0].Title; got != "card" {
		t.Fatalf("failed update must not mutate the serving copy, got %q", got)
	}
	if _, err := ts.Create("second", "", "", ""); err == nil {
		t.Fatal("create over a dead db must error, not report success")
	}
	if n := len(ts.List()); n != 1 {
		t.Fatalf("failed create must not append to the serving copy, got %d cards", n)
	}
	if _, err := ts.SetSnapshot(card.ID, &TodoSnapshot{Tokens: 1, Sessions: 1}); err == nil {
		t.Fatal("snapshot over a dead db must error, not report success")
	}
	if ts.List()[0].Snapshot != nil {
		t.Fatal("failed snapshot write must not stick in the serving copy")
	}
	if err := ts.Delete(card.ID); err == nil {
		t.Fatal("delete over a dead db must error, not report success")
	}
	if n := len(ts.List()); n != 1 {
		t.Fatalf("failed delete must not drop the card from the serving copy, got %d", n)
	}
}

func TestTodoStoreCRUDAndPersistence(t *testing.T) {
	db := newTestDataDB(t)
	ts := NewTodoStore(db)

	a, err := ts.Create("first", "a note", "repo-a", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := ts.Create("second", "", "", "backlog")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Status != "backlog" || b.Order <= a.Order {
		t.Fatalf("new todos should stack at the bottom of backlog: %+v %+v", a, b)
	}
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("want card numbers 1 and 2, got %d and %d", a.Seq, b.Seq)
	}

	labels := []string{" api ", "", "ui", "api"}
	relabeled, err := ts.Update(a.ID, TodoPatch{Labels: &labels})
	if err != nil {
		t.Fatalf("update labels: %v", err)
	}
	if len(relabeled.Labels) != 2 || relabeled.Labels[0] != "api" || relabeled.Labels[1] != "ui" {
		t.Fatalf("labels should be trimmed + deduped, got %v", relabeled.Labels)
	}

	status, order := "doing", 1.0
	moved, err := ts.Update(b.ID, TodoPatch{Status: &status, Order: &order})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if moved.Status != "doing" || moved.Order != 1.0 {
		t.Fatalf("patch not applied: %+v", moved)
	}

	links := []string{" session-123 ", "session-456", "session-123", "  "}
	linked, err := ts.Update(b.ID, TodoPatch{LinkedSessionIDs: &links})
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	if len(linked.LinkedSessionIDs) != 2 || linked.LinkedSessionIDs[0] != "session-123" ||
		linked.LinkedSessionIDs[1] != "session-456" {
		t.Fatalf("session links should be trimmed, deduped and ordered, got %#v", linked.LinkedSessionIDs)
	}
	unlink := []string{}
	if unlinked, _ := ts.Update(b.ID, TodoPatch{LinkedSessionIDs: &unlink}); len(unlinked.LinkedSessionIDs) != 0 {
		t.Fatalf("empty patch should unlink all, got %#v", unlinked.LinkedSessionIDs)
	}

	frozen, err := ts.SetSnapshot(b.ID, &TodoSnapshot{Tokens: 42, CostUSD: 1.5, Agents: 3})
	if err != nil || frozen.Snapshot == nil || frozen.Snapshot.Tokens != 42 {
		t.Fatalf("snapshot not stored: %+v (%v)", frozen.Snapshot, err)
	}
	// Snapshots survive a reload and clear with nil.
	if reloaded := NewTodoStore(db).List(); reloaded[1].Snapshot == nil {
		t.Fatal("snapshot should persist through the db")
	}
	if thawed, _ := ts.SetSnapshot(b.ID, nil); thawed.Snapshot != nil {
		t.Fatal("nil should clear the snapshot")
	}
	if _, err := ts.SetSnapshot("nope", nil); err != ErrTodoNotFound {
		t.Fatalf("want ErrTodoNotFound, got %v", err)
	}

	// Reload: a fresh store over the same db is a restart, column-ordered.
	ts2 := NewTodoStore(db)
	list := ts2.List()
	if len(list) != 2 {
		t.Fatalf("want 2 todos after reload, got %d", len(list))
	}
	if list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("want backlog before doing, got %s then %s", list[0].Status, list[1].Status)
	}

	if err := ts2.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := len(ts2.List()); got != 1 {
		t.Fatalf("want 1 todo after delete, got %d", got)
	}
	if got := len(NewTodoStore(db).List()); got != 1 {
		t.Fatalf("delete should persist, reload got %d", got)
	}
}

func TestTodoStoreValidation(t *testing.T) {
	ts := NewTodoStore(newTestDataDB(t))

	if _, err := ts.Create("   ", "", "", ""); err == nil {
		t.Fatal("blank title should be rejected")
	}
	if _, err := ts.Create("ok", "", "", "shipped"); err == nil {
		t.Fatal("unknown status should be rejected on create")
	}

	todo, err := ts.Create("ok", "", "", "doing")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if todo.Status != "doing" {
		t.Fatalf("create should honor the given column, got %s", todo.Status)
	}
	bad := "shipped"
	if _, err := ts.Update(todo.ID, TodoPatch{Status: &bad}); err == nil {
		t.Fatal("unknown status should be rejected")
	}
	empty := " "
	if _, err := ts.Update(todo.ID, TodoPatch{Title: &empty}); err == nil {
		t.Fatal("blank title patch should be rejected")
	}

	if _, err := ts.Update("nope", TodoPatch{}); err != ErrTodoNotFound {
		t.Fatalf("want ErrTodoNotFound, got %v", err)
	}
	if err := ts.Delete("nope"); err != ErrTodoNotFound {
		t.Fatalf("want ErrTodoNotFound, got %v", err)
	}
}

func TestTodoStoreLinkedDrawings(t *testing.T) {
	db := newTestDataDB(t)
	ts := NewTodoStore(db)

	a, err := ts.Create("with wireframes", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := ts.Create("other card", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Linking trims + dedupes, keeping order.
	ids := []string{" d1 ", "d2", "", "d1"}
	linked, err := ts.Update(a.ID, TodoPatch{LinkedDrawingIDs: &ids})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(linked.LinkedDrawingIDs) != 2 || linked.LinkedDrawingIDs[0] != "d1" || linked.LinkedDrawingIDs[1] != "d2" {
		t.Fatalf("want [d1 d2], got %v", linked.LinkedDrawingIDs)
	}
	if _, err := ts.Update(b.ID, TodoPatch{LinkedDrawingIDs: &[]string{"d2"}}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Deleting a drawing unlinks it from every card (and persists).
	if !ts.UnlinkDrawing("d2") {
		t.Fatal("UnlinkDrawing(d2) should report a change")
	}
	if ts.UnlinkDrawing("d2") {
		t.Fatal("second UnlinkDrawing(d2) should be a no-op")
	}
	ts2 := NewTodoStore(db)
	for _, todo := range ts2.List() {
		for _, id := range todo.LinkedDrawingIDs {
			if id == "d2" {
				t.Fatalf("d2 should be unlinked everywhere, still on %q", todo.Title)
			}
		}
		if todo.ID == a.ID && len(todo.LinkedDrawingIDs) != 1 {
			t.Fatalf("card a should keep d1, got %v", todo.LinkedDrawingIDs)
		}
	}

	// An empty list unlinks everything.
	cleared, err := ts2.Update(a.ID, TodoPatch{LinkedDrawingIDs: &[]string{}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(cleared.LinkedDrawingIDs) != 0 {
		t.Fatalf("want no linked drawings, got %v", cleared.LinkedDrawingIDs)
	}
}

func TestTodoStoreLinkedDocs(t *testing.T) {
	db := newTestDataDB(t)
	ts := NewTodoStore(db)

	a, err := ts.Create("with docs", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	b, err := ts.Create("other card", "", "", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Linking trims + dedupes, keeping order — and it is independent of the
	// wireframe link list on the same card.
	if _, err := ts.Update(a.ID, TodoPatch{LinkedDrawingIDs: &[]string{"draw1"}}); err != nil {
		t.Fatalf("update drawings: %v", err)
	}
	ids := []string{" doc1 ", "doc2", "", "doc1"}
	linked, err := ts.Update(a.ID, TodoPatch{LinkedDocIDs: &ids})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(linked.LinkedDocIDs) != 2 || linked.LinkedDocIDs[0] != "doc1" || linked.LinkedDocIDs[1] != "doc2" {
		t.Fatalf("want [doc1 doc2], got %v", linked.LinkedDocIDs)
	}
	if len(linked.LinkedDrawingIDs) != 1 || linked.LinkedDrawingIDs[0] != "draw1" {
		t.Fatalf("linking docs must not disturb the wireframe list, got %v", linked.LinkedDrawingIDs)
	}
	if _, err := ts.Update(b.ID, TodoPatch{LinkedDocIDs: &[]string{"doc2"}}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Deleting a doc unlinks it from every card (and persists across a reload).
	if !ts.UnlinkDoc("doc2") {
		t.Fatal("UnlinkDoc(doc2) should report a change")
	}
	if ts.UnlinkDoc("doc2") {
		t.Fatal("second UnlinkDoc(doc2) should be a no-op")
	}
	ts2 := NewTodoStore(db)
	for _, todo := range ts2.List() {
		for _, id := range todo.LinkedDocIDs {
			if id == "doc2" {
				t.Fatalf("doc2 should be unlinked everywhere, still on %q", todo.Title)
			}
		}
		if todo.ID == a.ID {
			if len(todo.LinkedDocIDs) != 1 || todo.LinkedDocIDs[0] != "doc1" {
				t.Fatalf("card a should keep doc1, got %v", todo.LinkedDocIDs)
			}
			// Unlinking a doc must leave the wireframe link alone.
			if len(todo.LinkedDrawingIDs) != 1 || todo.LinkedDrawingIDs[0] != "draw1" {
				t.Fatalf("card a should keep draw1, got %v", todo.LinkedDrawingIDs)
			}
		}
	}

	// An empty list unlinks everything.
	cleared, err := ts2.Update(a.ID, TodoPatch{LinkedDocIDs: &[]string{}})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(cleared.LinkedDocIDs) != 0 {
		t.Fatalf("want no linked docs, got %v", cleared.LinkedDocIDs)
	}
}
