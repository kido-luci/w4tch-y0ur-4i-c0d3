package main

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// runningWindow: a session whose transcript changed within this window is
// considered live.
const runningWindow = 60 * time.Second

// sessionStamp captures everything on disk that feeds one session's parse
// result, so re-scans can skip sessions whose inputs are unchanged.
type sessionStamp struct {
	mainMod time.Time
	mainSz  int64
	subSig  string // concatenated "name:mtime:size" of subagent files
}

type Index struct {
	root string

	// db is the on-disk cache (see db.go): parsed sessions warm-load from it,
	// fresh parses persist to it, and Search queries its FTS table. nil (as in
	// most tests) means no persistence and an empty search — never an error.
	db *sql.DB

	mu       sync.RWMutex
	sessions map[string]*Session     // sessionID -> parsed session
	stamps   map[string]sessionStamp // mainPath -> input signature
	byPath   map[string]string       // mainPath -> sessionID

	// active/archived status comes from the app's session store, refreshed on a
	// timer independently of transcript parsing. It has its own lock so status
	// reads never contend with (or re-enter) the session lock above.
	archMu    sync.RWMutex
	activeIDs map[string]bool
	hasStore  bool
}

func NewIndex(root string) *Index {
	return &Index{
		root:      root,
		sessions:  map[string]*Session{},
		stamps:    map[string]sessionStamp{},
		byPath:    map[string]string{},
		activeIDs: map[string]bool{},
	}
}

// refreshArchived reloads the active-session set from the app's session store.
func (ix *Index) refreshArchived() {
	active, ok := loadActiveIDs()
	ix.archMu.Lock()
	ix.activeIDs = active
	ix.hasStore = ok
	ix.archMu.Unlock()
}

// archived reports whether a non-running session should be shown as archived:
// the store exists and this id isn't among its active sessions ("no entry =
// archived"). A running session is always active; an absent store means the
// status is unknown, so nothing is hidden.
func (ix *Index) archived(id string, running bool) bool {
	if running {
		return false
	}
	ix.archMu.RLock()
	defer ix.archMu.RUnlock()
	return ix.hasStore && !ix.activeIDs[id]
}

// sessionFiles lists all main-session transcripts: <root>/<proj>/<uuid>.jsonl.
// Subagent transcripts live deeper (<proj>/<uuid>/subagents/) and are
// deliberately not returned — they are inputs to their session's parse.
func (ix *Index) sessionFiles() ([]string, error) {
	var out []string
	projects, err := os.ReadDir(ix.root)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(ix.root, p.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
				out = append(out, filepath.Join(dir, e.Name()))
			}
		}
	}
	return out, nil
}

func stampFor(mainPath string) (sessionStamp, bool) {
	fi, err := os.Stat(mainPath)
	if err != nil {
		return sessionStamp{}, false
	}
	st := sessionStamp{mainMod: fi.ModTime(), mainSz: fi.Size()}
	subDir := filepath.Join(strings.TrimSuffix(mainPath, ".jsonl"), "subagents")

	// Walk, don't ReadDir: Workflow agents live in subagents/workflows/<runId>/,
	// and a one-level listing only sees that directory's own mtime — which does
	// not move when a file inside it grows. A running workflow agent would have
	// gone unnoticed until something else re-stamped the session.
	//
	// Sorted, because WalkDir's order is stable but the signature must not
	// depend on that: a reordering would read as a change and re-parse the world.
	var parts []string
	_ = filepath.WalkDir(subDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry shouldn't sink the stamp
		}
		info, err := d.Info()
		if err != nil {
			return nil //nolint:nilerr
		}
		parts = append(parts, path+":"+info.ModTime().String()+":"+strconv.FormatInt(info.Size(), 10))
		return nil
	})
	sort.Strings(parts)
	st.subSig = strings.Join(parts, ";")
	return st, true
}

