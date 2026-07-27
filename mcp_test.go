package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialMCP connects an in-process MCP client session to a server over fresh
// drawing, todo and doc stores, mirroring what Claude Code does against /mcp.
// The index is empty — no test here needs a real transcript on disk.
func dialMCP(t *testing.T) (*mcp.ClientSession, *DrawingStore, *TodoStore, *DocStore) {
	t.Helper()
	db := newTestDataDB(t)
	ds := NewDrawingStore(db)
	ts := NewTodoStore(db)
	dcs := NewDocStore(db)
	server := newMCPServer(ds, ts, dcs, NewGroupStore(db), NewIndex(t.TempDir()), newSSEHub())

	ctx := context.Background()
	clientTr, serverTr := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTr, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTr, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, ds, ts, dcs
}

func TestMCPListsDesignAndBoardTools(t *testing.T) {
	session, _, _, _ := dialMCP(t)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range res.Tools {
		got[tool.Name] = true
	}
	for _, name := range []string{
		"list_drawings", "get_drawing", "create_drawing", "rename_drawing", "update_drawing", "set_drawing_topics",
		"list_todos", "create_todo", "update_todo",
		"list_docs", "get_doc", "create_doc", "update_doc", "move_doc",
		"list_ships",
		"list_groups", "upsert_group",
	} {
		if !got[name] {
			t.Errorf("tool %q missing (got %v)", name, res.Tools)
		}
	}
	if len(res.Tools) != 17 {
		t.Errorf("want exactly 17 tools (delete stays UI-only across stores), got %d", len(res.Tools))
	}
}

func TestMCPRenameDrawing(t *testing.T) {
	session, ds, _, _ := dialMCP(t)
	ctx := context.Background()
	d, err := ds.Create("old name", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "rename_drawing",
		Arguments: map[string]any{"id": d.ID, "name": "  new name  "},
	})
	if err != nil {
		t.Fatalf("rename_drawing: %v", err)
	}
	if res.IsError {
		t.Fatalf("rename_drawing failed: %v", res.Content)
	}
	if got, _ := ds.Get(d.ID); got.Name != "new name" {
		t.Fatalf("want trimmed rename, got %q", got.Name)
	}

	// Unknown ids and blank names surface as tool errors.
	for _, args := range []map[string]any{
		{"id": "nope", "name": "x"},
		{"id": d.ID, "name": "   "},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "rename_drawing", Arguments: args})
		if err != nil {
			t.Fatalf("rename_drawing(%v): %v", args, err)
		}
		if !res.IsError {
			t.Fatalf("rename_drawing(%v) should be a tool error", args)
		}
	}
}

func TestMCPDrawingRoundTrip(t *testing.T) {
	session, ds, _, _ := dialMCP(t)
	ctx := context.Background()

	// create_drawing returns the new drawing's metadata (group = its tab).
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_drawing",
		Arguments: map[string]any{"name": "login screen", "group": "shop"},
	})
	if err != nil {
		t.Fatalf("create_drawing: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_drawing failed: %v", res.Content)
	}
	meta, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("create_drawing structured content: %#v", res.StructuredContent)
	}
	id, _ := meta["id"].(string)
	if id == "" || meta["name"] != "login screen" || meta["group"] != "shop" {
		t.Fatalf("unexpected drawing metadata: %#v", meta)
	}

	// update_drawing replaces the scene, and the store sees it.
	scene := `{"type":"excalidraw","version":2,"elements":[{"id":"r1"}],"appState":{},"files":{}}`
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_drawing",
		Arguments: map[string]any{"id": id, "content": scene},
	})
	if err != nil {
		t.Fatalf("update_drawing: %v", err)
	}
	if res.IsError {
		t.Fatalf("update_drawing failed: %v", res.Content)
	}
	stored, err := ds.Content(id)
	if err != nil || string(stored) != scene {
		t.Fatalf("store content after update: %q (%v)", stored, err)
	}

	// get_drawing returns the raw scene JSON as text.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_drawing",
		Arguments: map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("get_drawing: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_drawing failed: %v", res.Content)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != scene {
		t.Fatalf("get_drawing content: %#v", res.Content)
	}

	// get_drawing also carries metadata so callers can do conditional writes.
	gotMeta, ok := res.StructuredContent.(map[string]any)
	if !ok || gotMeta["id"] != id || gotMeta["updatedAt"] == "" {
		t.Fatalf("get_drawing should return metadata, got %#v", res.StructuredContent)
	}

	// Unknown ids surface as tool errors, not protocol errors.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_drawing",
		Arguments: map[string]any{"id": "nope"},
	})
	if err != nil {
		t.Fatalf("get_drawing(bad id): %v", err)
	}
	if !res.IsError {
		t.Fatal("get_drawing with unknown id should be a tool error")
	}
}

