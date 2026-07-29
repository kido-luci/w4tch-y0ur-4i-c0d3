package board

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestProjectStoreCRUDAndPersistence(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)

	p, err := ps.Upsert("  proj-a  ", []string{" f1 ", "", "f2", "f1"}, false, 5, "")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if p.Name != "proj-a" {
		t.Fatalf("name should trim, got %q", p.Name)
	}
	// Folders are trimmed, de-duplicated and keep their order.
	if len(p.Folders) != 2 || p.Folders[0] != "f1" || p.Folders[1] != "f2" {
		t.Fatalf("want [f1 f2], got %v", p.Folders)
	}

	if _, err := ps.Upsert("proj-b", []string{"f3"}, true, 2, ""); err != nil {
		t.Fatalf("upsert second: %v", err)
	}

	// List is ordered by ord (proj-b=2 before proj-a=5).
	list := ps.List()
	if len(list) != 2 || list[0].Name != "proj-b" || list[1].Name != "proj-a" {
		t.Fatalf("want [proj-b proj-a] by ord, got %+v", list)
	}
	if !list[0].Hidden {
		t.Fatalf("proj-b should be hidden")
	}

	// Reload: a fresh store over the same db is a restart.
	ps2 := NewProjectStore(db)
	got := ps2.List()
	if len(got) != 2 || got[0].Name != "proj-b" || !got[0].Hidden || got[1].Ord != 5 {
		t.Fatalf("registry should persist across reload, got %+v", got)
	}

	if err := ps2.Delete("proj-b"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := NewProjectStore(db).List(); len(got) != 1 || got[0].Name != "proj-a" {
		t.Fatalf("delete should persist, got %+v", got)
	}
}

// A folder belongs to exactly one project: claiming it for B strips it off A.
func TestProjectStoreExclusiveOwnership(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)

	if _, err := ps.Upsert("A", []string{"x", "y"}, false, 0, ""); err != nil {
		t.Fatalf("upsert A: %v", err)
	}
	if _, err := ps.Upsert("B", []string{"y", "z"}, false, 1, ""); err != nil {
		t.Fatalf("upsert B: %v", err)
	}

	byName := map[string]Project{}
	for _, p := range ps.List() {
		byName[p.Name] = p
	}
	if a := byName["A"]; len(a.Folders) != 1 || a.Folders[0] != "x" {
		t.Fatalf("A should have lost y, got %v", a.Folders)
	}
	if b := byName["B"]; len(b.Folders) != 2 || b.Folders[0] != "y" || b.Folders[1] != "z" {
		t.Fatalf("B should own [y z], got %v", b.Folders)
	}

	// The strip is persisted, not just in-memory.
	rel := map[string]Project{}
	for _, p := range NewProjectStore(db).List() {
		rel[p.Name] = p
	}
	if a := rel["A"]; len(a.Folders) != 1 || a.Folders[0] != "x" {
		t.Fatalf("strip should persist, got A=%v", a.Folders)
	}
}

func TestProjectStoreValidation(t *testing.T) {
	ps := NewProjectStore(newTestDataDB(t))

	if _, err := ps.Upsert("   ", nil, false, 0, ""); err == nil {
		t.Fatal("blank name should be rejected")
	}
	// The name doubles as an URL path segment.
	if _, err := ps.Upsert("a/b", nil, false, 0, ""); err == nil {
		t.Fatal("a name with '/' should be rejected")
	}
	if err := ps.Delete("nope"); err != ErrProjectNotFound {
		t.Fatalf("want ErrProjectNotFound, got %v", err)
	}
	// An empty folder set is legal (a project with no Claude sessions yet).
	p, err := ps.Upsert("empty", nil, false, 0, "")
	if err != nil || len(p.Folders) != 0 {
		t.Fatalf("empty project should be allowed, got %+v (%v)", p, err)
	}
}

// Seed is add-only: it mirrors names into the registry once, never touches an
// existing (possibly renamed/hidden) row, and appends new ones after the tail.
func TestProjectStoreSeedIsAddOnly(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)

	if n := ps.Seed([]string{"b", "a", "a", "", "  "}); n != 2 {
		t.Fatalf("first seed should add 2 (a, b), got %d", n)
	}
	// Sorted names get ascending ord, so List (ord then name) is a, b.
	list := ps.List()
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("want [a b], got %+v", list)
	}

	// A user hides "a" and renames nothing — Seed must leave that alone.
	if _, err := ps.Upsert("a", []string{"a"}, true, 0, ""); err != nil {
		t.Fatalf("hide a: %v", err)
	}
	if n := ps.Seed([]string{"a", "b", "c"}); n != 1 {
		t.Fatalf("second seed should add only c, got %d", n)
	}
	byName := map[string]Project{}
	for _, p := range ps.List() {
		byName[p.Name] = p
	}
	if !byName["a"].Hidden {
		t.Fatal("seed must not un-hide an existing project")
	}
	if byName["c"].Ord <= byName["b"].Ord {
		t.Fatalf("new seed should append after the tail, got c.ord=%d b.ord=%d", byName["c"].Ord, byName["b"].Ord)
	}
}