// Rescan parses every session whose on-disk inputs changed since the last
// scan (everything, on first call). Returns the IDs of updated sessions —
// sessions warm-loaded from the cache are installed but not returned: nothing
// about them changed, so nothing should broadcast.
func (ix *Index) Rescan() ([]string, error) {
	paths, err := ix.sessionFiles()
	if err != nil {
		return nil, err
	}

	type job struct {
		path  string
		stamp sessionStamp
	}
	type cacheHit struct {
		path  string
		stamp sessionStamp
		sess  *Session
	}
	var jobs []job
	var cached []cacheHit
	keep := make(map[string]bool, len(paths))
	ix.mu.RLock()
	for _, p := range paths {
		keep[p] = true
		st, ok := stampFor(p)
		if !ok {
			continue
		}
		prev, seen := ix.stamps[p]
		if seen && prev == st {
			continue
		}
		// First sight of this path (boot, typically): the cache may hold its
		// parse. A changed stamp skips the lookup — the cached row is exactly
		// as stale as the in-memory one.
		if !seen {
			if s, ok := ix.dbLookup(p, st); ok {
				cached = append(cached, cacheHit{p, st, s})
				continue
			}
		}
		jobs = append(jobs, job{p, st})
	}
	ix.mu.RUnlock()

	if len(cached) > 0 {
		ix.mu.Lock()
		for _, c := range cached {
			ix.sessions[c.sess.ID] = c.sess
			ix.stamps[c.path] = c.stamp
			ix.byPath[c.path] = c.sess.ID
		}
		ix.mu.Unlock()
		log.Printf("index cache: %d sessions warm-loaded", len(cached))
	}
	ix.dbPrune(keep)
	if len(jobs) == 0 {
		return nil, nil
	}

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	jobCh := make(chan job)
	var wg sync.WaitGroup
	var updMu sync.Mutex
	var updated []string

	// Fresh parses stream to the cache off the parse path: a persist failure
	// costs the cache, never the scan.
	persistCh := make(chan persistReq, workers)
	persistDone := make(chan struct{})
	if ix.db != nil {
		go func() {
			ix.dbPersist(persistCh)
			close(persistDone)
		}()
	} else {
		close(persistDone)
	}

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				s, texts, err := parseSession(j.path)
				if err != nil {
					log.Printf("parse %s: %v", j.path, err)
					continue
				}
				ix.mu.Lock()
				ix.sessions[s.ID] = s
				ix.stamps[j.path] = j.stamp
				ix.byPath[j.path] = s.ID
				ix.mu.Unlock()
				updMu.Lock()
				updated = append(updated, s.ID)
				updMu.Unlock()
				if ix.db != nil {
					persistCh <- persistReq{j.path, j.stamp, s, texts}
				}
			}
		}()
	}
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	close(persistCh)
	<-persistDone
	return updated, nil
}

// RescanSession re-parses a single session by any path under it (main file
// or a subagent file). Returns the session if it was re-parsed.
func (ix *Index) RescanSession(changedPath string) *Session {
	mainPath := changedPath
	// a path under <proj>/<uuid>/subagents/... maps to <proj>/<uuid>.jsonl
	if i := strings.Index(filepath.ToSlash(changedPath), "/subagents/"); i >= 0 {
		mainPath = changedPath[:i] + ".jsonl"
	}
	if !strings.HasSuffix(mainPath, ".jsonl") {
		return nil
	}
	st, ok := stampFor(mainPath)
	if !ok {
		return nil
	}
	s, texts, err := parseSession(mainPath)
	if err != nil {
		log.Printf("parse %s: %v", mainPath, err)
		return nil
	}
	ix.mu.Lock()
	ix.sessions[s.ID] = s
	ix.stamps[mainPath] = st
	ix.byPath[mainPath] = s.ID
	ix.mu.Unlock()
	ix.dbPersistOne(persistReq{mainPath, st, s, texts})
	return s
}

func (s *Session) withRunning(now time.Time) *Session {
	c := *s
	c.Running = now.Sub(c.EndedAt) < runningWindow
	if len(s.Agents) > 0 {
		c.Agents = make([]AgentRun, len(s.Agents))
		copy(c.Agents, s.Agents)
		for i := range c.Agents {
			a := &c.Agents[i]
			a.Running = now.Sub(a.EndedAt) < runningWindow
			// Background spawns are logged as "async_launched" and the rollup
			// is never rewritten on completion — normalize by file freshness.
			if a.Status == "async_launched" {
				if a.Running {
					a.Status = "running"
				} else {
					a.Status = "completed"
				}
			}
		}
	}
	return &c
}

// withStatus returns a copy of s with its live (Running) and Archived flags set.
func (ix *Index) withStatus(s *Session, now time.Time) *Session {
	c := s.withRunning(now)
	c.Archived = ix.archived(c.ID, c.Running)
	return c
}

