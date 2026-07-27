package main

import "testing"

func TestGroupStoreCRUDAndPersistence(t *testing.T) {
	db := newTestDataDB(t)
	gs := NewGroupStore(db)

	g, err := gs.Upsert("  workspace  ", []string{" alpha ", "", "beta", "alpha", "gamma"})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if g.Name != "workspace" {
		t.Fatalf("name should trim, got %q", g.Name)
	}
	// Members are trimmed, de-duplicated and keep their order.
	if len(g.Projects) != 3 || g.Projects[0] != "alpha" || g.Projects[1] != "beta" || g.Projects[2] != "gamma" {
		t.Fatalf("want [alpha beta gamma], got %v", g.Projects)
	}

	// Upsert on an existing name replaces the member set.
	if _, err := gs.Upsert("workspace", []string{"alpha"}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if _, err := gs.Upsert("Bundle", []string{"delta", "epsilon"}); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	// List is case-insensitively name-sorted.
	list := gs.List()
	if len(list) != 2 || list[0].Name != "Bundle" || list[1].Name != "workspace" {
		t.Fatalf("want [Bundle workspace], got %+v", list)
	}
	if len(list[1].Projects) != 1 || list[1].Projects[0] != "alpha" {
		t.Fatalf("upsert should replace members, got %v", list[1].Projects)
	}

	// Reload: a fresh store over the same db is a restart.
	gs2 := NewGroupStore(db)
	if got := gs2.List(); len(got) != 2 || len(got[0].Projects) != 2 {
		t.Fatalf("groups should persist across reload, got %+v", got)
	}

	if err := gs2.Delete("Bundle"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := NewGroupStore(db).List(); len(got) != 1 || got[0].Name != "workspace" {
		t.Fatalf("delete should persist, got %+v", got)
	}
}

func TestGroupStoreValidation(t *testing.T) {
	gs := NewGroupStore(newTestDataDB(t))

	if _, err := gs.Upsert("   ", nil); err == nil {
		t.Fatal("blank name should be rejected")
	}
	// The name doubles as an URL path segment.
	if _, err := gs.Upsert("a/b", nil); err == nil {
		t.Fatal("a name with '/' should be rejected")
	}
	if err := gs.Delete("nope"); err != errGroupNotFound {
		t.Fatalf("want errGroupNotFound, got %v", err)
	}
	// An empty member set is legal (a group being assembled).
	g, err := gs.Upsert("empty", nil)
	if err != nil || len(g.Projects) != 0 {
		t.Fatalf("empty group should be allowed, got %+v (%v)", g, err)
	}
}
