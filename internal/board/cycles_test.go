package board

import (
	"database/sql"
	"testing"
	"time"
)

// boardWithDepth builds the three stores the depth features need, wired the
// way main.go wires them.
func boardWithDepth(t *testing.T) (*sql.DB, *TodoStore, *StateStore, *EventStore, *CycleStore) {
	t.Helper()
	db := newTestDataDB(t)
	ss := NewStateStore(db)
	es := NewEventStore(db)
	ts := NewTodoStore(db)
	ts.UseStates(ss)
	ts.UseEvents(es)
	return db, ts, ss, es, NewCycleStore(db)
}

// --- hierarchy --------------------------------------------------------------

func TestHierarchyNestsTwoLevelsDeep(t *testing.T) {
	_, ts, _, _, _ := boardWithDepth(t)
	epic, err := ts.CreateFull(TodoCreate{Title: "checkout rewrite", Kind: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	story, err := ts.CreateFull(TodoCreate{Title: "cart page", ParentID: epic.ID})
	if err != nil {
		t.Fatalf("a child of a top-level card must be allowed: %v", err)
	}
	// Third level is refused.
	if _, err := ts.CreateFull(TodoCreate{Title: "too deep", ParentID: story.ID}); err == nil {
		t.Fatal("nesting under a child must be refused")
	}
	// So is nesting a card that already has children.
	other, err := ts.CreateFull(TodoCreate{Title: "another top level"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Update(epic.ID, TodoPatch{ParentID: &other.ID}); err == nil {
		t.Fatal("a card with children must not become a child itself")
	}
	// And a card cannot parent itself.
	if _, err := ts.Update(story.ID, TodoPatch{ParentID: &story.ID}); err == nil {
		t.Fatal("self-parenting must be refused")
	}
	unknown := "nope"
	if _, err := ts.Update(story.ID, TodoPatch{ParentID: &unknown}); err == nil {
		t.Fatal("an unknown parent id must be refused")
	}
}

func TestRollupCountsChildrenAndPoints(t *testing.T) {
	_, ts, _, _, _ := boardWithDepth(t)
	epic, err := ts.CreateFull(TodoCreate{Title: "epic", Kind: "epic"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		title    string
		status   string
		estimate float64
	}{
		{"a", "done", 3},
		{"b", "doing", 5},
		{"c", "backlog", 2},
	} {
		if _, err := ts.CreateFull(TodoCreate{
			Title: c.title, ParentID: epic.ID, Status: c.status, Estimate: c.estimate,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var got *TodoRollup
	for _, card := range ts.List() {
		if card.ID == epic.ID {
			got = card.Rollup
		} else if card.Rollup != nil {
			t.Errorf("a childless card must carry no rollup: %#v", card.Rollup)
		}
	}
	if got == nil {
		t.Fatal("the epic should carry a rollup")
	}
	if got.Children != 3 || got.Done != 1 || got.Estimate != 10 || got.EstimateDone != 3 {
		t.Fatalf("unexpected rollup: %#v", got)
	}
}

// Deleting a parent promotes its children instead of taking them with it.
func TestDeletePromotesChildren(t *testing.T) {
	_, ts, _, _, _ := boardWithDepth(t)
	epic, _ := ts.CreateFull(TodoCreate{Title: "epic", Kind: "epic"})
	child, _ := ts.CreateFull(TodoCreate{Title: "child", ParentID: epic.ID})
	if err := ts.Delete(epic.ID); err != nil {
		t.Fatal(err)
	}
	list := ts.List()
	if len(list) != 1 {
		t.Fatalf("the child must survive its parent, got %d cards", len(list))
	}
	if list[0].ID != child.ID || list[0].ParentID != "" {
		t.Fatalf("the child should be promoted to top level, got %#v", list[0])
	}
}

func TestCardFieldsSurviveAReload(t *testing.T) {
	db, ts, _, _, _ := boardWithDepth(t)
	made, err := ts.CreateFull(TodoCreate{
		Title: "sized", Kind: "bug", Priority: 3, Estimate: 5, CycleID: "cy1",
	})
	if err != nil {
		t.Fatal(err)
	}
	reloaded := NewTodoStore(db).List()
	if len(reloaded) != 1 {
		t.Fatalf("want 1 card, got %d", len(reloaded))
	}
	got := reloaded[0]
	if got.ID != made.ID || got.Kind != "bug" || got.Priority != 3 || got.Estimate != 5 || got.CycleID != "cy1" {
		t.Fatalf("depth fields did not survive the reload: %#v", got)
	}
}

func TestCardFieldValidation(t *testing.T) {
	_, ts, _, _, _ := boardWithDepth(t)
	if _, err := ts.CreateFull(TodoCreate{Title: "x", Kind: "saga"}); err == nil {
		t.Error("an unknown kind must be refused")
	}
	if _, err := ts.CreateFull(TodoCreate{Title: "x", Priority: 9}); err == nil {
		t.Error("a priority out of 0-4 must be refused")
	}
	if _, err := ts.CreateFull(TodoCreate{Title: "x", Estimate: -1}); err == nil {
		t.Error("a negative estimate must be refused")
	}
}

// --- cycles -----------------------------------------------------------------

func TestCycleCreateValidatesWindow(t *testing.T) {
	_, _, _, _, cs := boardWithDepth(t)
	now := time.Now()
	if _, err := cs.Create("bad", "", "", now, now.Add(-time.Hour)); err == nil {
		t.Fatal("an end before the start must be refused")
	}
	if _, err := cs.Create("", "", "", now, now.Add(time.Hour)); err == nil {
		t.Fatal("a nameless cycle must be refused")
	}
	c, err := cs.Create("Sprint 1", "myapp", "ship checkout", now, now.Add(14*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.Goal != "ship checkout" || c.ClosedAt != nil {
		t.Fatalf("unexpected cycle: %#v", c)
	}
}

func TestCycleCloseAndReopen(t *testing.T) {
	_, _, _, _, cs := boardWithDepth(t)
	now := time.Now()
	c, _ := cs.Create("Sprint 1", "", "", now, now.Add(time.Hour))
	closed := true
	got, err := cs.Update(c.ID, CyclePatch{Closed: &closed})
	if err != nil {
		t.Fatal(err)
	}
	if got.ClosedAt == nil {
		t.Fatal("closing must stamp closedAt")
	}
	closed = false
	if got, err = cs.Update(c.ID, CyclePatch{Closed: &closed}); err != nil || got.ClosedAt != nil {
		t.Fatalf("reopening must clear closedAt: %#v (%v)", got, err)
	}
}

func TestVelocityTotalsCommittedAndLanded(t *testing.T) {
	_, ts, _, _, cs := boardWithDepth(t)
	now := time.Now()
	c, _ := cs.Create("Sprint 1", "", "", now.Add(-24*time.Hour), now.Add(24*time.Hour))
	for _, in := range []TodoCreate{
		{Title: "a", CycleID: c.ID, Estimate: 3, Status: "done"},
		{Title: "b", CycleID: c.ID, Estimate: 5, Status: "doing"},
		{Title: "c", CycleID: c.ID}, // unestimated
		{Title: "d", Estimate: 8},   // not in the cycle
	} {
		if _, err := ts.CreateFull(in); err != nil {
			t.Fatal(err)
		}
	}
	rows := Velocity(cs.List(), ts, AllScopes())
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	r := rows[0]
	if r.Cards != 3 || r.CardsDone != 1 || r.Points != 8 || r.PointsDone != 3 || r.Unestimated != 1 {
		t.Fatalf("unexpected velocity row: %#v", r)
	}
}

// Deleting a cycle leaves its cards on the board, unplanned.
func TestUnlinkCycleClearsCards(t *testing.T) {
	_, ts, _, _, cs := boardWithDepth(t)
	now := time.Now()
	c, _ := cs.Create("Sprint 1", "", "", now, now.Add(time.Hour))
	if _, err := ts.CreateFull(TodoCreate{Title: "planned", CycleID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Delete(c.ID); err != nil {
		t.Fatal(err)
	}
	if !ts.UnlinkCycle(c.ID) {
		t.Fatal("the card should have been unlinked")
	}
	if got := ts.List(); len(got) != 1 || got[0].CycleID != "" {
		t.Fatalf("card should survive with no cycle, got %#v", got)
	}
}

// --- burndown ---------------------------------------------------------------

// The burndown is a backwards replay of the event log, so a card that crossed
// into done two days ago must still show as remaining on day one.
func TestBurndownReplaysHistory(t *testing.T) {
	db, ts, _, es, cs := boardWithDepth(t)
	now := time.Now()
	start := startOfDay(now).AddDate(0, 0, -3)
	c, err := cs.Create("Sprint 1", "", "", start, start.AddDate(0, 0, 6))
	if err != nil {
		t.Fatal(err)
	}
	card, err := ts.CreateFull(TodoCreate{Title: "sized", CycleID: c.ID, Estimate: 4})
	if err != nil {
		t.Fatal(err)
	}
	// Move it to done, then rewrite both events onto the timeline: created on
	// day 0, finished on day 2. Append stamps time.Now(), and a burndown over
	// real days needs events on real days.
	if _, err := ts.Update(card.ID, TodoPatch{Status: strPtr("done")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM todo_events`); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		kind, from, to string
		at             time.Time
	}{
		{"created", "", "backlog", start.Add(time.Hour)},
		{"status", "backlog", "done", start.AddDate(0, 0, 2).Add(time.Hour)},
	} {
		if _, err := db.Exec(
			`INSERT INTO todo_events(todo_id,ts,kind,from_val,to_val) VALUES(?,?,?,?,?)`,
			card.ID, timeToNano(e.at), e.kind, e.from, e.to); err != nil {
			t.Fatal(err)
		}
	}

	bd, err := ComputeBurndown(c, ts, es, now, AllScopes())
	if err != nil {
		t.Fatal(err)
	}
	if len(bd.Points) != 4 { // day 0..3, today included
		t.Fatalf("want 4 days of points, got %d: %#v", len(bd.Points), bd.Points)
	}
	if bd.Points[0].Remaining != 4 {
		t.Errorf("day 0 should still owe 4 points, got %v", bd.Points[0].Remaining)
	}
	if bd.Points[1].Remaining != 4 {
		t.Errorf("day 1 should still owe 4 points, got %v", bd.Points[1].Remaining)
	}
	if bd.Points[2].Remaining != 0 || bd.Points[2].Done != 4 {
		t.Errorf("day 2 should be burnt down, got %#v", bd.Points[2])
	}
	if bd.Cards != 1 || bd.CardsDone != 1 || bd.Unestimated != 0 {
		t.Errorf("unexpected totals: %#v", bd)
	}
	// The ideal line starts at the committed total and slopes to zero.
	if bd.Points[0].Ideal != 4 {
		t.Errorf("the ideal line should start at the committed total, got %v", bd.Points[0].Ideal)
	}
	_ = es
}

// A card created after a given day must not appear on that day's total.
func TestBurndownIgnoresCardsNotYetCreated(t *testing.T) {
	db, ts, _, es, cs := boardWithDepth(t)
	now := time.Now()
	start := startOfDay(now).AddDate(0, 0, -2)
	c, _ := cs.Create("Sprint 1", "", "", start, start.AddDate(0, 0, 5))
	card, err := ts.CreateFull(TodoCreate{Title: "late arrival", CycleID: c.ID, Estimate: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM todo_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO todo_events(todo_id,ts,kind,from_val,to_val) VALUES(?,?,?,?,?)`,
		card.ID, timeToNano(startOfDay(now).Add(time.Hour)), "created", "", "backlog"); err != nil {
		t.Fatal(err)
	}
	bd, err := ComputeBurndown(c, ts, es, now, AllScopes())
	if err != nil {
		t.Fatal(err)
	}
	if bd.Points[0].Total != 0 {
		t.Errorf("day 0 predates the card, want 0 points, got %v", bd.Points[0].Total)
	}
	if last := bd.Points[len(bd.Points)-1]; last.Total != 7 {
		t.Errorf("today should carry the card, got %v", last.Total)
	}
}

func strPtr(s string) *string { return &s }