// Sessions returns summaries (no Agents) newest-first, filtered by window,
// project, and status. days<=0 means no time filter; project may carry several
// comma-separated names (a group scope) — any match keeps the session; status
// "active"/"archived" keeps only that set, anything else keeps all.
func (ix *Index) Sessions(days int, project, status string) []*Session {
	now := time.Now()
	var cutoff time.Time
	if days > 0 {
		cutoff = now.AddDate(0, 0, -days)
	}
	var projects map[string]bool
	if list := splitProjects(project); list != nil {
		projects = make(map[string]bool, len(list))
		for _, p := range list {
			projects[p] = true
		}
	}
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*Session, 0, len(ix.sessions))
	for _, s := range ix.sessions {
		if !cutoff.IsZero() && s.EndedAt.Before(cutoff) {
			continue
		}
		if projects != nil && !projects[s.Project] {
			continue
		}
		c := ix.withStatus(s, now)
		switch status {
		case "active":
			if c.Archived {
				continue
			}
		case "archived":
			if !c.Archived {
				continue
			}
		}
		c.Agents = nil
		// Detail-only breakdowns — keep the list payload lean.
		c.MainToolStats = nil
		c.MainTools = nil
		c.MainActivity = nil
		c.MainFlow = nil
		c.MainToolEvents = nil
		c.MainToolEventsDropped = 0
		c.InterruptTimes = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndedAt.After(out[j].EndedAt) })
	return out
}

// splitProjects parses a project query param — one name, or several
// comma-separated (a group scope: the group's name plus its members) — into a
// list, dropping empties; nil means unfiltered.
func splitProjects(project string) []string {
	if project == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(project, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Session returns one session with its agent runs.
func (ix *Index) Session(id string) *Session {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	s, ok := ix.sessions[id]
	if !ok {
		return nil
	}
	return ix.withStatus(s, time.Now())
}

// The three accessors below are what lets repo resolution, search and the ship
// records live outside this package: a method must sit with its receiver's
// type, a plain function needn't. Each consumer declares the one-method
// interface it needs and takes that instead of the whole index.
//
// They hand out STORED pointers, which is only safe because a *Session in the
// map is never mutated in place — Rescan and RescanSession replace the entry
// with a freshly parsed value. Keep it that way: mutating a stored session
// would turn every reader here into a data race.

// Snapshot returns the parsed sessions as of now, safe to read after the index
// lock is released.
func (ix *Index) Snapshot() []*Session {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]*Session, 0, len(ix.sessions))
	for _, s := range ix.sessions {
		out = append(out, s)
	}
	return out
}

// SessionRef returns the raw parse for id, without the live-status decoration
// Session applies. For callers that only need a session's identifying fields
// and would otherwise pay to copy the whole thing (agent runs included) per
// lookup. nil when there is no such session.
func (ix *Index) SessionRef(id string) *Session {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.sessions[id]
}

// DB is the index-cache handle (index.db, see db.go), whose schema this
// package owns. The query layers built over it — search, ship records — take
// it rather than reaching into the index. nil when the cache is disabled,
// which every caller treats as "no rows", never as an error.
func (ix *Index) DB() *sql.DB { return ix.db }

// Churn pivots the index by file instead of by session: which files were
// edited across how many sessions, and the lines those edits moved. Like the
// heatmap it ignores the archived flag — an archived session's edits still
// happened. Sessions are filtered before the pivot, so a file edited from two
// projects reports only the sessions the filter kept.
func (ix *Index) Churn(days int, project string, minSessions, limit int) ChurnResult {
	return churnFrom(ix.Sessions(days, project, ""), minSessions, limit)
}

// churnFrom aggregates sessions' per-file edits into ranked churn rows: files
// touched by at least minSessions sessions, worst first, capped at limit.
// TotalFiles counts everything that passed minSessions, so a capped list can
// say what it dropped.
func churnFrom(sessions []*Session, minSessions, limit int) ChurnResult {
	if minSessions < 1 {
		minSessions = 1
	}
	acc := map[string]*ChurnFile{}
	for _, s := range sessions {
		for _, fe := range s.FileEdits {
			cf := acc[fe.Path]
			if cf == nil {
				cf = &ChurnFile{Path: fe.Path}
				acc[fe.Path] = cf
			}
			cf.Sessions++
			cf.Edits += fe.Edits
			cf.LinesAdded += fe.LinesAdded
			cf.LinesRemoved += fe.LinesRemoved
			if s.EndedAt.After(cf.LastTouched) {
				cf.LastTouched = s.EndedAt
			}
			cf.Refs = append(cf.Refs, ChurnSessionRef{
				ID:           s.ID,
				Title:        s.Title,
				Project:      s.Project,
				EndedAt:      s.EndedAt,
				Edits:        fe.Edits,
				LinesAdded:   fe.LinesAdded,
				LinesRemoved: fe.LinesRemoved,
				CostUSD:      s.CostUSD,
			})
		}
	}

	files := make([]ChurnFile, 0, len(acc))
	for _, cf := range acc {
		if cf.Sessions < minSessions {
			continue
		}
		cf.ChurnedLines = min(cf.LinesAdded, cf.LinesRemoved)
		sort.Slice(cf.Refs, func(i, j int) bool { return cf.Refs[i].EndedAt.After(cf.Refs[j].EndedAt) })
		files = append(files, *cf)
	}
	// Worst first by churned lines, NOT by session count: a file every session
	// appends a line to (MEMORY.md, a changelog) racks up sessions without ever
	// being reworked, and ranking on that buried the actually-rewritten code.
	// Sessions break ties, then path, so the order is stable across rescans.
	sort.Slice(files, func(i, j int) bool {
		a, b := files[i], files[j]
		if a.ChurnedLines != b.ChurnedLines {
			return a.ChurnedLines > b.ChurnedLines
		}
		if a.Sessions != b.Sessions {
			return a.Sessions > b.Sessions
		}
		return a.Path < b.Path
	})

	res := ChurnResult{TotalFiles: len(files)}
	if limit > 0 && len(files) > limit {
		files = files[:limit]
	}
	res.Files = files
	return res
}