func TestMCPBoardRoundTrip(t *testing.T) {
	session, _, ts, _ := dialMCP(t)
	ctx := context.Background()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_todo",
		Arguments: map[string]any{"title": "fix login redirect", "repo": "myapp", "status": "doing"},
	})
	if err != nil {
		t.Fatalf("create_todo: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_todo failed: %v", res.Content)
	}
	card, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("create_todo structured content: %#v", res.StructuredContent)
	}
	id, _ := card["id"].(string)
	if id == "" || card["status"] != "doing" {
		t.Fatalf("unexpected card: %#v", card)
	}

	// A card in another repo must not show up under the repo filter.
	if _, err := ts.Create("elsewhere", "", "otherapp", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_todos",
		Arguments: map[string]any{"repo": "myapp"},
	})
	if err != nil {
		t.Fatalf("list_todos: %v", err)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("list_todos structured content: %#v", res.StructuredContent)
	}
	list, _ := out["todos"].([]any)
	if len(list) != 1 {
		t.Fatalf("repo filter should leave 1 card, got %d: %#v", len(list), list)
	}

	// update_todo links sessions and leaves unsent fields alone.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_todo",
		Arguments: map[string]any{"id": id, "linkedSessionIds": []string{"sess-a", " sess-b ", "sess-a", ""}},
	})
	if err != nil {
		t.Fatalf("update_todo: %v", err)
	}
	if res.IsError {
		t.Fatalf("update_todo failed: %v", res.Content)
	}
	got, err := findTodo(ts, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LinkedSessionIDs) != 2 || got.LinkedSessionIDs[1] != "sess-b" {
		t.Fatalf("want deduped+trimmed session links, got %#v", got.LinkedSessionIDs)
	}
	if got.Title != "fix login redirect" || got.Status != "doing" {
		t.Fatalf("update_todo touched fields it wasn't sent: %#v", got)
	}

	// Unknown card ids, drawing ids and doc ids are all tool errors.
	for _, args := range []map[string]any{
		{"id": "nope", "title": "x"},
		{"id": id, "linkedDrawingIds": []string{"not-a-drawing"}},
		{"id": id, "linkedDocIds": []string{"not-a-doc"}},
	} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "update_todo", Arguments: args})
		if err != nil {
			t.Fatalf("update_todo(%v): %v", args, err)
		}
		if !res.IsError {
			t.Fatalf("update_todo(%v) should be a tool error", args)
		}
	}
}

// findTodo pulls one card out of the store by id.
func findTodo(ts *TodoStore, id string) (Todo, error) {
	for _, t := range ts.List() {
		if t.ID == id {
			return t, nil
		}
	}
	return Todo{}, fmt.Errorf("todo %q not in the store", id)
}

func TestMCPUpdateConflictsOnStaleBase(t *testing.T) {
	session, ds, _, _ := dialMCP(t)
	ctx := context.Background()

	d, err := ds.Create("wf", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	stale := d.UpdatedAt.Format(time.RFC3339Nano)
	// Someone saves after our read (fresh base so the timestamp moves on).
	d2, err := ds.SetContent(d.ID, []byte(`{"v":1}`), d.UpdatedAt)
	if err != nil {
		t.Fatalf("set content: %v", err)
	}

	// Writing against the stale base is a tool error and leaves the scene alone.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_drawing",
		Arguments: map[string]any{"id": d.ID, "content": `{"v":2}`, "baseUpdatedAt": stale},
	})
	if err != nil {
		t.Fatalf("update_drawing: %v", err)
	}
	if !res.IsError {
		t.Fatal("stale baseUpdatedAt should be a tool error")
	}
	content, _ := ds.Content(d.ID)
	if string(content) != `{"v":1}` {
		t.Fatalf("conflicted MCP write must not change the scene, got %s", content)
	}

	// The fresh base goes through.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_drawing",
		Arguments: map[string]any{"id": d.ID, "content": `{"v":2}`, "baseUpdatedAt": d2.UpdatedAt.Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatalf("update_drawing: %v", err)
	}
	if res.IsError {
		t.Fatalf("fresh baseUpdatedAt should succeed: %v", res.Content)
	}
}

