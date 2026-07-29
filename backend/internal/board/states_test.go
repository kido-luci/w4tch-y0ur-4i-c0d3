package board

import "testing"

// The migration seeds the three ids the pre-v12 enum used, so an existing
// board keeps working without a single card being rewritten.
func TestStatesSeedTheBuiltinColumns(t *testing.T) {
	ss := NewStateStore(newTestDataDB(t))
	got := ss.List()
	if len(got) != 3 {
		t.Fatalf("want the 3 seeded columns, got %d: %#v", len(got), got)
	}
	for i, want := range []struct{ id, category string }{
		{"backlog", "todo"}, {"doing", "started"}, {"done", "done"},
	} {
		if got[i].ID != want.id || got[i].Category != want.category {
			t.Errorf("column %d = %s/%s, want %s/%s", i, got[i].ID, got[i].Category, want.id, want.category)
		}
		if !got[i].Builtin {
			t.Errorf("column %q should report as builtin", got[i].ID)
		}
	}
}

func TestStatesCreateSlugsAndDeduplicates(t *testing.T) {
	ss := NewStateStore(newTestDataDB(t))
	first, err := ss.Create("In review", "started", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "in-review" {
		t.Fatalf("want a readable slug id, got %q", first.ID)
	}
	if first.WIPLimit != 3 {
		t.Fatalf("want the wip limit kept, got %d", first.WIPLimit)
	}
	second, err := ss.Create("in review", "started", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != "in-review-2" {
		t.Fatalf("a colliding name must get its own id, got %q", second.ID)
	}
	// A name with nothing slug-able still produces a usable id.
	cjk, err := ss.Create("待办", "todo", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if cjk.ID == "" || cjk.ID == "-" {
		t.Fatalf("unusable id for a non-latin name: %q", cjk.ID)
	}
}

func TestStatesRejectBadCategoryAndBuiltinDelete(t *testing.T) {
	ss := NewStateStore(newTestDataDB(t))
	if _, err := ss.Create("Nope", "shipped", "", 0); err == nil {
		t.Fatal("an unknown category must be refused")
	}
	for _, id := range []string{"backlog", "doing", "done"} {
		if err := ss.Delete(id); err == nil {
			t.Errorf("builtin column %q must not be deletable", id)
		}
	}
}

// Renaming and reordering a builtin is allowed — only its id is fixed — and
// the change survives a reload.
func TestStatesUpdatePersists(t *testing.T) {
	db := newTestDataDB(t)
	ss := NewStateStore(db)
	name, order := "Shipped", 9.0
	if _, err := ss.Update("done", StatePatch{Name: &name, Order: &order}); err != nil {
		t.Fatal(err)
	}
	got, ok := NewStateStore(db).Get("done")
	if !ok || got.Name != "Shipped" || got.Order != 9 {
		t.Fatalf("rename did not survive a reload: %#v (%v)", got, ok)
	}
	if got.Category != "done" {
		t.Fatalf("a rename must not touch the category, got %q", got.Category)
	}
}

// The board's order follows the columns, not the old hardcoded trio: a column
// inserted between doing and done sorts between them.
func TestBoardSortsByCustomColumnOrder(t *testing.T) {
	db := newTestDataDB(t)
	ss := NewStateStore(db)
	ts := NewTodoStore(db)
	ts.UseStates(ss)

	review, err := ss.Create("In review", "started", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	order := 1.5 // between doing (1) and done (2)
	if _, err := ss.Update(review.ID, StatePatch{Order: &order}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"done", review.ID, "backlog"} {
		if _, err := ts.Create("card "+status, "", "", status); err != nil {
			t.Fatalf("create in %q: %v", status, err)
		}
	}
	got := ts.List()
	want := []string{"backlog", "doing", review.ID, "done"}
	var seen []string
	for _, c := range got {
		seen = append(seen, c.Status)
	}
	// Only three cards exist; check they came back in column order.
	j := 0
	for _, s := range seen {
		for j < len(want) && want[j] != s {
			j++
		}
		if j == len(want) {
			t.Fatalf("cards are out of column order: %v", seen)
		}
	}
}

// A card cannot be parked in a column that does not exist.
func TestBoardRejectsUnknownStatus(t *testing.T) {
	db := newTestDataDB(t)
	ts := NewTodoStore(db)
	ts.UseStates(NewStateStore(db))
	if _, err := ts.Create("nope", "", "", "in-review"); err == nil {
		t.Fatal("an unknown column must be refused")
	}
}

// The snapshot freeze keys off the column's CATEGORY, so a renamed or added
// done-column freezes exactly like the builtin one.
func TestIsDoneStatusFollowsCategory(t *testing.T) {
	db := newTestDataDB(t)
	ss := NewStateStore(db)
	ts := NewTodoStore(db)
	ts.UseStates(ss)

	shipped, err := ss.Create("Shipped", "done", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !ts.IsDoneStatus(shipped.ID) {
		t.Fatal("a done-category column must count as done")
	}
	if !ts.IsDoneStatus("done") {
		t.Fatal("the builtin done column must still count as done")
	}
	if ts.IsDoneStatus("doing") {
		t.Fatal("a started-category column must not count as done")
	}
	// Recategorising the builtin flips it — the string "done" is not magic.
	cat := "started"
	if _, err := ss.Update("done", StatePatch{Category: &cat}); err != nil {
		t.Fatal(err)
	}
	if ts.IsDoneStatus("done") {
		t.Fatal("category, not name, must decide what counts as done")
	}
}

// A store with no columns attached still answers with the pre-v12 trio, which
// is what the todos.json import path relies on.
func TestBareStoreFallsBackToBuiltinTrio(t *testing.T) {
	ts := NewTodoStore(newTestDataDB(t))
	if !ts.validStatus("done") || ts.validStatus("in-review") {
		t.Fatal("a bare store should accept exactly backlog/doing/done")
	}
	if !ts.IsDoneStatus("done") {
		t.Fatal("a bare store should still treat done as done")
	}
}