// Friction ranks the window's sessions by how often the user stopped them, with
// permission denials as a footnote. Like the heatmap and the churn radar it
// ignores the archived flag — the friction happened either way.
func (ix *Index) Friction(days int, project string, limit int) FrictionResult {
	return frictionFrom(ix.Sessions(days, project, ""), limit)
}

// frictionFrom ranks sessions with any friction, worst first, capped at limit.
// TotalSessions counts every session that had some, so a capped list can say
// what it left out.
func frictionFrom(sessions []*Session, limit int) FrictionResult {
	var res FrictionResult
	denials := map[string]int{}
	out := make([]FrictionSession, 0, len(sessions))
	for _, s := range sessions {
		res.Interrupts += s.Interrupts
		res.Denials += s.Denials
		for _, tc := range s.DenialTools {
			denials[tc.Name] += tc.Count
		}
		if s.Interrupts == 0 && s.Denials == 0 {
			continue
		}
		out = append(out, FrictionSession{
			ID:          s.ID,
			Title:       s.Title,
			Project:     s.Project,
			EndedAt:     s.EndedAt,
			Interrupts:  s.Interrupts,
			Denials:     s.Denials,
			CostUSD:     s.CostUSD + s.AgentCostUSD,
			TotalTokens: s.TotalTokens + s.AgentTokens,
			DurationMs:  s.DurationMs,
		})
	}
	res.DenialTools = sortedToolCounts(denials)
	res.TotalSessions = len(out)

	// Worst first: most interrupts, then denials, then cost — a session you
	// stopped ten times while it burned $40 outranks a cheap one you stopped ten
	// times. Newest breaks the remaining ties, so the order is stable.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Interrupts != b.Interrupts {
			return a.Interrupts > b.Interrupts
		}
		if a.Denials != b.Denials {
			return a.Denials > b.Denials
		}
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.EndedAt.After(b.EndedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	res.Sessions = out
	return res
}

// heavyCompactions is where "this outgrew one sitting" starts. A single
// compaction is routine; three means the context was refilled and blown twice
// over after that.
const heavyCompactions = 3

// Sizing ranks the window's sessions by how often they compacted — the marker
// of work that outgrew its context. Archived sessions count: the work happened.
func (ix *Index) Sizing(days int, project string, limit int) SizingResult {
	return sizingFrom(ix.Sessions(days, project, ""), limit)
}

