package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nestedAgentTree lays out a session's subagents/ the way real ones look: a
// couple directly inside, and Workflow-spawned ones a level deeper under
// workflows/<runId>/. Returns the main transcript's path.
func nestedAgentTree(t *testing.T) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "proj-n")
	sub := filepath.Join(base, "55555555-aaaa-bbbb-cccc-000000000005", "subagents")
	wf := filepath.Join(sub, "workflows", "wf_abc123")
	if err := os.MkdirAll(wf, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(base, "55555555-aaaa-bbbb-cccc-000000000005.jsonl")
	write(main, `{"type":"user","uuid":"m1","timestamp":"2026-07-17T10:00:00.000Z","cwd":"/Users/x/dev/proj-n","message":{"role":"user","content":[{"type":"text","text":"go"}]}}`+"\n")

	// Directly under subagents/ — always been found.
	write(filepath.Join(sub, "agent-top1.jsonl"), `{"type":"assistant","uuid":"t1","timestamp":"2026-07-17T10:01:00.000Z","requestId":"r1","message":{"id":"tm1","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":10},"content":[]}}`+"\n")
	write(filepath.Join(sub, "agent-top1.meta.json"), `{"agentType":"general-purpose","description":"top","spawnDepth":1}`)

	// Under workflows/<runId>/ — invisible to a single-level ReadDir.
	write(filepath.Join(wf, "agent-wf1.jsonl"), `{"type":"assistant","uuid":"w1","timestamp":"2026-07-17T10:02:00.000Z","requestId":"r2","message":{"id":"wm1","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":200,"output_tokens":20},"content":[]}}`+"\n")
	write(filepath.Join(wf, "agent-wf1.meta.json"), `{"agentType":"workflow-subagent","spawnDepth":1}`)
	write(filepath.Join(wf, "agent-wf2.jsonl"), `{"type":"assistant","uuid":"w2","timestamp":"2026-07-17T10:03:00.000Z","requestId":"r3","message":{"id":"wm2","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":300,"output_tokens":30},"content":[]}}`+"\n")
	write(filepath.Join(wf, "agent-wf2.meta.json"), `{"agentType":"workflow-subagent","spawnDepth":1}`)
	return main
}