func TestProjectRenameValidation(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)
	ps.Seed([]string{"old", "taken"})

	if err := ps.Rename("nope", "x"); err != ErrProjectNotFound {
		t.Fatalf("want ErrProjectNotFound, got %v", err)
	}
	if err := ps.Rename("old", "taken"); err == nil {
		t.Fatal("rename onto an existing project should be rejected")
	}
	if err := ps.Rename("old", "a/b"); err == nil {
		t.Fatal("a name with '/' should be rejected")
	}
	// Success keeps the folders/hidden/ord and persists.
	if _, err := ps.Upsert("old", []string{"f1", "f2"}, true, 9, ""); err != nil {
		t.Fatalf("setup upsert: %v", err)
	}
	if err := ps.Rename("old", "New"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	var got *Project
	for _, p := range NewProjectStore(db).List() {
		if p.Name == "New" {
			pc := p
			got = &pc
		}
		if p.Name == "old" {
			t.Fatal("old name should be gone after rename")
		}
	}
	if got == nil || len(got.Folders) != 2 || !got.Hidden || got.Ord != 9 {
		t.Fatalf("rename should keep folders/hidden/ord, got %+v", got)
	}
}

// A project rename cascades its new name across every label that carried it.
func TestProjectRenameCascade(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)
	ts := NewTodoStore(db)
	ds := NewDocStore(db)
	dr := NewDrawingStore(db)
	gs := NewGroupStore(db)

	ps.Seed([]string{"old"})
	if _, err := ts.Create("card", "", "old", "backlog"); err != nil {
		t.Fatalf("create todo: %v", err)
	}
	if _, err := ds.Create("page", "", "old"); err != nil {
		t.Fatalf("create doc: %v", err)
	}
	if _, err := dr.Create("draw", "old"); err != nil {
		t.Fatalf("create drawing: %v", err)
	}
	if _, err := gs.Upsert("grp", []string{"old", "other"}); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	if err := ps.Rename("old", "New"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n := ts.RenameRepo("old", "New"); n != 1 {
		t.Fatalf("todo cascade: want 1, got %d", n)
	}
	if n := ds.RenameGroup("old", "New"); n != 1 {
		t.Fatalf("doc cascade: want 1, got %d", n)
	}
	if n := dr.RenameGroup("old", "New"); n != 1 {
		t.Fatalf("drawing cascade: want 1, got %d", n)
	}
	if n := gs.RenameMember("old", "New"); n != 1 {
		t.Fatalf("group cascade: want 1, got %d", n)
	}

	// Persisted across a reload (fresh stores over the same db).
	if r := NewTodoStore(db).List()[0].Repo; r != "New" {
		t.Fatalf("todo repo: want New, got %q", r)
	}
	if g := NewDocStore(db).List()[0].Group; g != "New" {
		t.Fatalf("doc group: want New, got %q", g)
	}
	if g := NewDrawingStore(db).List()[0].Group; g != "New" {
		t.Fatalf("drawing group: want New, got %q", g)
	}
	got := NewGroupStore(db).List()[0].Projects
	if len(got) != 2 || got[0] != "New" || got[1] != "other" {
		t.Fatalf("group member: want [New other], got %v", got)
	}
}

// Renaming onto a name a group already holds dedupes rather than duplicating.
func TestProjectRenameMemberDedupes(t *testing.T) {
	gs := NewGroupStore(newTestDataDB(t))
	if _, err := gs.Upsert("g", []string{"old", "keep"}); err != nil {
		t.Fatal(err)
	}
	if n := gs.RenameMember("old", "keep"); n != 1 {
		t.Fatalf("want 1 group changed, got %d", n)
	}
	got := gs.List()[0].Projects
	if len(got) != 1 || got[0] != "keep" {
		t.Fatalf("want deduped [keep], got %v", got)
	}
}