// sizingFrom ranks the sessions that compacted, worst first, capped at limit,
// and measures what compaction goes with: the median cost of sessions that
// never compacted against those that did it heavily.
func sizingFrom(sessions []*Session, limit int) SizingResult {
	res := SizingResult{Scanned: len(sessions), HeavyThreshold: heavyCompactions}
	var cleanCosts, heavyCosts []float64
	out := make([]SizingSession, 0, len(sessions))

	for _, s := range sessions {
		cost := s.CostUSD + s.AgentCostUSD
		switch {
		case s.CompactCount == 0:
			cleanCosts = append(cleanCosts, cost)
		case s.CompactCount >= heavyCompactions:
			heavyCosts = append(heavyCosts, cost)
		}
		if s.CompactCount == 0 {
			continue
		}
		out = append(out, SizingSession{
			ID:          s.ID,
			Title:       s.Title,
			Project:     s.Project,
			EndedAt:     s.EndedAt,
			Compactions: s.CompactCount,
			CostUSD:     cost,
			TotalTokens: s.TotalTokens + s.AgentTokens,
			DurationMs:  s.DurationMs,
		})
	}

	res.TotalSessions = len(out)
	res.CleanCount, res.HeavyCount = len(cleanCosts), len(heavyCosts)
	res.MedianCostClean = medianOf(cleanCosts)
	res.MedianCostHeavy = medianOf(heavyCosts)

	// Worst first: most compactions, then cost — of two sessions that compacted
	// four times, the $300 one is the one worth splitting next time.
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Compactions != b.Compactions {
			return a.Compactions > b.Compactions
		}
		if a.CostUSD != b.CostUSD {
			return a.CostUSD > b.CostUSD
		}
		return a.EndedAt.After(b.EndedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	res.Sessions = out
	return res
}

// medianOf returns the median of v (0 when empty), without disturbing v.
func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// Ledger measures each week's spend against what came out of it. Archived
// sessions count — the money was spent either way.
func (ix *Index) Ledger(days int, project string) LedgerResult {
	return ledgerFrom(ix.Sessions(days, project, ""))
}

// isoWeekKey labels t's ISO week ("2026-W29") and returns the Monday it starts
// on. Local time, matching the heatmap: a session belongs to the week you were
// sitting in, not the one UTC was in.
func isoWeekKey(t time.Time) (string, time.Time) {
	t = t.Local()
	year, week := t.ISOWeek()
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7 // Sunday closes the ISO week rather than opening it
	}
	monday := time.Date(t.Year(), t.Month(), t.Day()-(wd-1), 0, 0, 0, 0, t.Location())
	return fmt.Sprintf("%d-W%02d", year, week), monday
}

// ledgerFrom buckets sessions by the week they ended and counts the outcomes
// their milestones recorded. A PR or release is counted once, in the week it
// first appeared: 42 of this corpus's 1068 PRs are touched again in a later
// session, and counting those twice would quietly inflate the denominator that
// makes the whole block's numbers look good.
func ledgerFrom(sessions []*Session) LedgerResult {
	weeks := map[string]*LedgerWeek{}
	seenPR := map[string]bool{}
	seenRelease := map[string]bool{} // keyed with the project: v1.0.0 exists twice

	at := func(t time.Time) *LedgerWeek {
		key, monday := isoWeekKey(t)
		w := weeks[key]
		if w == nil {
			w = &LedgerWeek{Week: key, StartsOn: monday}
			weeks[key] = w
		}
		return w
	}

	var total LedgerWeek
	for _, s := range sessions {
		// A session with no usable end has no week to belong to; it would land in
		// a phantom "1-W01" bucket dated year zero.
		if s.EndedAt.IsZero() {
			continue
		}
		cost := s.CostUSD + s.AgentCostUSD
		lines := s.LinesAdded + s.LinesRemoved

		w := at(s.EndedAt)
		w.Sessions++
		w.CostUSD += cost
		w.Lines += lines
		total.Sessions++
		total.CostUSD += cost
		total.Lines += lines

		// Outcomes land in the week they happened, from the milestone's own
		// timestamp — a 38-hour session can open its PR in a different week than
		// the one it finished in.
		for _, m := range s.Milestones {
			if m.Ts.IsZero() {
				continue
			}
			switch m.Kind {
			case "pr":
				if m.URL == "" || seenPR[m.URL] {
					continue
				}
				seenPR[m.URL] = true
				at(m.Ts).PRs++
				total.PRs++
			case "release":
				key := s.Project + "\x00" + m.Label
				if m.Label == "" || seenRelease[key] {
					continue
				}
				seenRelease[key] = true
				at(m.Ts).Releases++
				total.Releases++
			}
		}
	}

	out := make([]LedgerWeek, 0, len(weeks))
	for _, w := range weeks {
		w.CostPerPR = perOutcome(w.CostUSD, float64(w.PRs))
		w.CostPer1kLines = perOutcome(w.CostUSD*1000, float64(w.Lines))
		out = append(out, *w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartsOn.After(out[j].StartsOn) })

	total.CostPerPR = perOutcome(total.CostUSD, float64(total.PRs))
	total.CostPer1kLines = perOutcome(total.CostUSD*1000, float64(total.Lines))
	return LedgerResult{Weeks: out, Total: total}
}

// perOutcome divides, reporting 0 rather than an infinity when a week produced
// nothing to divide by — the UI prints that as "—".
func perOutcome(cost, n float64) float64 {
	if n == 0 {
		return 0
	}
	return cost / n
}

// Projects returns the distinct project names, alphabetical.
func (ix *Index) Projects() []string {
	ix.mu.RLock()
	set := map[string]bool{}
	for _, s := range ix.sessions {
		set[s.Project] = true
	}
	ix.mu.RUnlock()
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