// Regression: a one-level ReadDir missed every Workflow-spawned agent — 562
// transcripts over 31 real sessions, and sessions whose graph drew a lone main
// node while they had spawned a hundred.
func TestParseSessionFindsNestedWorkflowAgents(t *testing.T) {
	s, _, err := parseSession(nestedAgentTree(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.AgentCount != 3 {
		t.Fatalf("agentCount = %d, want 3 (1 top-level + 2 under workflows/)", s.AgentCount)
	}
	byType := map[string]int{}
	for _, a := range s.Agents {
		byType[a.AgentType]++
	}
	if byType["workflow-subagent"] != 2 || byType["general-purpose"] != 1 {
		t.Errorf("agent types = %v, want 2 workflow + 1 general-purpose", byType)
	}
	// Their tokens must reach the session total: 110 + 220 + 330.
	if s.AgentTokens != 660 {
		t.Errorf("agentTokens = %d, want 660 — nested agents' usage counts too", s.AgentTokens)
	}
	// A workflow agent's meta carries no toolUseId, so it parents to the main
	// session (which is what called the Workflow tool).
	for _, a := range s.Agents {
		if a.AgentType == "workflow-subagent" && a.ParentAgentID != "" {
			t.Errorf("workflow agent %s parented to %q, want main", a.ID, a.ParentAgentID)
		}
	}
}

func TestCollectAgentFilesAtAnyDepth(t *testing.T) {
	main := nestedAgentTree(t)
	sub := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents")

	agents := collectAgentFiles(sub)

	if len(agents) != 3 {
		t.Fatalf("found %d agents, want 3: %v", len(agents), agents)
	}
	for _, id := range []string{"top1", "wf1", "wf2"} {
		af := agents[id]
		if af == nil || af.jsonl == "" || af.meta == "" {
			t.Errorf("agent %q: transcript and meta must both be found, got %+v", id, af)
		}
	}
}

// The cache stamp has to see nested files too, or a running workflow agent's
// growth never triggers a re-parse: the workflows/ directory's own mtime does
// not move when a file inside it is appended to.
func TestStampForSeesNestedAgentChanges(t *testing.T) {
	main := nestedAgentTree(t)
	before, ok := stampFor(main)
	if !ok {
		t.Fatal("stampFor failed")
	}

	nested := filepath.Join(strings.TrimSuffix(main, ".jsonl"), "subagents", "workflows", "wf_abc123", "agent-wf1.jsonl")
	f, err := os.OpenFile(nested, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","uuid":"w3"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	after, _ := stampFor(main)
	if before == after {
		t.Error("a nested agent grew and the stamp didn't move — the session would never re-parse")
	}
}

func churnSession(id, project string, endedAt time.Time, cost float64, edits ...FileEdit) *Session {
	return &Session{ID: id, Title: id, Project: project, EndedAt: endedAt, CostUSD: cost, FileEdits: edits}
}

func TestChurnFromRanking(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	sessions := []*Session{
		// hot.go: three sessions. warm.go: two. cold.go: one (below min).
		churnSession("s1", "p", t0, 1.0,
			FileEdit{Path: "/r/hot.go", Edits: 2, LinesAdded: 10, LinesRemoved: 4},
			FileEdit{Path: "/r/cold.go", Edits: 1, LinesAdded: 99, LinesRemoved: 99}),
		churnSession("s2", "p", t0.Add(time.Hour), 2.0,
			FileEdit{Path: "/r/hot.go", Edits: 1, LinesAdded: 5, LinesRemoved: 5},
			FileEdit{Path: "/r/warm.go", Edits: 1, LinesAdded: 1, LinesRemoved: 0}),
		churnSession("s3", "p", t0.Add(2*time.Hour), 4.0,
			FileEdit{Path: "/r/hot.go", Edits: 3, LinesAdded: 0, LinesRemoved: 20},
			FileEdit{Path: "/r/warm.go", Edits: 1, LinesAdded: 2, LinesRemoved: 0}),
	}

	res := churnFrom(sessions, 2, 50)

	// cold.go moved the most lines but only one session touched it — that isn't
	// rework, so min=2 drops it regardless of size.
	if res.TotalFiles != 2 || len(res.Files) != 2 {
		t.Fatalf("files = %+v, want hot.go + warm.go only", res.Files)
	}
	hot := res.Files[0]
	if hot.Path != "/r/hot.go" {
		t.Fatalf("files[0] = %q, want /r/hot.go", hot.Path)
	}
	if hot.Sessions != 3 || hot.Edits != 6 || hot.LinesAdded != 15 || hot.LinesRemoved != 29 {
		t.Errorf("hot.go = %+v, want 3 sessions, 6 edits, +15/-29", hot)
	}
	// min(15, 29): the writing that got unwritten, not the total lines moved.
	if hot.ChurnedLines != 15 {
		t.Errorf("hot.go churnedLines = %d, want 15", hot.ChurnedLines)
	}
	if !hot.LastTouched.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("hot.go lastTouched = %v, want the newest session's end", hot.LastTouched)
	}
	// Refs are newest first and carry the session's own cost, not a file share.
	if len(hot.Refs) != 3 || hot.Refs[0].ID != "s3" || hot.Refs[2].ID != "s1" {
		t.Fatalf("refs order = %+v, want s3, s2, s1", hot.Refs)
	}
	if hot.Refs[0].CostUSD != 4.0 {
		t.Errorf("ref cost = %v, want the session's full 4.0", hot.Refs[0].CostUSD)
	}
}

// Regression: ranking by session count put append-only journals on top. Real
// data had a MEMORY.md at 116 sessions (+422/-347) outranking a genuinely
// rewritten HomePage.astro at 29 sessions (+3910/-2746). Churned lines — the
// writing that got unwritten — is what the radar is actually looking for.
func TestChurnFromRanksReworkOverTouchCount(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	var sessions []*Session
	// journal.md: appended to by five sessions, almost nothing ever removed.
	for i := 0; i < 5; i++ {
		sessions = append(sessions, churnSession("j", "p", t0.Add(time.Duration(i)*time.Hour), 0,
			FileEdit{Path: "/r/journal.md", Edits: 1, LinesAdded: 10, LinesRemoved: 0}))
	}
	// code.go: two sessions, but most of what was written got deleted again.
	for i := 0; i < 2; i++ {
		sessions = append(sessions, churnSession("c", "p", t0.Add(time.Duration(i)*time.Hour), 0,
			FileEdit{Path: "/r/code.go", Edits: 6, LinesAdded: 100, LinesRemoved: 80}))
	}

	res := churnFrom(sessions, 2, 50)

	if len(res.Files) != 2 {
		t.Fatalf("files = %+v, want both", res.Files)
	}
	if res.Files[0].Path != "/r/code.go" {
		t.Errorf("files[0] = %q, want /r/code.go — rework outranks touch count",
			res.Files[0].Path)
	}
	// The journal is still listed (no path is special-cased), just not on top.
	if res.Files[1].Path != "/r/journal.md" || res.Files[1].ChurnedLines != 0 {
		t.Errorf("files[1] = %+v, want journal.md with 0 churned lines", res.Files[1])
	}
	if res.Files[1].Sessions <= res.Files[0].Sessions {
		t.Error("fixture is wrong: the journal must have MORE sessions than the code")
	}
}

func TestChurnFromLimitReportsTotal(t *testing.T) {
	t0 := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	var sessions []*Session
	// Six files, each touched by two sessions.
	for i := 0; i < 2; i++ {
		var edits []FileEdit
		for _, p := range []string{"/r/a", "/r/b", "/r/c", "/r/d", "/r/e", "/r/f"} {
			edits = append(edits, FileEdit{Path: p, Edits: 1, LinesAdded: 1})
		}
		sessions = append(sessions, churnSession("s", "p", t0, 0, edits...))
	}

	res := churnFrom(sessions, 2, 2)
	if len(res.Files) != 2 {
		t.Errorf("files = %d, want the limit of 2", len(res.Files))
	}
	// The cap must not hide the rest: totalFiles counts everything past min.
	if res.TotalFiles != 6 {
		t.Errorf("totalFiles = %d, want 6 (all that passed min)", res.TotalFiles)
	}
}

func sizingSession(id string, compactions int, cost float64) *Session {
	return &Session{ID: id, Title: id, Project: "p", CompactCount: compactions, CostUSD: cost}
}

func TestSizingFrom(t *testing.T) {
	sessions := []*Session{
		sizingSession("clean1", 0, 10),
		sizingSession("clean2", 0, 20),
		sizingSession("clean3", 0, 30),
		sizingSession("light", 1, 100), // compacted, but not heavy
		sizingSession("heavy", 4, 200),
		sizingSession("heaviest", 4, 400), // same compactions, more cost -> first
	}

	res := sizingFrom(sessions, 20)

	if res.Scanned != 6 {
		t.Errorf("scanned = %d, want every session in the window (6)", res.Scanned)
	}
	// Only sessions that compacted are listed; the clean ones are the baseline.
	if res.TotalSessions != 3 || len(res.Sessions) != 3 {
		t.Fatalf("sessions = %+v, want the 3 that compacted", res.Sessions)
	}
	if res.Sessions[0].ID != "heaviest" || res.Sessions[1].ID != "heavy" {
		t.Errorf("order = %s, %s — want cost to break the 4-compaction tie",
			res.Sessions[0].ID, res.Sessions[1].ID)
	}
	// Medians, not means: 10/20/30 -> 20 (a mean would say the same here, but
	// 200/400 -> 300 while a mean of the heavy set is also 300; the guard is
	// that "light" belongs to NEITHER bucket).
	if res.CleanCount != 3 || res.MedianCostClean != 20 {
		t.Errorf("clean = %d sessions, median %v; want 3 / 20", res.CleanCount, res.MedianCostClean)
	}
	if res.HeavyCount != 2 || res.MedianCostHeavy != 300 {
		t.Errorf("heavy = %d sessions, median %v; want 2 / 300 (only >= %d compactions)",
			res.HeavyCount, res.MedianCostHeavy, heavyCompactions)
	}
	if res.HeavyThreshold != heavyCompactions {
		t.Errorf("heavyThreshold = %d, want %d — the UI prints it", res.HeavyThreshold, heavyCompactions)
	}
}

func TestMedianOf(t *testing.T) {
	if got := medianOf(nil); got != 0 {
		t.Errorf("medianOf(nil) = %v, want 0", got)
	}
	if got := medianOf([]float64{5}); got != 5 {
		t.Errorf("single = %v, want 5", got)
	}
	if got := medianOf([]float64{3, 1, 2}); got != 2 {
		t.Errorf("odd = %v, want 2 (must sort first)", got)
	}
	if got := medianOf([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("even = %v, want 2.5", got)
	}
	// The caller's slice must survive: sizingFrom passes ones it still reads.
	v := []float64{3, 1, 2}
	medianOf(v)
	if v[0] != 3 {
		t.Errorf("input was sorted in place: %v", v)
	}
}

func TestSizingFromNoCompactions(t *testing.T) {
	res := sizingFrom([]*Session{sizingSession("a", 0, 5)}, 20)
	if res.TotalSessions != 0 || len(res.Sessions) != 0 {
		t.Errorf("nothing compacted, so nothing to list: %+v", res.Sessions)
	}
	if res.MedianCostHeavy != 0 || res.HeavyCount != 0 {
		t.Errorf("no heavy sessions -> no heavy median, got %v (n=%d)", res.MedianCostHeavy, res.HeavyCount)
	}
	if res.MedianCostClean != 5 {
		t.Errorf("clean median = %v, want 5", res.MedianCostClean)
	}
}

func ledgerSession(project string, endedAt time.Time, cost float64, added, removed int, ms ...Milestone) *Session {
	return &Session{
		ID: "s", Title: "s", Project: project, EndedAt: endedAt, CostUSD: cost,
		LinesAdded: added, LinesRemoved: removed, Milestones: ms,
	}
}

func TestLedgerFrom(t *testing.T) {
	// Two ISO weeks, local time: Mon 2026-07-06 (W28) and Mon 2026-07-13 (W29).
	w28 := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
	w29 := time.Date(2026, 7, 15, 12, 0, 0, 0, time.Local)
	pr := func(url string, ts time.Time) Milestone { return Milestone{Kind: "pr", URL: url, Ts: ts} }

	res := ledgerFrom([]*Session{
		ledgerSession("a", w28, 100, 400, 100, pr("u/1", w28), pr("u/2", w28)),
		ledgerSession("a", w29, 50, 100, 0, pr("u/3", w29)),
	})

	if len(res.Weeks) != 2 {
		t.Fatalf("weeks = %+v, want 2", res.Weeks)
	}
	// Newest first.
	if res.Weeks[0].Week != "2026-W29" || res.Weeks[1].Week != "2026-W28" {
		t.Fatalf("order = %s, %s — want newest first", res.Weeks[0].Week, res.Weeks[1].Week)
	}
	if got := res.Weeks[1]; got.PRs != 2 || got.CostUSD != 100 || got.CostPerPR != 50 {
		t.Errorf("W28 = %+v, want 2 PRs / $100 / $50 per PR", got)
	}
	// $/1k lines: $100 over 500 lines moved -> $200.
	if got := res.Weeks[1].CostPer1kLines; got != 200 {
		t.Errorf("W28 $/1k lines = %v, want 200", got)
	}
	if res.Total.PRs != 3 || res.Total.CostUSD != 150 || res.Total.Sessions != 2 {
		t.Errorf("total = %+v, want 3 PRs / $150 / 2 sessions", res.Total)
	}
}

func TestLedgerCountsEachOutcomeOnce(t *testing.T) {
	w28 := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
	w29 := time.Date(2026, 7, 15, 12, 0, 0, 0, time.Local)

	// The same PR touched again a week later must not count twice — 42 of the
	// real corpus's 1068 PRs do exactly this, and double-counting them inflates
	// the denominator that makes $/PR look good.
	res := ledgerFrom([]*Session{
		ledgerSession("a", w28, 100, 0, 0, Milestone{Kind: "pr", URL: "u/1", Ts: w28}),
		ledgerSession("a", w29, 100, 0, 0, Milestone{Kind: "pr", URL: "u/1", Ts: w29}),
	})
	if res.Total.PRs != 1 {
		t.Errorf("total PRs = %d, want 1 — the same PR seen twice is one PR", res.Total.PRs)
	}
	byWeek := map[string]int{}
	for _, w := range res.Weeks {
		byWeek[w.Week] = w.PRs
	}
	if byWeek["2026-W28"] != 1 || byWeek["2026-W29"] != 0 {
		t.Errorf("PRs by week = %v, want it counted in the week it first appeared", byWeek)
	}

	// Releases repeat across repos: v1.0.0 in two projects is two releases.
	res = ledgerFrom([]*Session{
		ledgerSession("a", w28, 10, 0, 0, Milestone{Kind: "release", Label: "v1.0.0", Ts: w28}),
		ledgerSession("b", w28, 10, 0, 0, Milestone{Kind: "release", Label: "v1.0.0", Ts: w28}),
		ledgerSession("a", w28, 10, 0, 0, Milestone{Kind: "release", Label: "v1.0.0", Ts: w28}), // dup
	})
	if res.Total.Releases != 2 {
		t.Errorf("releases = %d, want 2 — same tag, different projects", res.Total.Releases)
	}
}

func TestLedgerSkipsSessionsWithNoEnd(t *testing.T) {
	// A zero EndedAt would bucket into a phantom week dated year zero.
	res := ledgerFrom([]*Session{
		ledgerSession("a", time.Time{}, 5, 10, 0),
		ledgerSession("a", time.Date(2026, 7, 15, 12, 0, 0, 0, time.Local), 5, 10, 0),
	})
	if len(res.Weeks) != 1 || res.Weeks[0].Week != "2026-W29" {
		t.Fatalf("weeks = %+v, want only the real one", res.Weeks)
	}
	if res.Total.Sessions != 1 || res.Total.CostUSD != 5 {
		t.Errorf("total = %+v, want the undated session left out entirely", res.Total)
	}
}

func TestPerOutcome(t *testing.T) {
	if got := perOutcome(100, 0); got != 0 {
		t.Errorf("dividing by no outcomes = %v, want 0 (not Inf/NaN)", got)
	}
	if got := perOutcome(100, 4); got != 25 {
		t.Errorf("perOutcome(100,4) = %v, want 25", got)
	}
}

func TestChurnFromEmpty(t *testing.T) {
	res := churnFrom(nil, 2, 50)
	if res.TotalFiles != 0 || len(res.Files) != 0 {
		t.Errorf("empty index should churn nothing, got %+v", res)
	}
}

func TestSessionsMultiProjectFilter(t *testing.T) {
	ix := New("")
	now := time.Now()
	for id, proj := range map[string]string{"sa": "repo-a", "sb": "repo-b", "sc": "repo-c"} {
		ix.sessions[id] = &Session{ID: id, Project: proj, EndedAt: now}
	}

	if got := ix.Sessions(0, "", ""); len(got) != 3 {
		t.Errorf("unfiltered: %d sessions, want 3", len(got))
	}
	if got := ix.Sessions(0, "repo-a", ""); len(got) != 1 || got[0].Project != "repo-a" {
		t.Errorf("single project: %+v, want just repo-a", got)
	}
	// A group scope sends several comma-separated names — any member matches.
	if got := ix.Sessions(0, "repo-a,repo-c", ""); len(got) != 2 {
		t.Errorf("multi project: %d sessions, want 2", len(got))
	}
	// Whitespace around names (and empty entries) must not break matching.
	if got := ix.Sessions(0, " repo-b , ,repo-c ", ""); len(got) != 2 {
		t.Errorf("padded multi project: %d sessions, want 2", len(got))
	}
	if got := ix.Sessions(0, "repo-x", ""); len(got) != 0 {
		t.Errorf("unknown project leaked %d sessions", len(got))
	}
}