// The parent edge persists, rejects self/cycles, follows a rename and is cut
// when the parent is deleted (children fall back to top-level).
func TestProjectParent(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)

	if _, err := ps.Upsert("games", nil, false, 0, ""); err != nil {
		t.Fatalf("upsert parent: %v", err)
	}
	if _, err := ps.Upsert("bloomrise", nil, false, 1, "games"); err != nil {
		t.Fatalf("upsert child: %v", err)
	}
	// Persists across a reload.
	parentOf := func(store *ProjectStore, name string) string {
		for _, p := range store.List() {
			if p.Name == name {
				return p.Parent
			}
		}
		return "<missing>"
	}
	if got := parentOf(NewProjectStore(db), "bloomrise"); got != "games" {
		t.Fatalf("parent should persist, got %q", got)
	}

	// A project cannot parent itself, and a cycle is refused.
	if _, err := ps.Upsert("games", nil, false, 0, "games"); err == nil {
		t.Fatal("self-parent should be rejected")
	}
	if _, err := ps.Upsert("games", nil, false, 0, "bloomrise"); err == nil {
		t.Fatal("cycle (games→bloomrise→games) should be rejected")
	}

	// Renaming the parent repoints the child.
	if err := ps.Rename("games", "arcade"); err != nil {
		t.Fatalf("rename parent: %v", err)
	}
	if got := parentOf(NewProjectStore(db), "bloomrise"); got != "arcade" {
		t.Fatalf("rename should cascade to child parent, got %q", got)
	}

	// Deleting the parent orphans the child to top-level.
	if err := ps.Delete("arcade"); err != nil {
		t.Fatalf("delete parent: %v", err)
	}
	if got := parentOf(NewProjectStore(db), "bloomrise"); got != "" {
		t.Fatalf("delete should orphan child, got parent %q", got)
	}
}

func TestProjectLogo(t *testing.T) {
	db := newTestDataDB(t)
	ps := NewProjectStore(db)
	ps.Seed([]string{"p"})

	if err := ps.SetLogo("nope", []byte("x"), "image/png", 5); err != ErrProjectNotFound {
		t.Fatalf("set on missing project: want ErrProjectNotFound, got %v", err)
	}
	if _, _, err := ps.Logo("p"); err != errNoLogo {
		t.Fatalf("no logo yet: want errNoLogo, got %v", err)
	}

	if err := ps.SetLogo("p", []byte("PNGDATA"), "image/png", 123); err != nil {
		t.Fatalf("set logo: %v", err)
	}
	data, ct, err := ps.Logo("p")
	if err != nil || string(data) != "PNGDATA" || ct != "image/png" {
		t.Fatalf("logo readback: %q %q %v", data, ct, err)
	}
	// The version rides the list; the bytes never do.
	logoVer := func(store *ProjectStore) int64 {
		for _, pr := range store.List() {
			if pr.Name == "p" {
				return pr.LogoVersion
			}
		}
		return -1
	}
	if v := logoVer(ps); v != 123 {
		t.Fatalf("want logoVersion 123, got %d", v)
	}
	// Persists across a reload (fresh store, same db).
	if d2, _, _ := NewProjectStore(db).Logo("p"); string(d2) != "PNGDATA" {
		t.Fatalf("logo should persist, got %q", d2)
	}

	if err := ps.DeleteLogo("p"); err != nil {
		t.Fatalf("delete logo: %v", err)
	}
	if _, _, err := ps.Logo("p"); err != errNoLogo {
		t.Fatalf("after delete: want errNoLogo, got %v", err)
	}
	if v := logoVer(NewProjectStore(db)); v != 0 {
		t.Fatalf("logoVersion should reset to 0, got %d", v)
	}
}

func TestMigrateAddsProjectsTable(t *testing.T) {
	cfg := t.TempDir()
	path := filepath.Join(cfg, "data.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A v6 fixture: user_version, plus the drawings table every real v6 db
	// carries — the v→10 step ALTERs it, so unlike 7–9 it touches a
	// pre-existing table.
	if _, err := raw.Exec(fixtureDrawingsDDL + `PRAGMA user_version = 6;`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	db, err := OpenDB(cfg)
	if err != nil {
		t.Fatalf("open should migrate v6→v7: %v", err)
	}
	defer db.Close()

	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != dataSchemaVersion {
		t.Fatalf("want user_version %d, got %d (%v)", dataSchemaVersion, v, err)
	}
	ps := NewProjectStore(db)
	if _, err := ps.Upsert("studio", []string{"blog", "wyac"}, false, 0, ""); err != nil {
		t.Fatalf("upsert after migration: %v", err)
	}
	if got := NewProjectStore(db).List(); len(got) != 1 || len(got[0].Folders) != 2 {
		t.Fatalf("project should persist across reload, got %+v", got)
	}
}
