package main

// Cycles — the board's sprints. A named window with a start and an end that
// cards are planned into, plus the two reports that make planning worth the
// bookkeeping: a burndown for the cycle you are in, and a velocity table for
// the ones you closed.
//
// Named Cycle and NOT Milestone on purpose: `Milestone` is already taken in
// types.go for something unrelated — the plan/branch/commit/pr/release beats
// this app infers from a session transcript. Two meanings under one name in
// one package is a debugging afternoon nobody needs.
//
// The burndown is computed by REPLAYING todo_events backwards from the current
// board (events.go), never from stored daily totals. Current state is the one
// thing guaranteed correct, so walking back from it cannot drift; a nightly
// snapshot table would silently record whatever the board looked like the one
// night the process was asleep.

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Cycle is one planning window. A nil ClosedAt means still open — an end date
// that has passed does not close a cycle, the user does.
//
// ClosedAt is a POINTER because `omitempty` does nothing for a time.Time: a
// zero one marshals as "0001-01-01T00:00:00Z", which is a truthy string to
// every client, so an open cycle rendered as closed. A pointer omits.
type Cycle struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Repo     string     `json:"repo,omitempty"`
	Goal     string     `json:"goal,omitempty"`
	StartsAt time.Time  `json:"startsAt"`
	EndsAt   time.Time  `json:"endsAt"`
	ClosedAt *time.Time `json:"closedAt,omitempty"`
}

var errCycleNotFound = errors.New("cycle not found")

// CycleStore persists the cycles to data.db (cycles). Same write model as the
// other stores: single writer, in-memory serving copy, DB-first.
type CycleStore struct {
	db *sql.DB

	mu     sync.Mutex
	cycles []*Cycle
}

func NewCycleStore(db *sql.DB) *CycleStore {
	cs := &CycleStore{db: db}
	cs.loadDB()
	return cs
}

func (cs *CycleStore) loadDB() {
	rows, err := cs.db.Query(`SELECT id,name,repo,goal,starts_at,ends_at,closed_at FROM cycles`)
	if err != nil {
		log.Printf("cycles: load: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		c := &Cycle{}
		var starts, ends, closed int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Repo, &c.Goal, &starts, &ends, &closed); err != nil {
			log.Printf("cycles: load row: %v", err)
			continue
		}
		c.StartsAt, c.EndsAt = nanoToTime(starts), nanoToTime(ends)
		if closed != 0 {
			at := nanoToTime(closed)
			c.ClosedAt = &at
		}
		cs.cycles = append(cs.cycles, c)
	}
}

// persist writes one cycle through. Callers hold cs.mu.
func (cs *CycleStore) persist(c *Cycle) error {
	closed := int64(0)
	if c.ClosedAt != nil {
		closed = timeToNano(*c.ClosedAt)
	}
	res, err := cs.db.Exec(
		`UPDATE cycles SET name=?,repo=?,goal=?,starts_at=?,ends_at=?,closed_at=? WHERE id=?`,
		c.Name, c.Repo, c.Goal, timeToNano(c.StartsAt), timeToNano(c.EndsAt), closed, c.ID)
	if err != nil {
		return fmt.Errorf("persist cycle: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	if _, err := cs.db.Exec(
		`INSERT INTO cycles(id,name,repo,goal,starts_at,ends_at,closed_at) VALUES(?,?,?,?,?,?,?)`,
		c.ID, c.Name, c.Repo, c.Goal, timeToNano(c.StartsAt), timeToNano(c.EndsAt), closed); err != nil {
		return fmt.Errorf("persist cycle: %w", err)
	}
	return nil
}

func sortCycles(out []Cycle) {
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.After(out[j].StartsAt) // newest first
		}
		return out[i].Name < out[j].Name
	})
}

// List returns every cycle, newest window first.
func (cs *CycleStore) List() []Cycle {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Cycle, 0, len(cs.cycles))
	for _, c := range cs.cycles {
		out = append(out, *c)
	}
	sortCycles(out)
	return out
}

// ListFor returns the cycles one scope sees: the shared ones plus that
// project's own — the same union rule the workflow columns use.
func (cs *CycleStore) ListFor(repo string) []Cycle {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]Cycle, 0, len(cs.cycles))
	for _, c := range cs.cycles {
		if c.Repo == "" || c.Repo == repo {
			out = append(out, *c)
		}
	}
	sortCycles(out)
	return out
}

func (cs *CycleStore) Get(id string) (Cycle, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.cycles {
		if c.ID == id {
			return *c, true
		}
	}
	return Cycle{}, false
}