func TestMCPDocRoundTrip(t *testing.T) {
	session, _, _, dcs := dialMCP(t)
	ctx := context.Background()

	// create_doc returns the new page's metadata; parentId defaults to root
	// and group carries the project scope.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_doc",
		Arguments: map[string]any{"title": "Architecture", "group": "wyac"},
	})
	if err != nil {
		t.Fatalf("create_doc: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_doc failed: %v", res.Content)
	}
	meta, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("create_doc structured content: %#v", res.StructuredContent)
	}
	id, _ := meta["id"].(string)
	if id == "" || meta["title"] != "Architecture" || meta["parentId"] != "" || meta["group"] != "wyac" {
		t.Fatalf("unexpected doc metadata: %#v", meta)
	}

	// update_doc replaces the markdown body, and the store sees it.
	body := "# Architecture\n\nThe server is the single writer."
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "update_doc",
		Arguments: map[string]any{"id": id, "content": body},
	})
	if err != nil {
		t.Fatalf("update_doc: %v", err)
	}
	if res.IsError {
		t.Fatalf("update_doc failed: %v", res.Content)
	}
	stored, err := dcs.Content(id)
	if err != nil || stored != body {
		t.Fatalf("store body after update: %q (%v)", stored, err)
	}

	// get_doc returns the raw markdown as text plus metadata.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_doc",
		Arguments: map[string]any{"id": id},
	})
	if err != nil {
		t.Fatalf("get_doc: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok || text.Text != body {
		t.Fatalf("get_doc content: %#v", res.Content)
	}

	// move_doc nests a second page under the first.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_doc",
		Arguments: map[string]any{"title": "Storage"},
	})
	if err != nil || res.IsError {
		t.Fatalf("create_doc(child): %v %v", err, res.Content)
	}
	childID, _ := res.StructuredContent.(map[string]any)["id"].(string)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "move_doc",
		Arguments: map[string]any{"id": childID, "parentId": id},
	})
	if err != nil {
		t.Fatalf("move_doc: %v", err)
	}
	if res.IsError {
		t.Fatalf("move_doc failed: %v", res.Content)
	}
	if got, _ := dcs.Get(childID); got.ParentID != id {
		t.Fatalf("move_doc should re-nest, parent = %q", got.ParentID)
	}

	// Moving the parent under its own child is a cycle — a tool error, no change.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "move_doc",
		Arguments: map[string]any{"id": id, "parentId": childID},
	})
	if err != nil {
		t.Fatalf("move_doc(cycle): %v", err)
	}
	if !res.IsError {
		t.Fatal("moving a page under its own descendant should be a tool error")
	}
	if got, _ := dcs.Get(id); got.ParentID != "" {
		t.Fatalf("cycle-rejected move must not change the parent, got %q", got.ParentID)
	}
}

func TestMCPGroupRoundTrip(t *testing.T) {
	session, _, _, _ := dialMCP(t)
	ctx := context.Background()

	// upsert_group creates and returns the cleaned group (trimmed name,
	// trimmed + deduped members).
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upsert_group",
		Arguments: map[string]any{"name": " studio ", "projects": []string{" blog ", "wyac", "blog"}},
	})
	if err != nil {
		t.Fatalf("upsert_group: %v", err)
	}
	if res.IsError {
		t.Fatalf("upsert_group failed: %v", res.Content)
	}
	meta, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("upsert_group structured content: %#v", res.StructuredContent)
	}
	projects, _ := meta["projects"].([]any)
	if meta["name"] != "studio" || len(projects) != 2 || projects[0] != "blog" || projects[1] != "wyac" {
		t.Fatalf("unexpected group metadata: %#v", meta)
	}

	// A second upsert replaces the member set, and list_groups sees it.
	if res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upsert_group",
		Arguments: map[string]any{"name": "studio", "projects": []string{"wyac"}},
	}); err != nil || res.IsError {
		t.Fatalf("re-upsert: %v %v", err, res.Content)
	}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_groups"})
	if err != nil || res.IsError {
		t.Fatalf("list_groups: %v", err)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("list_groups structured content: %#v", res.StructuredContent)
	}
	groups, _ := out["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %#v", out)
	}
	g, _ := groups[0].(map[string]any)
	ps, _ := g["projects"].([]any)
	if g["name"] != "studio" || len(ps) != 1 || ps[0] != "wyac" {
		t.Fatalf("upsert should replace members, got %#v", g)
	}

	// A bad name (it doubles as an URL path segment in the REST API) errors
	// instead of writing.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upsert_group",
		Arguments: map[string]any{"name": "a/b"},
	})
	if err != nil {
		t.Fatalf("upsert_group(bad name): %v", err)
	}
	if !res.IsError {
		t.Fatal("a name with '/' should error")
	}
}
