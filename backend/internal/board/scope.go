package board

// ONE definition of "in scope", server-side.
//
// The rail's scope label is not always a project name. It can be a project, or a
// group standing for several, or a parent project with children nested under it
// in the rail tree — and the cards themselves only ever carry a plain project
// name. So "is this in scope?" is a set-membership question, never a string
// compare, and the set has to be built by expanding the label.
//
// The client has always done this (getScopeSet in scope.ts). The server did not,
// and three stores added later — workflow columns, cycles and saved views —
// each shipped a `repo == label` compare instead. That reads fine and is wrong
// in a specific way: a column created while scoped to a GROUP stores the group's
// name, so narrowing to one of its member projects made the column vanish, as
// if the board had lost it. Same for a cycle, a saved view, and MCP's
// list_todos, which answered a group name with zero cards.
//
// Hence this file. Every scope question on the server resolves through
// ResolveScope, so a new consumer cannot quietly invent a fourth rule.

// ScopeSet answers two different questions, which is why it is not one set.
//
//   - Cards: which projects' CARDS belong to this view. Resolved DOWNWARD —
//     the label, a group's members, and anything nested under those.
//   - owners: whose CONFIGURATION applies here. Resolved BOTH ways, because a
//     column can be owned by an ancestor (created while the rail sat on the
//     group) or by a descendant (created on a member project).
//
// Conflating them is the bug this file was written to fix, and then the first
// draft of this file repeated it: expanding only downward still hid a column
// owned by the group when the rail narrowed to a member, because the member's
// set never contained the group's name. Both directions, or cards land in a
// column nothing draws.
//
// All and Cards are exported for httpapi/mcpserver, which read them directly
// (the all-projects check, and the /api/scopes name listing); owners has no
// consumer outside this package and stays private.
type ScopeSet struct {
	All    bool            // the all-projects scope: nothing is filtered
	Cards  map[string]bool // projects whose cards are in scope
	owners map[string]bool // labels whose configuration applies here
	// Exclude names projects presentation mode is hiding — consulted before
	// every other answer, including All. A nil map excludes nothing, so plain
	// ResolveScope results are unaffected. Deliberately NOT keyed on the empty
	// string: an unscoped card keeps its usual answer either way, because a
	// card with no project name leaks nothing worth hiding.
	Exclude map[string]bool
}

// Covers reports whether a CARD carrying this repo is in scope.
//
// An empty repo is deliberately out of scope for any real scope: a card with no
// project shows only under all-projects, which is the rule the board has had
// since v0.63. Do not "fix" this to return true.
func (s ScopeSet) Covers(repo string) bool {
	if s.Exclude[repo] {
		return false
	}
	if s.All {
		return true
	}
	if repo == "" {
		return false
	}
	return s.Cards[repo]
}

// CoversOwner reports whether a piece of CONFIGURATION owned by this repo is in
// scope — a workflow column, a cycle, a saved view.
//
// The difference from Covers is the empty string, and it is not an oversight:
// for a card, no repo means unscoped and therefore hidden; for configuration,
// no repo means SHARED and therefore visible everywhere. Two meanings for one
// empty value, so they get two methods rather than one flag.
func (s ScopeSet) CoversOwner(repo string) bool {
	if s.Exclude[repo] {
		return false
	}
	if repo == "" || s.All {
		return true
	}
	return s.owners[repo]
}

// WithExclude returns a copy of s that also refuses everything in names — how
// presentation mode subtracts private projects from an already-resolved scope
// without a second resolution rule. An empty or nil set returns s unchanged.
func (s ScopeSet) WithExclude(names map[string]bool) ScopeSet {
	if len(names) == 0 {
		return s
	}
	s.Exclude = names
	return s
}

// ResolveScope expands a scope label into the two sets above. An empty label is
// the all-projects scope and filters nothing.
//
// The downward walk mirrors getScopeSet in scope.ts step for step, including the
// fixpoint loop, because the two must agree: a client that shows a card and a
// server that hides its column is worse than either rule alone.
func ResolveScope(label string, groups *GroupStore, projects *ProjectStore) ScopeSet {
	if label == "" {
		return ScopeSet{All: true}
	}
	var grps []ProjectGroup
	if groups != nil {
		grps = groups.List()
	}
	var projs []Project
	if projects != nil {
		projs = projects.List()
	}

	// Downward: the label, a group's members, then transitively every project
	// nested under anything already in.
	cards := map[string]bool{label: true}
	for _, g := range grps {
		if g.Name == label {
			for _, p := range g.Projects {
				cards[p] = true
			}
		}
	}
	for {
		grew := false
		for _, p := range projs {
			if p.Parent != "" && cards[p.Parent] && !cards[p.Name] {
				cards[p.Name] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}

	// Upward: the parent chain, plus any group containing something already in.
	// Bounded by the number of projects and groups; ProjectStore already refuses
	// a parent cycle (wouldCycle), so the fixpoint terminates.
	up := map[string]bool{label: true}
	for {
		grew := false
		for _, p := range projs {
			if up[p.Name] && p.Parent != "" && !up[p.Parent] {
				up[p.Parent] = true
				grew = true
			}
		}
		for _, g := range grps {
			if up[g.Name] {
				continue
			}
			for _, member := range g.Projects {
				if up[member] {
					up[g.Name] = true
					grew = true
					break
				}
			}
		}
		if !grew {
			break
		}
	}

	// Configuration from either direction applies: an ancestor's column governs
	// the cards below it, and a descendant's column holds cards this view shows.
	owners := map[string]bool{}
	for k := range cards {
		owners[k] = true
	}
	for k := range up {
		owners[k] = true
	}
	return ScopeSet{Cards: cards, owners: owners}
}

// AllScopes is the all-projects scope, for callers with no label to resolve
// (tests, and any future caller that means "everything").
func AllScopes() ScopeSet { return ScopeSet{All: true} }
