package mcpserver

// MCP endpoint (route `/mcp`, streamable HTTP): exposes the design library
// (`#/design`), the todo board (`#/board`) and the docs wiki (`#/docs`) to MCP
// clients. Register it in Claude Code with
//
//	claude mcp add -s user --transport http wyac http://127.0.0.1:4777/mcp
//
// Tools wrap the same stores as the /api handlers (drawings, todos, docs, and
// the scope switcher's project groups) and broadcast the same SSE events, so
// open tabs stay in sync with MCP writes. Delete is deliberately not exposed
// for any of them — destructive ops stay in the UI.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/ships"
	"watch-your-ai-code/internal/sse"
)

type drawingIDInput struct {
	ID string `json:"id" jsonschema:"drawing id, as returned by list_drawings"`
}

type createDrawingInput struct {
	Name string `json:"name" jsonschema:"display name for the new wireframe"`
	// Group is the design tab the drawing lands in — a project name or a
	// custom label; empty keeps it in Ungrouped.
	Group string `json:"group,omitempty" jsonschema:"design tab to create it in — a project name or a custom label; omit for Ungrouped"`
	// Topics tag the drawing within its tab — the grid groups by them.
	Topics []string `json:"topics,omitempty" jsonschema:"topic tags — the design grid renders a section per topic, and a drawing can carry several; omit for untagged"`
}

type setDrawingTopicsInput struct {
	ID     string   `json:"id" jsonschema:"drawing id, as returned by list_drawings"`
	Topics []string `json:"topics" jsonschema:"the FULL new topic set, not a delta — an empty list untags the drawing"`
}

type renameDrawingInput struct {
	ID   string `json:"id" jsonschema:"drawing id, as returned by list_drawings"`
	Name string `json:"name" jsonschema:"the new display name"`
}

type updateDrawingInput struct {
	ID      string `json:"id" jsonschema:"drawing id, as returned by list_drawings"`
	Content string `json:"content" jsonschema:"full .excalidraw scene JSON; replaces the current scene"`
	// BaseUpdatedAt is optional but strongly recommended: it turns the write
	// into a compare-and-swap so a human editing in the app isn't clobbered.
	BaseUpdatedAt string `json:"baseUpdatedAt,omitempty" jsonschema:"the drawing's updatedAt from when you last read it (get_drawing/list_drawings); the write fails with a conflict if someone saved since, instead of overwriting their work"`
}

type drawingListOutput struct {
	Drawings []board.Drawing `json:"drawings"`
}

type todoListInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"only cards for this project, named as the board reports it (e.g. watch-your-ai-code); omit for the whole board"`
}

type todoListOutput struct {
	Todos []board.Todo `json:"todos"`
}

type createTodoInput struct {
	Title    string  `json:"title" jsonschema:"one-line card title; renders as markdown"`
	Note     string  `json:"note,omitempty" jsonschema:"optional markdown body holding the details"`
	Repo     string  `json:"repo,omitempty" jsonschema:"project the card belongs to, named as the board reports it"`
	Status   string  `json:"status,omitempty" jsonschema:"which column to land in — a state id from list_board_states; defaults to backlog"`
	Kind     string  `json:"kind,omitempty" jsonschema:"epic, story, task (default) or bug"`
	ParentID string  `json:"parentId,omitempty" jsonschema:"nest under this card id (from list_todos); the board nests two levels deep, so the parent must itself be top level"`
	Priority int     `json:"priority,omitempty" jsonschema:"0 none (default), 1 low, 2 medium, 3 high, 4 urgent"`
	Estimate float64 `json:"estimate,omitempty" jsonschema:"story points — relative size, not hours; 0 means unestimated"`
	CycleID  string  `json:"cycleId,omitempty" jsonschema:"plan into this cycle (sprint) id, from list_cycles"`
}

// updateTodoInput mirrors board.TodoPatch: every field is optional, and only the
// ones present are touched.
type updateTodoInput struct {
	ID               string    `json:"id" jsonschema:"todo id, as returned by list_todos — note the #N shown on a card is its seq, not its id"`
	Title            *string   `json:"title,omitempty" jsonschema:"replace the title"`
	Note             *string   `json:"note,omitempty" jsonschema:"replace the whole markdown note (read it first if you mean to append)"`
	Repo             *string   `json:"repo,omitempty" jsonschema:"replace the project name"`
	Labels           *[]string `json:"labels,omitempty" jsonschema:"replace the whole label list"`
	Status           *string   `json:"status,omitempty" jsonschema:"move the card to this column — a state id from list_board_states. Landing in a done-category column freezes the linked sessions' summed tokens/cost onto the card"`
	LinkedSessionIDs *[]string `json:"linkedSessionIds,omitempty" jsonschema:"replace the linked session list (empty unlinks all). A ticket may span several sessions and the done snapshot sums them; to add yours, send the existing ids plus your own"`
	LinkedDrawingIDs *[]string `json:"linkedDrawingIds,omitempty" jsonschema:"replace the linked wireframe list (empty unlinks all); ids come from list_drawings"`
	LinkedDocIDs     *[]string `json:"linkedDocIds,omitempty" jsonschema:"replace the linked docs-wiki page list (empty unlinks all); ids come from list_docs"`
	ParentID         *string   `json:"parentId,omitempty" jsonschema:"re-nest under this card id; empty string un-nests to top level. The parent must be top level itself, and a card that already has children cannot be nested"`
	Kind             *string   `json:"kind,omitempty" jsonschema:"epic, story, task or bug"`
	Priority         *int      `json:"priority,omitempty" jsonschema:"0 none, 1 low, 2 medium, 3 high, 4 urgent"`
	Estimate         *float64  `json:"estimate,omitempty" jsonschema:"story points; 0 clears the estimate"`
	CycleID          *string   `json:"cycleId,omitempty" jsonschema:"move into this cycle (sprint) id from list_cycles; empty string takes it out of any cycle"`
}

type stateListInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"only the columns this project sees (its own plus the shared ones); omit for every column"`
}

type stateListOutput struct {
	States []board.TodoState `json:"states"`
}

type cycleListInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"only cycles this project sees; omit for all of them"`
}

type cycleListOutput struct {
	Cycles []board.CycleReport `json:"cycles"`
}

type createCycleInput struct {
	Name     string `json:"name" jsonschema:"display name, e.g. Sprint 12"`
	Repo     string `json:"repo,omitempty" jsonschema:"project that owns the cycle, named as the board reports it; omit for a shared cycle every scope sees"`
	Goal     string `json:"goal,omitempty" jsonschema:"one-line goal shown on the cycle row"`
	StartsAt string `json:"startsAt" jsonschema:"window start — RFC3339, or a bare YYYY-MM-DD which lands at local midnight"`
	EndsAt   string `json:"endsAt" jsonschema:"window end — RFC3339, or a bare YYYY-MM-DD which lands at local 23:59:59; must be after startsAt"`
}

// updateCycleInput mirrors board.CyclePatch: every field is optional, and only
// the ones present are touched. Dates travel as strings so a bare YYYY-MM-DD
// gets the same local-zone reading create_cycle gives it.
type updateCycleInput struct {
	ID       string  `json:"id" jsonschema:"cycle id, as returned by list_cycles"`
	Name     *string `json:"name,omitempty" jsonschema:"rename the cycle"`
	Goal     *string `json:"goal,omitempty" jsonschema:"replace the goal; empty string clears it"`
	Repo     *string `json:"repo,omitempty" jsonschema:"re-own to this project; empty string makes it a shared cycle"`
	StartsAt *string `json:"startsAt,omitempty" jsonschema:"move the window start — RFC3339 or bare YYYY-MM-DD (local midnight)"`
	EndsAt   *string `json:"endsAt,omitempty" jsonschema:"move the window end — RFC3339 or bare YYYY-MM-DD (local 23:59:59); must stay after startsAt"`
	Closed   *bool   `json:"closed,omitempty" jsonschema:"true closes the cycle (the server stamps the moment), false reopens it; an end date passing does not close a cycle on its own"`
}

type docIDInput struct {
	ID string `json:"id" jsonschema:"doc id, as returned by list_docs"`
}

type createDocInput struct {
	Title    string `json:"title" jsonschema:"title for the new page"`
	ParentID string `json:"parentId,omitempty" jsonschema:"id of the parent page to nest under (from list_docs); omit for a top-level page"`
	// Group is the page's project scope; empty inherits from the parent tree
	// (unscoped on a root), while a child's own group overrides — the page
	// lifts to the top of that scope's tree.
	Group string `json:"group,omitempty" jsonschema:"project scope — a project or project-group name; omit to inherit from the parent tree (on a top-level page, omitting means visible only under 'all projects')"`
}

type updateDocInput struct {
	ID      string `json:"id" jsonschema:"doc id, as returned by list_docs"`
	Content string `json:"content" jsonschema:"full markdown body; replaces the current page body"`
	// BaseUpdatedAt is optional but strongly recommended: it turns the write
	// into a compare-and-swap so a human editing in the app isn't clobbered.
	BaseUpdatedAt string `json:"baseUpdatedAt,omitempty" jsonschema:"the doc's updatedAt from when you last read it (get_doc/list_docs); the write fails with a conflict if someone saved since, instead of overwriting their work"`
}

// moveDocInput retitles and/or re-nests a page; both fields are optional, and
// only the ones present are touched.
type moveDocInput struct {
	ID       string  `json:"id" jsonschema:"doc id, as returned by list_docs"`
	Title    *string `json:"title,omitempty" jsonschema:"rename the page"`
	ParentID *string `json:"parentId,omitempty" jsonschema:"re-nest under this parent page id; empty string moves it to the top level. A page cannot move under itself or its descendants"`
}

type docListOutput struct {
	Docs []board.Doc `json:"docs"`
}

type groupListOutput struct {
	Groups []board.ProjectGroup `json:"groups"`
}

type upsertGroupInput struct {
	Name     string   `json:"name" jsonschema:"group name — doubles as the scope label in the nav"`
	Projects []string `json:"projects" jsonschema:"the FULL member set (project names, as the sessions index reports them); replaces the previous set"`
}

type listShipsInput struct {
	Project string `json:"project,omitempty" jsonschema:"only records for this project (as its Makefile reports it, e.g. watch-your-ai-code); omit for all projects"`
	Days    int    `json:"days,omitempty" jsonschema:"only records from the last N days; omit or 0 for all time"`
	WithLog bool   `json:"withLog,omitempty" jsonschema:"include each run's captured log tail — verbose, ask only when debugging a failed run"`
}

// cycleTime parses one cycle bound. RFC3339 is taken as-is; a bare YYYY-MM-DD
// is read in the server's zone — midnight for a start, 23:59:59 for an end —
// the same rule the cycles view applies, because a bare date parsed as UTC
// shifts the window a day for anyone east of Greenwich.
func cycleTime(s string, endOfDay bool) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("date is required — RFC3339 or YYYY-MM-DD")
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	d, err := time.ParseInLocation("2006-01-02", s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("bad date %q — use RFC3339 or YYYY-MM-DD", s)
	}
	if endOfDay {
		y, m, day := d.Date()
		d = time.Date(y, m, day, 23, 59, 59, 0, time.Local)
	}
	return d, nil
}

// newServer builds the "wyac" MCP server over the drawing, todo and doc
// stores. ix is read-only here — it backs the done snapshot.
func newServer(drawings *board.DrawingStore, todos *board.TodoStore, states *board.StateStore, cycles *board.CycleStore, docs *board.DocStore, groups *board.GroupStore, projects *board.ProjectStore, settings *board.SettingsStore, shipStore *ships.Store, sessions board.Sessions, hub *sse.Hub) *mcp.Server {
	// scoped resolves a repo label the way every HTTP handler does (scope.go),
	// and follows presentation mode: while the toggle hides private projects
	// from the UI it hides them here too — one source of truth, so a screen
	// share can't leak through an MCP-driven session either.
	scoped := func(label string) board.ScopeSet {
		s := board.ResolveScope(strings.TrimSpace(label), groups, projects)
		if settings != nil && settings.PresentationHidden() {
			names := projects.PrivateNames()
			s = s.WithExclude(names)
		}
		return s
	}
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "wyac",
		Title:   "W4tch y0ur 4I c0d3",
		Version: "local",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_drawings",
		Description: "List every wireframe in the design library, most recently updated first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, drawingListOutput, error) {
		return nil, drawingListOutput{Drawings: drawings.List()}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_drawing",
		Description: "Read one wireframe's scene as raw .excalidraw JSON (text content), plus its metadata (structured). Keep the updatedAt — pass it as baseUpdatedAt when you update_drawing.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in drawingIDInput) (*mcp.CallToolResult, board.Drawing, error) {
		content, err := drawings.Content(in.ID)
		if err != nil {
			return nil, board.Drawing{}, err
		}
		d, err := drawings.Get(in.ID)
		if err != nil {
			return nil, board.Drawing{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(content)}},
		}, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_drawing",
		Description: "Create a new, empty wireframe in the design library, optionally in a group tab (a project name or a custom label) and with topic tags (the grid groups by them).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createDrawingInput) (*mcp.CallToolResult, board.Drawing, error) {
		d, err := drawings.Create(in.Name, in.Group)
		if err != nil {
			return nil, board.Drawing{}, err
		}
		// The drawing exists from here on — broadcast even if tagging fails,
		// or open tabs wouldn't see it until some unrelated refresh.
		defer hub.Broadcast("drawings-updated", drawings.List())
		if len(in.Topics) > 0 {
			tagged, terr := drawings.SetTopics(d.ID, in.Topics)
			if terr != nil {
				return nil, board.Drawing{}, fmt.Errorf("drawing %s was created, but tagging failed: %w — set_drawing_topics to retry", d.ID, terr)
			}
			d = tagged
		}
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "set_drawing_topics",
		Description: "Replace one wireframe's topic tags (metadata only — the scene is untouched). The design grid renders a section per topic; a drawing carrying several appears under each.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in setDrawingTopicsInput) (*mcp.CallToolResult, board.Drawing, error) {
		d, err := drawings.SetTopics(in.ID, in.Topics)
		if err != nil {
			return nil, board.Drawing{}, err
		}
		hub.Broadcast("drawings-updated", drawings.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "rename_drawing",
		Description: "Rename one wireframe in the design library (metadata only — the scene is untouched).",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in renameDrawingInput) (*mcp.CallToolResult, board.Drawing, error) {
		d, err := drawings.Rename(in.ID, in.Name)
		if err != nil {
			return nil, board.Drawing{}, err
		}
		hub.Broadcast("drawings-updated", drawings.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_drawing",
		Description: "Replace one wireframe's scene with new .excalidraw JSON. Overwrites the whole scene — read it with get_drawing first when editing, and pass its updatedAt as baseUpdatedAt so a save from the app in the meantime fails the write instead of being clobbered. On a conflict, re-read and re-apply.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateDrawingInput) (*mcp.CallToolResult, board.Drawing, error) {
		var base time.Time
		if in.BaseUpdatedAt != "" {
			var err error
			if base, err = time.Parse(time.RFC3339Nano, in.BaseUpdatedAt); err != nil {
				return nil, board.Drawing{}, fmt.Errorf("bad baseUpdatedAt %q (want RFC3339)", in.BaseUpdatedAt)
			}
		}
		d, err := drawings.SetContent(in.ID, []byte(in.Content), base)
		if errors.Is(err, board.ErrDrawingConflict) {
			return nil, board.Drawing{}, fmt.Errorf("conflict: the drawing was saved by someone else after your baseUpdatedAt — get_drawing again and re-apply your change")
		}
		if err != nil {
			return nil, board.Drawing{}, err
		}
		hub.Broadcast("drawings-updated", drawings.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_todos",
		Description: "List the board's cards, column-ordered (the workflow's own column order — see list_board_states). Each card carries its id, its #seq, title, note, repo, labels, kind, priority, estimate, parentId, cycleId, linked session/wireframe ids, a rollup of its children, and — once done — the frozen cost snapshot.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in todoListInput) (*mcp.CallToolResult, todoListOutput, error) {
		list := todos.List()
		// Resolve rather than compare: `repo` may name a GROUP or a parent in the
		// rail tree, and an exact match answered those with zero cards even
		// though every card sat inside them. See scope.go.
		if in := scoped(in.Repo); !in.All || len(in.Exclude) > 0 {
			kept := make([]board.Todo, 0, len(list))
			for _, t := range list {
				if in.Covers(t.Repo) {
					kept = append(kept, t)
				}
			}
			list = kept
		}
		return nil, todoListOutput{Todos: list}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_todo",
		Description: "Add a card to the board. Title and note render as markdown. An epic and its children can be created in one pass: create the epic, then create each child with parentId set to the epic's id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createTodoInput) (*mcp.CallToolResult, board.Todo, error) {
		t, err := todos.CreateFull(board.TodoCreate{
			Title:    in.Title,
			Note:     in.Note,
			Repo:     in.Repo,
			Status:   in.Status,
			Kind:     in.Kind,
			ParentID: in.ParentID,
			Priority: in.Priority,
			Estimate: in.Estimate,
			CycleID:  in.CycleID,
		})
		if err != nil {
			return nil, board.Todo{}, err
		}
		hub.Broadcast("todos-updated", todos.List())
		return nil, t, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_board_states",
		Description: "List the board's workflow columns in order. A column's id is what create_todo/update_todo take as `status`, and its category (todo | started | done) is what decides whether landing there freezes a card's cost snapshot — so read this before moving a card, rather than assuming backlog/doing/done.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in stateListInput) (*mcp.CallToolResult, stateListOutput, error) {
		return nil, stateListOutput{
			States: states.ListForScope(scoped(in.Repo)),
		}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_cycles",
		Description: "List the board's cycles (sprints), newest first, each with what it committed to and what has landed: card counts, story points, and how many cards carry no estimate. Use a cycle's id as create_todo/update_todo's cycleId to plan work into it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in cycleListInput) (*mcp.CallToolResult, cycleListOutput, error) {
		sc := scoped(in.Repo)
		return nil, cycleListOutput{Cycles: board.Velocity(cycles.ListForScope(sc), todos, sc)}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_cycle",
		Description: "Open a new cycle (sprint) — a named window cards are planned into by setting their cycleId (create_todo/update_todo). Dates take RFC3339 or a bare YYYY-MM-DD read in the server's zone: a start lands at midnight, an end at 23:59:59.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createCycleInput) (*mcp.CallToolResult, board.Cycle, error) {
		starts, err := cycleTime(in.StartsAt, false)
		if err != nil {
			return nil, board.Cycle{}, fmt.Errorf("startsAt: %w", err)
		}
		ends, err := cycleTime(in.EndsAt, true)
		if err != nil {
			return nil, board.Cycle{}, fmt.Errorf("endsAt: %w", err)
		}
		c, err := cycles.Create(in.Name, in.Repo, in.Goal, starts, ends)
		if err != nil {
			return nil, board.Cycle{}, err
		}
		hub.Broadcast("cycles-updated", cycles.List())
		return nil, c, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_cycle",
		Description: "Update one cycle. Only the fields you send are touched. Send closed=true when the sprint is over — the server stamps the close moment and the velocity table keeps the row — or closed=false to reopen it.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateCycleInput) (*mcp.CallToolResult, board.Cycle, error) {
		p := board.CyclePatch{Name: in.Name, Goal: in.Goal, Repo: in.Repo, Closed: in.Closed}
		if in.StartsAt != nil {
			t, err := cycleTime(*in.StartsAt, false)
			if err != nil {
				return nil, board.Cycle{}, fmt.Errorf("startsAt: %w", err)
			}
			p.StartsAt = &t
		}
		if in.EndsAt != nil {
			t, err := cycleTime(*in.EndsAt, true)
			if err != nil {
				return nil, board.Cycle{}, fmt.Errorf("endsAt: %w", err)
			}
			p.EndsAt = &t
		}
		c, err := cycles.Update(in.ID, p)
		if errors.Is(err, board.ErrCycleNotFound) {
			return nil, board.Cycle{}, fmt.Errorf("no cycle with id %q — see list_cycles", in.ID)
		}
		if err != nil {
			return nil, board.Cycle{}, err
		}
		hub.Broadcast("cycles-updated", cycles.List())
		return nil, c, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_todo",
		Description: "Update one card. Only the fields you send are touched; list-valued fields replace the whole list. Use this to link the session you are running in to the card you are working on, or to move a card between columns.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateTodoInput) (*mcp.CallToolResult, board.Todo, error) {
		if in.LinkedDrawingIDs != nil {
			if bad := board.UnknownDrawingID(drawings, *in.LinkedDrawingIDs); bad != "" {
				return nil, board.Todo{}, fmt.Errorf("unknown drawing id %q — see list_drawings", bad)
			}
		}
		if in.LinkedDocIDs != nil {
			if bad := board.UnknownDocID(docs, *in.LinkedDocIDs); bad != "" {
				return nil, board.Todo{}, fmt.Errorf("unknown doc id %q — see list_docs", bad)
			}
		}
		t, err := todos.Update(in.ID, board.TodoPatch{
			Title:            in.Title,
			Note:             in.Note,
			Repo:             in.Repo,
			Labels:           in.Labels,
			Status:           in.Status,
			LinkedSessionIDs: in.LinkedSessionIDs,
			LinkedDrawingIDs: in.LinkedDrawingIDs,
			LinkedDocIDs:     in.LinkedDocIDs,
			ParentID:         in.ParentID,
			Kind:             in.Kind,
			Priority:         in.Priority,
			Estimate:         in.Estimate,
			CycleID:          in.CycleID,
		})
		if errors.Is(err, board.ErrTodoNotFound) {
			return nil, board.Todo{}, fmt.Errorf("no todo with id %q — see list_todos (the #N on a card is its seq, not its id)", in.ID)
		}
		if err != nil {
			return nil, board.Todo{}, err
		}
		if in.Status != nil {
			t = board.RefreezeTodo(todos, sessions, t, *in.Status)
		}
		hub.Broadcast("todos-updated", todos.List())
		return nil, t, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_docs",
		Description: "List every page in the docs wiki (metadata only: id, title, parentId, order, timestamps). parentId ties the tree together — \"\" is a top-level page. Keep a page's updatedAt to pass as baseUpdatedAt when you update_doc.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, docListOutput, error) {
		return nil, docListOutput{Docs: docs.List()}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_doc",
		Description: "Read one wiki page's markdown body (text content), plus its metadata (structured). Keep the updatedAt — pass it as baseUpdatedAt when you update_doc.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in docIDInput) (*mcp.CallToolResult, board.Doc, error) {
		body, err := docs.Content(in.ID)
		if err != nil {
			return nil, board.Doc{}, err
		}
		d, err := docs.Get(in.ID)
		if err != nil {
			return nil, board.Doc{}, err
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: body}},
		}, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_doc",
		Description: "Create a new, empty page in the docs wiki. Nest it by passing an existing page id as parentId; omit for a top-level page. A top-level page can carry a project scope via group. Write its body with update_doc.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in createDocInput) (*mcp.CallToolResult, board.Doc, error) {
		d, err := docs.Create(in.Title, in.ParentID, in.Group)
		if err != nil {
			return nil, board.Doc{}, err
		}
		hub.Broadcast("docs-updated", docs.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_doc",
		Description: "Replace one wiki page's markdown body. Overwrites the whole body — read it with get_doc first when editing, and pass its updatedAt as baseUpdatedAt so a save from the app in the meantime fails the write instead of being clobbered. On a conflict, re-read and re-apply.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in updateDocInput) (*mcp.CallToolResult, board.Doc, error) {
		var base time.Time
		if in.BaseUpdatedAt != "" {
			var err error
			if base, err = time.Parse(time.RFC3339Nano, in.BaseUpdatedAt); err != nil {
				return nil, board.Doc{}, fmt.Errorf("bad baseUpdatedAt %q (want RFC3339)", in.BaseUpdatedAt)
			}
		}
		d, err := docs.SetContent(in.ID, in.Content, base)
		if errors.Is(err, board.ErrDocConflict) {
			return nil, board.Doc{}, fmt.Errorf("conflict: the page was saved by someone else after your baseUpdatedAt — get_doc again and re-apply your change")
		}
		if err != nil {
			return nil, board.Doc{}, err
		}
		hub.Broadcast("docs-updated", docs.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_doc",
		Description: "Rename a wiki page and/or re-nest it under a different parent (metadata only — the body is untouched). Pass parentId \"\" to move it to the top level. A page cannot move under itself or its descendants.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in moveDocInput) (*mcp.CallToolResult, board.Doc, error) {
		d, err := docs.Update(in.ID, board.DocPatch{Title: in.Title, ParentID: in.ParentID})
		if errors.Is(err, board.ErrDocNotFound) {
			return nil, board.Doc{}, fmt.Errorf("no doc with id %q — see list_docs", in.ID)
		}
		if err != nil {
			return nil, board.Doc{}, err
		}
		hub.Broadcast("docs-updated", docs.List())
		return nil, d, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_ships",
		Description: "Ship history across the solo projects: every recorded `make check` / `make release` run (project, kind, version, exit code, duration, when), newest first, from the ~/.wyac/ships drop records. Use it to see what actually shipped — e.g. to note on a done card which version carried it, or to check whether the last release's gates were green.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in listShipsInput) (*mcp.CallToolResult, ships.ShipsResult, error) {
		return nil, shipStore.List(strings.TrimSpace(in.Project), in.Days, 100, in.WithLog), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_groups",
		Description: "List the project groups behind the nav's global scope switcher — each is a named set of project names, so one scope covers several repos.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, groupListOutput, error) {
		return nil, groupListOutput{Groups: groups.List()}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "upsert_group",
		Description: "Create a project group, or replace an existing group's member set — projects is the FULL new set, not a delta. Items labelled with the group's own name also match its scope. Renaming and deleting stay in the UI (the scope select's \"+ groups…\" panel), like every other destructive op.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in upsertGroupInput) (*mcp.CallToolResult, board.ProjectGroup, error) {
		g, err := groups.Upsert(in.Name, in.Projects)
		if err != nil {
			return nil, board.ProjectGroup{}, err
		}
		hub.Broadcast("groups-updated", groups.List())
		return nil, g, nil
	})

	return server
}

// Handler serves the MCP server on a streamable HTTP endpoint.
func Handler(drawings *board.DrawingStore, todos *board.TodoStore, states *board.StateStore, cycles *board.CycleStore, docs *board.DocStore, groups *board.GroupStore, projects *board.ProjectStore, settings *board.SettingsStore, shipStore *ships.Store, sessions board.Sessions, hub *sse.Hub) http.Handler {
	server := newServer(drawings, todos, states, cycles, docs, groups, projects, settings, shipStore, sessions, hub)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}