// Create opens a new cycle. An end before the start is refused outright: every
// report divides by the window length, and a negative one produces charts that
// look like data.
func (cs *CycleStore) Create(name, repo, goal string, starts, ends time.Time) (Cycle, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Cycle{}, fmt.Errorf("name is required")
	}
	if starts.IsZero() || ends.IsZero() {
		return Cycle{}, fmt.Errorf("startsAt and endsAt are required")
	}
	if !ends.After(starts) {
		return Cycle{}, fmt.Errorf("endsAt must be after startsAt")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := &Cycle{
		ID:       randomID(),
		Name:     name,
		Repo:     strings.TrimSpace(repo),
		Goal:     strings.TrimSpace(goal),
		StartsAt: starts,
		EndsAt:   ends,
	}
	if err := cs.persist(c); err != nil {
		return Cycle{}, err
	}
	cs.cycles = append(cs.cycles, c)
	return *c, nil
}

// cyclePatch is a partial cycle update; nil fields stay untouched. Closed is a
// bool rather than a timestamp so the client never picks the close moment.
type cyclePatch struct {
	Name     *string    `json:"name"`
	Goal     *string    `json:"goal"`
	Repo     *string    `json:"repo"`
	StartsAt *time.Time `json:"startsAt"`
	EndsAt   *time.Time `json:"endsAt"`
	Closed   *bool      `json:"closed"`
}

func (cs *CycleStore) Update(id string, p cyclePatch) (Cycle, error) {
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		return Cycle{}, fmt.Errorf("name is required")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, c := range cs.cycles {
		if c.ID != id {
			continue
		}
		next := *c
		if p.Name != nil {
			next.Name = strings.TrimSpace(*p.Name)
		}
		if p.Goal != nil {
			next.Goal = strings.TrimSpace(*p.Goal)
		}
		if p.Repo != nil {
			next.Repo = strings.TrimSpace(*p.Repo)
		}
		if p.StartsAt != nil {
			next.StartsAt = *p.StartsAt
		}
		if p.EndsAt != nil {
			next.EndsAt = *p.EndsAt
		}
		if !next.EndsAt.After(next.StartsAt) {
			return Cycle{}, fmt.Errorf("endsAt must be after startsAt")
		}
		if p.Closed != nil {
			if *p.Closed && next.ClosedAt == nil {
				at := time.Now()
				next.ClosedAt = &at
			} else if !*p.Closed {
				next.ClosedAt = nil
			}
		}
		if err := cs.persist(&next); err != nil {
			return Cycle{}, err
		}
		*c = next
		return next, nil
	}
	return Cycle{}, errCycleNotFound
}

// Delete removes one cycle. Its cards keep existing — the caller clears their
// cycleId (TodoStore.UnlinkCycle), the way a deleted drawing is unlinked.
func (cs *CycleStore) Delete(id string) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for i, c := range cs.cycles {
		if c.ID != id {
			continue
		}
		if _, err := cs.db.Exec(`DELETE FROM cycles WHERE id=?`, id); err != nil {
			return fmt.Errorf("delete cycle: %w", err)
		}
		cs.cycles = append(cs.cycles[:i], cs.cycles[i+1:]...)
		return nil
	}
	return errCycleNotFound
}

// RenameRepo re-points a project's cycles at its new name.
func (cs *CycleStore) RenameRepo(old, name string) int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if _, err := cs.db.Exec(`UPDATE cycles SET repo=? WHERE repo=?`, name, old); err != nil {
		log.Printf("cycles: rename repo: %v", err)
		return 0
	}
	n := 0
	for _, c := range cs.cycles {
		if c.Repo == old {
			c.Repo = name
			n++
		}
	}
	return n
}

// --- reports ---------------------------------------------------------------

// BurndownPoint is one day of a cycle. Ideal is the straight line from the
// first day's committed total down to zero on the last — drawn alongside
// Remaining so a flat week is visible rather than merely felt.
type BurndownPoint struct {
	Date      string  `json:"date"` // YYYY-MM-DD
	Total     float64 `json:"total"`
	Done      float64 `json:"done"`
	Remaining float64 `json:"remaining"`
	Ideal     float64 `json:"ideal"`
}

// Burndown is a cycle's day-by-day chart plus the totals as they stand now.
type Burndown struct {
	CycleID   string          `json:"cycleId"`
	Points    []BurndownPoint `json:"points"`
	Cards     int             `json:"cards"`
	CardsDone int             `json:"cardsDone"`
	// Unestimated counts cards in the cycle carrying no points. The chart
	// cannot see them at all, so it says how many it is blind to instead of
	// quietly drawing a burndown that omits half the work.
	Unestimated int `json:"unestimated"`
}

// cardAt is one card's replayable state.
type cardAt struct {
	cycle    string
	status   string
	estimate float64
	exists   bool
}

