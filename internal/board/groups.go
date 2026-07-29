package board

// Project groups (the nav's global scope, phase 2): a named set of project
// names, so one scope entry can cover several repos — a product that spans a
// backend, a frontend and a tool is one pick instead of three. Tiny and
// durable (they cannot be rebuilt from transcripts), so they live in data.db
// with the same write model as the other stores: this server is the single
// writer, an in-memory slice behind a mutex is the serving copy, and every
// mutation writes its one row through an explicit column list.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
)

// ProjectGroup is one named set of project names. Members are stored as a
// JSON array in a single column, the todos.labels idiom.
type ProjectGroup struct {
	Name     string   `json:"name"`
	Projects []string `json:"projects"`
}

var errGroupNotFound = errors.New("group not found")

// GroupStore persists the project groups to data.db (project_groups).
type GroupStore struct {
	db *sql.DB

	mu     sync.Mutex
	groups []*ProjectGroup
}

// NewGroupStore opens the project groups over data.db.
func NewGroupStore(db *sql.DB) *GroupStore {
	gs := &GroupStore{db: db}
	gs.loadDB()
	return gs
}

func (gs *GroupStore) loadDB() {
	rows, err := gs.db.Query(`SELECT name, projects FROM project_groups`)
	if err != nil {
		log.Printf("groups: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		g := &ProjectGroup{}
		var projects string
		if err := rows.Scan(&g.Name, &projects); err != nil {
			log.Printf("groups: load row: %v", err)
			continue
		}
		_ = json.Unmarshal([]byte(projects), &g.Projects)
		gs.groups = append(gs.groups, g)
	}
}

// find returns the entry for name, or nil. Callers hold gs.mu.
func (gs *GroupStore) find(name string) *ProjectGroup {
	for _, g := range gs.groups {
		if g.Name == name {
			return g
		}
	}
	return nil
}

// List returns every group, case-insensitively sorted by name.
func (gs *GroupStore) List() []ProjectGroup {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	out := make([]ProjectGroup, 0, len(gs.groups))
	for _, g := range gs.groups {
		out = append(out, ProjectGroup{Name: g.Name, Projects: append([]string(nil), g.Projects...)})
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// cleanProjects trims members, drops empties and dedupes, preserving order.
func cleanProjects(projects []string) []string {
	out := make([]string, 0, len(projects))
	seen := make(map[string]bool, len(projects))
	for _, p := range projects {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Upsert creates or replaces one group's member set. The name doubles as the
// scope label and an URL path segment, so "/" is rejected.
func (gs *GroupStore) Upsert(name string, projects []string) (ProjectGroup, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ProjectGroup{}, fmt.Errorf("name is required")
	}
	if strings.Contains(name, "/") {
		return ProjectGroup{}, fmt.Errorf("name cannot contain %q", "/")
	}
	cleaned := cleanProjects(projects)
	raw, err := json.Marshal(cleaned)
	if err != nil {
		return ProjectGroup{}, fmt.Errorf("encode projects: %w", err)
	}
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if _, err := gs.db.Exec(
		`INSERT INTO project_groups(name, projects) VALUES(?, ?)
		 ON CONFLICT(name) DO UPDATE SET projects=excluded.projects`,
		name, string(raw)); err != nil {
		return ProjectGroup{}, fmt.Errorf("write group: %w", err)
	}
	g := gs.find(name)
	if g == nil {
		g = &ProjectGroup{Name: name}
		gs.groups = append(gs.groups, g)
	}
	g.Projects = cleaned
	return ProjectGroup{Name: g.Name, Projects: append([]string(nil), g.Projects...)}, nil
}

// RenameMember swaps a project name inside every group's member list — the
// groups' half of a project rename. Dedupes if the new name was already a
// member. Returns how many groups changed.
func (gs *GroupStore) RenameMember(old, name string) int {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	changed := 0
	for _, g := range gs.groups {
		has := false
		for _, m := range g.Projects {
			if m == old {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		next := make([]string, 0, len(g.Projects))
		seen := make(map[string]bool, len(g.Projects))
		for _, m := range g.Projects {
			v := m
			if v == old {
				v = name
			}
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			next = append(next, v)
		}
		raw, err := json.Marshal(next)
		if err != nil {
			continue
		}
		if _, err := gs.db.Exec(`UPDATE project_groups SET projects=? WHERE name=?`, string(raw), g.Name); err != nil {
			log.Printf("groups: rename member: %v", err)
			continue
		}
		g.Projects = next
		changed++
	}
	return changed
}

// Delete removes one group. Scopes stored client-side keep the name; the
// select just falls back to offering it as a plain (phantom) project.
func (gs *GroupStore) Delete(name string) error {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	if gs.find(name) == nil {
		return errGroupNotFound
	}
	if _, err := gs.db.Exec(`DELETE FROM project_groups WHERE name=?`, name); err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	kept := gs.groups[:0]
	for _, g := range gs.groups {
		if g.Name != name {
			kept = append(kept, g)
		}
	}
	gs.groups = kept
	return nil
}
