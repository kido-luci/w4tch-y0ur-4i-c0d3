package index

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An oversized line must be skipped and counted, never abort the scan — the
// old bufio.Scanner ErrTooLong abort made the whole session vanish from the
// index over one pathological line.
func TestLineReaderSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.jsonl")
	huge := strings.Repeat("x", 300)
	body := "short one\n" + huge + "\nshort two\n" + huge + "\nfinal unterminated"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	lr := newLineReader(f, 64) // cap far below the huge lines
	var got []string
	for {
		line, err := lr.next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(line))
	}
	want := []string{"short one", "short two", "final unterminated"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	if lr.skipped != 2 {
		t.Fatalf("skipped = %d, want 2", lr.skipped)
	}
}

// A file whose LAST line is oversized must still end cleanly at io.EOF.
func TestLineReaderOversizedFinalLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	if err := os.WriteFile(path, []byte("keep\n"+strings.Repeat("y", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	lr := newLineReader(f, 64)
	line, err := lr.next()
	if err != nil || string(line) != "keep" {
		t.Fatalf("first line = %q, %v", line, err)
	}
	if _, err := lr.next(); err != io.EOF {
		t.Fatalf("want io.EOF after oversized tail, got %v", err)
	}
	if lr.skipped != 1 {
		t.Fatalf("skipped = %d, want 1", lr.skipped)
	}
}

func fixtureSession(t *testing.T) *Session {
	t.Helper()
	s, _, err := parseSession(filepath.Join("testdata", "proj-a",
		"11111111-aaaa-bbbb-cccc-000000000001.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseSessionMainThread(t *testing.T) {
	s := fixtureSession(t)

	if s.Title != "Fixture Session" {
		t.Errorf("title = %q, want custom title", s.Title)
	}
	if s.Slug != "test-slug" || s.Project != "proj-a" || s.GitBranch != "feat/test" {
		t.Errorf("identity = %q/%q/%q", s.Slug, s.Project, s.GitBranch)
	}
	if s.CompactCount != 1 {
		t.Errorf("compactCount = %d, want 1", s.CompactCount)
	}

	// The duplicated (msg_1, req_1) line must count once; <synthetic> never.
	// msg_1: in 100, out 50, cr 1000, cw5 150, cw1h 50 (split takes priority
	// over the flat cache_creation_input_tokens 200)
	// msg_2: in 10, out 5
	want := TokenBuckets{Input: 110, Output: 55, CacheRead: 1000, CacheWrite5m: 150, CacheWrite1h: 50}
	if s.Tokens != want {
		t.Errorf("tokens = %+v, want %+v", s.Tokens, want)
	}
	if s.MessageCount != 2 {
		t.Errorf("messageCount = %d, want 2 (dedup + synthetic skip)", s.MessageCount)
	}
	if len(s.Models) != 1 || s.Models[0] != "fable" {
		t.Errorf("models = %v, want [fable]", s.Models)
	}
	// fable rate: (110*3 + 55*15 + 150*3.75 + 50*6 + 1000*0.30)/1e6
	wantCost := (110*3.0 + 55*15.0 + 150*3.75 + 50*6.0 + 1000*0.30) / 1e6
	if diff := s.CostUSD - wantCost; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v, want %v", s.CostUSD, wantCost)
	}
	if s.StartedAt.IsZero() || !s.EndedAt.After(s.StartedAt) {
		t.Errorf("time range %v..%v invalid", s.StartedAt, s.EndedAt)
	}
}

func TestParseSessionAgentTree(t *testing.T) {
	s := fixtureSession(t)

	if s.AgentCount != 2 {
		t.Fatalf("agentCount = %d, want 2", s.AgentCount)
	}
	byID := map[string]AgentRun{}
	for _, a := range s.Agents {
		byID[a.ID] = a
	}

	c1, ok := byID["child1"]
	if !ok {
		t.Fatal("child1 missing")
	}
	if c1.ParentAgentID != "" {
		t.Errorf("child1 parent = %q, want main (empty)", c1.ParentAgentID)
	}
	if c1.AgentType != "general-purpose" || c1.Description != "Do a subtask" {
		t.Errorf("child1 meta = %q/%q", c1.AgentType, c1.Description)
	}
	if c1.Model != "sonnet" {
		t.Errorf("child1 model = %q, want sonnet (resolvedModel wins)", c1.Model)
	}
	// self-measured from its own file: 200+100+50+10 + 60+40 = 460
	if c1.TotalTokens != 460 {
		t.Errorf("child1 totalTokens = %d, want 460", c1.TotalTokens)
	}
	// rollup enrichment
	if c1.DurationMs != 120000 || c1.ToolUseCount != 7 || c1.ToolStats == nil || c1.ToolStats.ReadCount != 3 {
		t.Errorf("child1 rollup enrichment missing: %+v", c1)
	}
	if c1.Status != "completed" {
		t.Errorf("child1 status = %q", c1.Status)
	}

	c2, ok := byID["child2"]
	if !ok {
		t.Fatal("child2 missing")
	}
	if c2.ParentAgentID != "child1" {
		t.Errorf("child2 parent = %q, want child1 (nested spawn)", c2.ParentAgentID)
	}
	if c2.SpawnDepth != 2 || c2.Model != "haiku" || c2.TotalTokens != 50 {
		t.Errorf("child2 = depth %d model %q tokens %d", c2.SpawnDepth, c2.Model, c2.TotalTokens)
	}

	if s.AgentTokens != 460+50 {
		t.Errorf("agentTokens = %d, want 510", s.AgentTokens)
	}
}

func TestParseSessionMainFlow(t *testing.T) {
	s := fixtureSession(t)

	// The fixture's main thread has exactly one tool use: the Agent spawn of
	// child1 (general-purpose). It collapses to a single "delegate" phase that
	// links back to that subagent.
	if len(s.MainFlow) != 1 {
		t.Fatalf("mainFlow len = %d, want 1: %+v", len(s.MainFlow), s.MainFlow)
	}
	n := s.MainFlow[0]
	if n.Kind != "delegate" {
		t.Errorf("mainFlow[0].kind = %q, want delegate", n.Kind)
	}
	if n.AgentID != "child1" {
		t.Errorf("mainFlow[0].agentId = %q, want child1", n.AgentID)
	}
	if n.Label != "general-purpose" {
		t.Errorf("mainFlow[0].label = %q, want general-purpose", n.Label)
	}
}

func TestBuildFlowGrouping(t *testing.T) {
	// Consecutive same-kind uses fold into one phase; a kind change or a spawn
	// starts a new node; delegate nodes resolve their spawned agent + label.
	events := []toolEvent{
		{name: "Read"}, {name: "Grep"}, // explore ×2
		{name: "Edit"},                 // edit ×1
		{name: "Bash"}, {name: "Bash"}, // run ×2
		{name: "Task", toolID: "tu_1"}, // delegate -> a1
		{name: "Read"},                 // explore ×1 (new phase after delegate)
	}
	flow := buildFlow(events,
		map[string]string{"tu_1": "a1"},
		map[string]string{"a1": "Explore"})

	if got := len(flow); got != 5 {
		t.Fatalf("flow len = %d, want 5: %+v", got, flow)
	}
	if flow[0].Kind != "explore" || flow[0].Count != 2 {
		t.Errorf("flow[0] = %+v, want explore ×2", flow[0])
	}
	if flow[3].Kind != "delegate" || flow[3].AgentID != "a1" || flow[3].Label != "Explore" {
		t.Errorf("flow[3] = %+v, want delegate a1/Explore", flow[3])
	}
	if flow[4].Kind != "explore" || flow[4].Count != 1 {
		t.Errorf("flow[4] = %+v, want explore ×1 (delegate breaks the run)", flow[4])
	}
}

// editSessionFile writes a standalone main transcript (no subagents) whose main
// thread edits two files, and returns its path. Written here rather than added
// to the shared fixture: that one's main thread holds exactly one tool use, and
// TestParseSessionMainFlow reads it.
func editSessionFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj-z")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two Edits on a.go (+2/-1 then +3/-0) and one Write on b.go (+5/-0): the
	// tool_use blocks name the file, the result lines carry the exact patch.
	lines := []string{
		`{"type":"assistant","uuid":"u1","timestamp":"2026-07-17T10:00:00.000Z","cwd":"/Users/x/dev/proj-z","requestId":"req_1","message":{"id":"msg_1","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_1","name":"Edit","input":{"file_path":"/repo/a.go","old_string":"gone","new_string":"one\ntwo"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-07-17T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1"}]},"toolUseResult":{"filePath":"/repo/a.go","structuredPatch":[{"lines":[" ctx","+one","+two","-gone"]}]}}`,
		`{"type":"assistant","uuid":"u3","timestamp":"2026-07-17T10:00:02.000Z","requestId":"req_2","message":{"id":"msg_2","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_2","name":"Edit","input":{"file_path":"/repo/a.go","old_string":"","new_string":"three\nfour\nfive"}}]}}`,
		`{"type":"user","uuid":"u4","timestamp":"2026-07-17T10:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2"}]},"toolUseResult":{"filePath":"/repo/a.go","structuredPatch":[{"lines":["+three","+four","+five"]}]}}`,
		`{"type":"assistant","uuid":"u5","timestamp":"2026-07-17T10:00:04.000Z","requestId":"req_3","message":{"id":"msg_3","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_3","name":"Write","input":{"file_path":"/repo/b.go","content":"a\nb\nc\nd\ne"}}]}}`,
		`{"type":"user","uuid":"u6","timestamp":"2026-07-17T10:00:05.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_3"}]},"toolUseResult":{"filePath":"/repo/b.go","structuredPatch":[{"lines":["+a","+b","+c","+d","+e"]}]}}`,
	}
	path := filepath.Join(dir, "22222222-aaaa-bbbb-cccc-000000000002.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSessionFileEdits(t *testing.T) {
	s, _, err := parseSession(editSessionFile(t))
	if err != nil {
		t.Fatal(err)
	}

	if s.FilesChanged != 2 || len(s.FileEdits) != 2 {
		t.Fatalf("filesChanged = %d, fileEdits = %+v, want 2 files", s.FilesChanged, s.FileEdits)
	}
	// Path-sorted: a.go before b.go.
	a, b := s.FileEdits[0], s.FileEdits[1]
	if a.Path != "/repo/a.go" || b.Path != "/repo/b.go" {
		t.Fatalf("paths = %q, %q", a.Path, b.Path)
	}
	// a.go: two Edit calls, patch lines +2/-1 then +3/-0.
	if a.Edits != 2 || a.LinesAdded != 5 || a.LinesRemoved != 1 {
		t.Errorf("a.go = %+v, want 2 edits +5/-1", a)
	}
	if b.Edits != 1 || b.LinesAdded != 5 || b.LinesRemoved != 0 {
		t.Errorf("b.go = %+v, want 1 edit +5/-0", b)
	}

	// The invariant the churn view rests on: per-file lines are the session's
	// lines, redistributed — never a second, drifting measurement.
	var sumAdd, sumRem int
	for _, fe := range s.FileEdits {
		sumAdd += fe.LinesAdded
		sumRem += fe.LinesRemoved
	}
	if sumAdd != s.LinesAdded || sumRem != s.LinesRemoved {
		t.Errorf("per-file sum +%d/-%d != session +%d/-%d",
			sumAdd, sumRem, s.LinesAdded, s.LinesRemoved)
	}
}

// frictionSessionFile writes a main transcript carrying one real interrupt, one
// real permission denial, and — the point of the fixture — two lines that merely
// quote both markers. A Bash call that greps for them, and its result echoing
// what it found, are exactly how these counts get inflated: measure by raw match
// and the session that ran the grep counts itself.
func frictionSessionFile(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "proj-f")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		// A Bash call whose command text quotes both markers. Not friction.
		`{"type":"assistant","uuid":"f1","timestamp":"2026-07-17T10:00:00.000Z","cwd":"/Users/x/dev/proj-f","requestId":"r1","message":{"id":"m1","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"grep -r \"[Request interrupted by user\" . ; grep -r \"The user doesn't want to proceed with this tool use\" ."}}]}}`,
		// Its result echoes the markers back. Still not friction.
		`{"type":"user","uuid":"f2","timestamp":"2026-07-17T10:00:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"3 matches: [Request interrupted by user] and The user doesn't want to proceed with this tool use"}]},"toolUseResult":{"stdout":"3 matches"}}`,
		// A real interrupt: a user text block that starts with the marker.
		`{"type":"user","uuid":"f3","timestamp":"2026-07-17T10:05:00.000Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		// The SAME line re-logged, as Claude Code does during a long turn. A real
		// transcript repeats each interrupt three times; counting the repeats
		// tripled the number and put six marks on a two-interrupt timeline.
		`{"type":"user","uuid":"f3","timestamp":"2026-07-17T10:05:00.000Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		// A real denial. Its toolUseResult is a bare STRING — the shape that used
		// to fail the whole line's decode and drop every denial on the floor.
		`{"type":"assistant","uuid":"f4","timestamp":"2026-07-17T10:06:00.000Z","requestId":"r2","message":{"id":"m2","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_2","name":"Edit","input":{"file_path":"/repo/x.go","old_string":"a","new_string":"b"}}]}}`,
		`{"type":"user","uuid":"f5","timestamp":"2026-07-17T10:06:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","content":"The user doesn't want to proceed with this tool use. The tool use was rejected."}]},"toolUseResult":"User rejected tool use"}`,
		// ...re-logged too.
		`{"type":"user","uuid":"f5","timestamp":"2026-07-17T10:06:01.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_2","content":"The user doesn't want to proceed with this tool use. The tool use was rejected."}]},"toolUseResult":"User rejected tool use"}`,
	}
	path := filepath.Join(dir, "33333333-aaaa-bbbb-cccc-000000000003.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestParseSessionFriction(t *testing.T) {
	s, _, err := parseSession(frictionSessionFile(t))
	if err != nil {
		t.Fatal(err)
	}

	// One interrupt: the marker is in the fixture three times over — once real,
	// once re-logged, and once quoted inside a Bash command.
	if s.Interrupts != 1 {
		t.Errorf("interrupts = %d, want 1 — quoting the marker isn't an interrupt, and a re-logged line isn't a second one", s.Interrupts)
	}
	if len(s.InterruptTimes) != 1 || s.InterruptTimes[0].Minute() != 5 {
		t.Errorf("interruptTimes = %v, want the 10:05 marker only", s.InterruptTimes)
	}
	if s.Denials != 1 {
		t.Errorf("denials = %d, want 1 — quoting the marker isn't a denial, and a re-logged line isn't a second one", s.Denials)
	}
	// The denial resolves to the tool it refused, via the result's tool_use_id.
	if len(s.DenialTools) != 1 || s.DenialTools[0].Name != "Edit" || s.DenialTools[0].Count != 1 {
		t.Errorf("denialTools = %+v, want Edit ×1", s.DenialTools)
	}
}

// A bare-string toolUseResult must not take its line down with it: that line is
// the one carrying a permission rejection, and the strict object type silently
// dropped 13.5k lines of this corpus — every denial among them.
func TestRollupTolerAtesNonObjectToolUseResult(t *testing.T) {
	for _, raw := range []string{
		`{"type":"user","toolUseResult":"User rejected tool use"}`,
		`{"type":"user","toolUseResult":[{"type":"text","text":"hi"}]}`,
		`{"type":"user","toolUseResult":null}`,
	} {
		var ln lineJSON
		if err := json.Unmarshal([]byte(raw), &ln); err != nil {
			t.Errorf("line failed to decode (%s): %v", raw, err)
		}
	}
	// An object still decodes into the rollup fields it always did.
	var ln lineJSON
	if err := json.Unmarshal([]byte(`{"type":"user","toolUseResult":{"agentId":"a1","totalTokens":42}}`), &ln); err != nil {
		t.Fatal(err)
	}
	if ln.ToolUseResult == nil || ln.ToolUseResult.AgentID != "a1" || ln.ToolUseResult.TotalTokens != 42 {
		t.Errorf("object rollup broke: %+v", ln.ToolUseResult)
	}
}

func TestProjectName(t *testing.T) {
	cases := map[string]string{
		"/Users/x/dev/proj-a":                          "proj-a",
		"/Users/x/dev/repo/.claude/worktrees/wt-1":     "repo",
		"/Users/x/dev/repo/.claude/worktrees/wt-1/sub": "repo",
		"": "unknown",
	}
	for in, want := range cases {
		if got := projectName(in); got != want {
			t.Errorf("projectName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGroupMilestones(t *testing.T) {
	m := func(kind, label string) Milestone { return Milestone{Kind: kind, Label: label} }

	// The real shape: branch-scoped units, each closed by its release; PRs
	// (even back-to-back) must NOT split a unit.
	groups := groupMilestones([]Milestone{
		m("plan", "grouped milestones"),
		m("branch", "feat/a"), m("commit", "feat: x"), m("release", "v1.0.0"),
		m("branch", "fix/b"), m("commit", "fix: y"), m("pr", "#37"), m("pr", "#38"),
		m("commit", "docs: changelog"), m("release", "v1.0.1"),
		m("commit", "chore: follow-up"), // post-release, no new branch
	})
	if len(groups) != 4 {
		t.Fatalf("groups = %d, want 4: %+v", len(groups), groups)
	}
	wantTitles := []string{"grouped milestones", "feat/a → v1.0.0", "fix/b → v1.0.1", "chore: follow-up"}
	wantSizes := []int{1, 3, 6, 1}
	for i, g := range groups {
		if g.Title != wantTitles[i] {
			t.Errorf("groups[%d].Title = %q, want %q", i, g.Title, wantTitles[i])
		}
		if len(g.Milestones) != wantSizes[i] {
			t.Errorf("groups[%d] size = %d, want %d", i, len(g.Milestones), wantSizes[i])
		}
	}

	// Consecutive releases (git tag + gh release create) stay in one group.
	groups = groupMilestones([]Milestone{
		m("branch", "feat/c"), m("release", "v2.0.0"), m("release", "v2.0.0"),
	})
	if len(groups) != 1 || groups[0].Title != "feat/c → v2.0.0" {
		t.Fatalf("consecutive releases split: %+v", groups)
	}

	if groupMilestones(nil) != nil {
		t.Error("nil milestones should yield nil groups")
	}
}

func TestBoundedToolEvents(t *testing.T) {
	if out, dropped := boundedToolEvents(nil); out != nil || dropped != 0 {
		t.Fatalf("empty input should stay empty, got %v (%d)", out, dropped)
	}

	small := make([]toolEvent, 40)
	for i := range small {
		small[i] = toolEvent{name: "Read"}
	}
	if out, dropped := boundedToolEvents(small); len(out) != 40 || dropped != 0 {
		t.Fatalf("under the cap nothing drops: got %d kept, %d dropped", len(out), dropped)
	}

	big := make([]toolEvent, 1000)
	for i := range big {
		big[i] = toolEvent{name: "Bash"}
	}
	out, dropped := boundedToolEvents(big)
	if len(out) > maxToolEvents {
		t.Fatalf("cap exceeded: %d > %d", len(out), maxToolEvents)
	}
	if len(out)+dropped != 1000 {
		t.Fatalf("kept+dropped should equal input: %d+%d != 1000", len(out), dropped)
	}
}