// undoEvent rewinds one recorded change, turning the current board into the
// board as it stood just before that event. Only the fields a burndown reads
// are rewound; a retitled card is not history.
func undoEvent(cur map[string]*cardAt, e TodoEvent) {
	c := cur[e.TodoID]
	if c == nil {
		return // a card deleted since — unreconstructable, and counted as absent
	}
	switch e.Kind {
	case "created":
		c.exists = false
	case "status":
		c.status = e.From
	case "cycle":
		c.cycle = e.From
	case "estimate":
		v, err := strconv.ParseFloat(e.From, 64)
		if err != nil {
			return
		}
		c.estimate = v
	}
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// ComputeBurndown replays the event log backwards from the live board to draw
// one point per day of the cycle, up to today (a future day has no data and is
// left off rather than flat-lined).
//
// Cards deleted since are absent from the live board and therefore invisible
// here — their events reference an id nothing can reconstruct. That is a known
// and deliberate hole: it is better than inventing a card from its event trail.
func ComputeBurndown(c Cycle, todos *TodoStore, events *EventStore, now time.Time) (Burndown, error) {
	out := Burndown{CycleID: c.ID}
	cur := map[string]*cardAt{}
	for _, t := range todos.List() {
		if t.CycleID == c.ID {
			out.Cards++
			if todos.IsDoneStatus(t.Status) {
				out.CardsDone++
			}
			if t.Estimate == 0 {
				out.Unestimated++
			}
		}
		cur[t.ID] = &cardAt{cycle: t.CycleID, status: t.Status, estimate: t.Estimate, exists: true}
	}
	evs, err := events.Since(c.StartsAt)
	if err != nil {
		return out, err
	}

	// Every day boundary is computed in NOW's zone. A cycle's bounds arrive as
	// UTC over JSON, and mixing the two shifted the last day off the end of the
	// chart — the run that caught it produced a three-day window for a
	// four-day-old cycle.
	loc := now.Location()
	first := startOfDay(c.StartsAt.In(loc))
	last := startOfDay(now)
	if end := startOfDay(c.EndsAt.In(loc)); last.After(end) {
		last = end
	}
	var cutoffs []time.Time
	for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
		cut := d.AddDate(0, 0, 1) // a day is measured at its end…
		if cut.After(now) {
			cut = now // …except today, which is measured now.
		}
		cutoffs = append(cutoffs, cut)
	}
	if len(cutoffs) == 0 {
		return out, nil // the cycle has not started yet
	}

	points := make([]BurndownPoint, len(cutoffs))
	i := len(evs) - 1
	for k := len(cutoffs) - 1; k >= 0; k-- {
		for i >= 0 && evs[i].Ts.After(cutoffs[k]) {
			undoEvent(cur, evs[i])
			i--
		}
		var total, done float64
		for _, cd := range cur {
			if !cd.exists || cd.cycle != c.ID {
				continue
			}
			total += cd.estimate
			if todos.IsDoneStatus(cd.status) {
				done += cd.estimate
			}
		}
		points[k] = BurndownPoint{
			Date:      first.AddDate(0, 0, k).Format("2006-01-02"),
			Total:     total,
			Done:      done,
			Remaining: total - done,
		}
	}
	// The ideal line slopes to zero on the END date — not on today — so a cycle
	// halfway through still shows where it should be.
	//
	// It is anchored to the LARGEST total the cycle ever carried, not to day
	// one's. Anchoring to day one drew a flat zero for every cycle opened
	// before its cards were filed, which is the normal order of events and was
	// exactly what the first verification run produced.
	span := int(startOfDay(c.EndsAt.In(loc)).Sub(first).Hours() / 24)
	if span < 1 {
		span = 1
	}
	peak := 0.0
	for _, p := range points {
		if p.Total > peak {
			peak = p.Total
		}
	}
	for k := range points {
		points[k].Ideal = peak * float64(span-k) / float64(span)
		if points[k].Ideal < 0 {
			points[k].Ideal = 0
		}
	}
	out.Points = points
	return out, nil
}

// CycleReport is one row of the velocity table: what a cycle committed to and
// what it actually landed.
type CycleReport struct {
	Cycle       Cycle   `json:"cycle"`
	Cards       int     `json:"cards"`
	CardsDone   int     `json:"cardsDone"`
	Points      float64 `json:"points"`
	PointsDone  float64 `json:"pointsDone"`
	Unestimated int     `json:"unestimated"`
}

// Velocity totals every cycle in a scope from the live board. It reads current
// state rather than replaying history on purpose: a closed cycle's cards do
// not move again, and for an open one "where it stands now" is the honest
// answer to what the row is asking.
func Velocity(cycles []Cycle, todos *TodoStore) []CycleReport {
	byCycle := map[string]*CycleReport{}
	out := make([]CycleReport, 0, len(cycles))
	for i := range cycles {
		r := &CycleReport{Cycle: cycles[i]}
		byCycle[cycles[i].ID] = r
	}
	for _, t := range todos.List() {
		r := byCycle[t.CycleID]
		if r == nil {
			continue
		}
		r.Cards++
		r.Points += t.Estimate
		if t.Estimate == 0 {
			r.Unestimated++
		}
		if todos.IsDoneStatus(t.Status) {
			r.CardsDone++
			r.PointsDone += t.Estimate
		}
	}
	for i := range cycles {
		out = append(out, *byCycle[cycles[i].ID])
	}
	return out
}
