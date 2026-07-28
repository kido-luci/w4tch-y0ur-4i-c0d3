package main

import (
	"testing"
	"time"
)

// board builds a rail shaped like a real one: a group over two projects, and a
// parent project with a child nested under it.
//
//	luci-studio (group) -> blog-frontend, blog-backend
//	blog-backend (project) -> blog-worker (child)
//	standalone (project, in no group)
func scopeFixture(t *testing.T) (*GroupStore, *ProjectStore) {
	t.Helper()
	db := newTestDataDB(t)
	gs := NewGroupStore(db)
	ps := NewProjectStore(db)
	if _, err := gs.Upsert("luci-studio", []string{"blog-frontend", "blog-backend"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ name, parent string }{
		{"blog-frontend", ""},
		{"blog-backend", ""},
		{"blog-worker", "blog-backend"},
		{"standalone", ""},
	} {
		if _, err := ps.Upsert(p.name, nil, false, 0, p.parent); err != nil {
			t.Fatal(err)
		}
	}
	return gs, ps
}

func TestResolveScopeExpandsGroupsAndTheRailTree(t *testing.T) {
	gs, ps := scopeFixture(t)

	// The all-projects scope is nil: it filters nothing.
	if got := resolveScope("", gs, ps); !got.all {
		t.Fatalf("an empty label must resolve to the all-projects scope, got %v", got)
	}

	// A GROUP covers its members — and, transitively, anything nested under a
	// member. This is the case that used to fail: a cycle stored as
	// "luci-studio" was invisible from blog-backend.
	g := resolveScope("luci-studio", gs, ps)
	for _, want := range []string{"luci-studio", "blog-frontend", "blog-backend", "blog-worker"} {
		if !g.covers(want) && want != "luci-studio" {
			t.Errorf("group scope should cover %q", want)
		}
	}
	if !g.cards["luci-studio"] {
		t.Error("the label itself belongs in the set")
	}
	if g.covers("standalone") {
		t.Error("a project outside the group must stay out of scope")
	}

	// A PARENT project covers its children but not its siblings.
	p := resolveScope("blog-backend", gs, ps)
	if !p.covers("blog-backend") || !p.covers("blog-worker") {
		t.Error("a parent scope should cover itself and its child")
	}
	if p.covers("blog-frontend") {
		t.Error("a sibling project must stay out of scope")
	}
	// And it does NOT reach up: scoping to a member does not pull in the group's
	// other projects.
	if p.covers("standalone") {
		t.Error("scoping to a project must not widen to unrelated ones")
	}

	// A leaf covers only itself.
	leaf := resolveScope("standalone", gs, ps)
	if !leaf.covers("standalone") || leaf.covers("blog-backend") {
		t.Errorf("a leaf scope should cover only itself, got %v", leaf)
	}
}

// A card with no project shows only under all-projects; a piece of CONFIGURATION
// with no project is shared and shows everywhere. Same empty string, opposite
// answers — the distinction the two methods exist for.
func TestScopeSetTreatsEmptyRepoDifferentlyForCardsAndConfig(t *testing.T) {
	gs, ps := scopeFixture(t)
	in := resolveScope("blog-backend", gs, ps)

	if in.covers("") {
		t.Error("a card with no project must be out of scope under a real scope")
	}
	if !in.coversOwner("") {
		t.Error("shared configuration (no repo) must be visible under every scope")
	}

	// Under all-projects both are visible.
	if !allScopes().covers("") || !allScopes().coversOwner("") {
		t.Error("all-projects must show both a card with no project and shared config")
	}
}

// The three stores must agree, since they all answer the same question.
func TestStoresAgreeOnScope(t *testing.T) {
	db := newTestDataDB(t)
	gs := NewGroupStore(db)
	ps := NewProjectStore(db)
	if _, err := gs.Upsert("luci-studio", []string{"blog-backend"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ps.Upsert("blog-backend", nil, false, 0, ""); err != nil {
		t.Fatal(err)
	}
	states := NewStateStore(db)
	cycles := NewCycleStore(db)
	views := NewViewStore(db)

	// One of each owned by the GROUP, which is what the UI stores when the rail
	// is on a group.
	if _, err := states.Create("In review", "started", "luci-studio", 0); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := cycles.Create("Sprint 1", "luci-studio", "", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := views.Create("mine", "luci-studio", "table", nil); err != nil {
		t.Fatal(err)
	}

	// Narrowing to a MEMBER project must still see all three. Before the
	// resolver each store compared the label to the stored repo, so every one
	// of these vanished.
	member := resolveScope("blog-backend", gs, ps)
	if n := len(states.ListForScope(member)); n != 4 { // 3 builtin + the group's
		t.Errorf("states: want 4 columns from a member project, got %d", n)
	}
	if n := len(cycles.ListForScope(member)); n != 1 {
		t.Errorf("cycles: want the group's cycle from a member project, got %d", n)
	}
	if n := len(views.ListForScope(member)); n != 1 {
		t.Errorf("views: want the group's view from a member project, got %d", n)
	}

	// An unrelated project sees only the shared builtins.
	other := resolveScope("standalone", gs, ps)
	if n := len(states.ListForScope(other)); n != 3 {
		t.Errorf("states: an unrelated project should see only the 3 builtins, got %d", n)
	}
	if n := len(cycles.ListForScope(other)); n != 0 {
		t.Errorf("cycles: an unrelated project should see none, got %d", n)
	}
	if n := len(views.ListForScope(other)); n != 0 {
		t.Errorf("views: an unrelated project should see none, got %d", n)
	}
}

// The reports must count the same cards the board shows, or one screen reports
// two different truths — which is exactly what shipped.
func TestReportsCountOnlyCardsInScope(t *testing.T) {
	db := newTestDataDB(t)
	gs := NewGroupStore(db)
	ps := NewProjectStore(db)
	if _, err := gs.Upsert("luci-studio", []string{"blog-backend"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"blog-backend", "elsewhere"} {
		if _, err := ps.Upsert(n, nil, false, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	ss := NewStateStore(db)
	es := NewEventStore(db)
	ts := NewTodoStore(db)
	ts.UseStates(ss)
	ts.UseEvents(es)
	cs := NewCycleStore(db)

	now := time.Now()
	c, err := cs.Create("Sprint 1", "", "", now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A shared cycle holding one card from each project.
	for _, in := range []todoCreate{
		{Title: "in scope", Repo: "blog-backend", CycleID: c.ID, Estimate: 3},
		{Title: "out of scope", Repo: "elsewhere", CycleID: c.ID, Estimate: 5},
	} {
		if _, err := ts.CreateFull(in); err != nil {
			t.Fatal(err)
		}
	}

	all := Velocity(cs.List(), ts, allScopes())
	if all[0].Cards != 2 || all[0].Points != 8 {
		t.Errorf("all projects should see both cards, got %#v", all[0])
	}

	scoped := resolveScope("luci-studio", gs, ps)
	one := Velocity(cs.ListForScope(scoped), ts, scoped)
	if len(one) != 1 || one[0].Cards != 1 || one[0].Points != 3 {
		t.Errorf("a group scope should count only its own card, got %#v", one)
	}

	bd, err := ComputeBurndown(c, ts, es, time.Now(), scoped)
	if err != nil {
		t.Fatal(err)
	}
	if bd.Cards != 1 {
		t.Errorf("the burndown should see only the in-scope card, got %d", bd.Cards)
	}
	if last := bd.Points[len(bd.Points)-1]; last.Total != 3 {
		t.Errorf("the burndown total should be the in-scope points, got %v", last.Total)
	}
}

// The burndown is a drill-down, so it validates its target against the resolved
// scope the way the git tab's endpoints validate ?repo. Without the guard it
// charted a cycle that GET /api/cycles at the same scope reports as absent.
func TestBurndownRefusesACycleOutsideTheScope(t *testing.T) {
	db := newTestDataDB(t)
	gs := NewGroupStore(db)
	ps := NewProjectStore(db)
	if _, err := gs.Upsert("luci-studio", []string{"blog-backend"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"blog-backend", "elsewhere"} {
		if _, err := ps.Upsert(n, nil, false, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	cs := NewCycleStore(db)
	now := time.Now()
	c, err := cs.Create("Sprint 1", "luci-studio", "", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}

	// The scope that owns it, and a member of that group, both see it.
	for _, label := range []string{"", "luci-studio", "blog-backend"} {
		if in := resolveScope(label, gs, ps); !in.coversOwner(c.Repo) {
			t.Errorf("scope %q should see the cycle", label)
		}
	}
	// An unrelated project must not — which is what makes the handler 404.
	if in := resolveScope("elsewhere", gs, ps); in.coversOwner(c.Repo) {
		t.Error("an unrelated project must not see another group's cycle")
	}
}
