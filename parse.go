package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Pricing in USD per 1M tokens, keyed by model-family substring;
// adjust as model prices change.
type rateTable struct{ in, out, cw5, cw1h, cr float64 }

var pricing = map[string]rateTable{
	"opus":   {15.0, 75.0, 18.75, 30.0, 1.50},
	"sonnet": {3.0, 15.0, 3.75, 6.0, 0.30},
	"haiku":  {1.0, 5.0, 1.25, 2.0, 0.10},
	"fable":  {3.0, 15.0, 3.75, 6.0, 0.30},
}

var modelFamilies = []string{"opus", "sonnet", "haiku", "fable"}

func modelFamily(model string) string {
	m := strings.ToLower(model)
	for _, f := range modelFamilies {
		if strings.Contains(m, f) {
			return f
		}
	}
	return "other"
}

func bucketCost(b TokenBuckets, family string) float64 {
	r, ok := pricing[family]
	if !ok {
		return 0
	}
	return (float64(b.Input)*r.in + float64(b.Output)*r.out +
		float64(b.CacheWrite5m)*r.cw5 + float64(b.CacheWrite1h)*r.cw1h +
		float64(b.CacheRead)*r.cr) / 1e6
}

// splitLines splits text into lines, ignoring a single trailing newline so a
// normal file body isn't credited an extra empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// lineDiff approximates changed lines between two versions as a multiset
// difference: lines present in both (context) cancel out, so only genuinely
// changed lines are counted regardless of order.
func lineDiff(oldStr, newStr string) (added, removed int) {
	freq := map[string]int{}
	for _, l := range splitLines(oldStr) {
		freq[l]++
	}
	for _, l := range splitLines(newStr) {
		if freq[l] > 0 {
			freq[l]--
		} else {
			added++
		}
	}
	for _, n := range freq {
		if n > 0 {
			removed += n
		}
	}
	return added, removed
}

// editLineDelta counts added/removed lines for one Edit/Write/MultiEdit tool use.
func editLineDelta(c *contentJSON) (added, removed int) {
	in := c.Input
	if in == nil {
		return 0, 0
	}
	switch c.Name {
	case "Write":
		return len(splitLines(in.Content)), 0
	case "Edit":
		return lineDiff(in.OldString, in.NewString)
	case "MultiEdit":
		for _, e := range in.Edits {
			a, r := lineDiff(e.OldString, e.NewString)
			added += a
			removed += r
		}
	}
	return added, removed
}

// patchLineDelta counts added ('+') and removed ('-') lines across a tool
// result's structuredPatch hunks.
func patchLineDelta(hunks []patchHunk) (added, removed int) {
	for _, h := range hunks {
		for _, l := range h.Lines {
			if l == "" {
				continue
			}
			switch l[0] {
			case '+':
				added++
			case '-':
				removed++
			}
		}
	}
	return added, removed
}

func parseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// projectName derives a project label from the cwd: basename, with
// .claude/worktrees/<name> collapsed back to the parent repo name.
func projectName(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	p := filepath.ToSlash(cwd)
	if i := strings.Index(p, "/.claude/worktrees/"); i >= 0 {
		p = p[:i]
	}
	base := filepath.Base(p)
	if base == "" || base == "." || base == "/" {
		return "unknown"
	}
	return base
}

// spawnInfo is one Agent/Task tool_use block found in a transcript.
type spawnInfo struct {
	description  string
	subagentType string
	ts           time.Time
}

// toolEvent is one tool use in file (chronological) order: its display name
// (MCP grouped by server), tool-use id, and time. Feeds both the timeline
// activity strip and the action-flow spine.
type toolEvent struct {
	name   string
	toolID string
	ts     time.Time
}

// fileScan is the result of scanning one transcript file (main session or
// one subagent file) — the shared per-line extraction.
type fileScan struct {
	buckets      map[string]*TokenBuckets // per model family
	messageCount int
	firstTS      time.Time
	lastTS       time.Time
	slug         string
	title        string
	gitBranch    string
	cwd          string
	prURL        string
	compactCount int
	linesAdded   int // main-thread edits, summed from structuredPatch hunks
	linesRemoved int
	// editLinesAdded/Removed: approximated from Edit/Write/MultiEdit tool inputs
	// via a line-multiset diff; used for subagents, which lack structuredPatch.
	editLinesAdded   int
	editLinesRemoved int
	editedFiles      map[string]*fileEdit // file path -> its edit footprint
	rawModels        map[string]int64     // raw model string -> total tokens
	lastCtx          int64                // prompt size of the newest usage line
	lastCtxTS        time.Time
	lastFam          string
	spawns           map[string]spawnInfo   // toolUseID -> spawn block
	rollups          map[string]*rollupJSON // agentID -> rollup (parent-side enrichment)
	// This file's own tool usage (deduped by tool_use id): coarse category
	// counts, per-tool counts (MCP grouped by server), and per-use events.
	coarse     ToolStats
	toolCounts map[string]int
	toolEvents []toolEvent // ordered tool uses, for the activity strip + action-flow spine
	milestones []Milestone // semantic checkpoints mined from this file's tool calls
	// Friction: when the user interrupted (times only — see noteFriction) and
	// which tools they refused permission for.
	interrupts []time.Time
	denials    map[string]int // tool display name -> times denied
	texts      []textTuple    // user/assistant text blocks, for the search index
}

// textTuple is one searchable message text block. Exported fields: these ride
// inside persistReq to the index cache, and gob would skip lowercase ones.
type textTuple struct {
	Ts   time.Time
	Role string
	Text string
}

// Friction markers, matched against the raw line before anything is decoded.
// Only a few hundred lines in a whole corpus contain either, so the scan stays
// a memchr on the hot path and the narrow re-decode below runs for candidates
// only — which is also what keeps the promise that message text never enters
// the index: the scanner reads a line's text solely to confirm a marker.
//
// The structural confirmation is not optional. A transcript that merely
// *mentions* these strings — a grep for them, a session discussing this very
// feature — contains them verbatim, so counting raw hits counts the session
// that ran the grep. Both marker counts were wrong exactly this way before
// being confirmed structurally (see docs/plan.md, section D).
var (
	interruptMarker = []byte("[Request interrupted by user")
	denialMarker    = []byte("The user doesn't want to proceed with this tool use")
)

// frictionLine is the narrow re-decode of a marker candidate: the only shape in
// this scanner that reaches into message text or a tool result's payload.
type frictionLine struct {
	Type    string `json:"type"`
	Message *struct {
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"` // tool_result: string or block list
		} `json:"content"`
	} `json:"message"`
}

// resultText flattens a tool_result's polymorphic content (a bare string, or a
// list of blocks) to the text it carries. Empty when it's neither.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// noteFriction records a confirmed interrupt or permission denial from a
// candidate line. Interrupts carry only their time: which tool was running is
// NOT recoverable — across 295 real interrupts, none directly followed a
// tool_use line and 78% sat 4+ lines from the nearest one, so attributing them
// to a tool would be a guess wearing a chart's clothing. Denials do carry it,
// via the tool_result's tool_use_id.
func (fs *fileScan) noteFriction(raw []byte, ts time.Time, hasTS bool) {
	var fl frictionLine
	if json.Unmarshal(raw, &fl) != nil || fl.Type != "user" || fl.Message == nil {
		return
	}
	for _, b := range fl.Message.Content {
		switch b.Type {
		case "text":
			if hasTS && strings.HasPrefix(b.Text, string(interruptMarker)) {
				fs.interrupts = append(fs.interrupts, ts)
			}
		case "tool_result":
			if strings.HasPrefix(strings.TrimSpace(resultText(b.Content)), string(denialMarker)) {
				fs.denials[fs.toolNameByID(b.ToolUseID)]++
			}
		}
	}
}

// toolNameByID resolves a tool_use id to the display name this scan recorded
// for it; "unknown" when the call isn't in this file (it always should be).
func (fs *fileScan) toolNameByID(id string) string {
	for i := len(fs.toolEvents) - 1; i >= 0; i-- {
		if fs.toolEvents[i].toolID == id {
			return fs.toolEvents[i].name
		}
	}
	return "unknown"
}

// fileEdit accumulates one path's edits within a single transcript. Lines are
// kept twice for the same reason the scan totals are: the main thread has exact
// +/- from each edit result's structuredPatch, while a subagent transcript
// carries none and can only be approximated from its tool inputs. parseSession
// picks per context, so per-file lines always sum to the session's own totals.
type fileEdit struct {
	edits        int
	patchAdded   int
	patchRemoved int
	editAdded    int
	editRemoved  int
}

// file returns path's footprint, creating it on first touch.
func (fs *fileScan) file(path string) *fileEdit {
	fe := fs.editedFiles[path]
	if fe == nil {
		fe = &fileEdit{}
		fs.editedFiles[path] = fe
	}
	return fe
}

// toolDisplayName groups MCP tools under their server (mcp__<server>__<tool> ->
// <server>) so the breakdown isn't drowned in long tool names; plain tools pass
// through unchanged.
func toolDisplayName(name string) string {
	if strings.HasPrefix(name, "mcp__") {
		if p := strings.SplitN(name, "__", 3); len(p) >= 2 && p[1] != "" {
			return p[1]
		}
	}
	return name
}

// countTool records one tool use into both the per-tool map and the coarse
// ToolStats categories (matching the subagent rollup's buckets).
func (fs *fileScan) countTool(name string) {
	fs.toolCounts[toolDisplayName(name)]++
	switch name {
	case "Read":
		fs.coarse.ReadCount++
	case "Grep", "Glob":
		fs.coarse.SearchCount++
	case "Bash":
		fs.coarse.BashCount++
	case "Edit", "Write", "MultiEdit":
		fs.coarse.EditFileCount++
	default:
		fs.coarse.OtherToolCount++
	}
}

func (fs *fileScan) totals() TokenBuckets {
	var t TokenBuckets
	for _, b := range fs.buckets {
		t.add(*b)
	}
	return t
}

func (fs *fileScan) cost() float64 {
	var c float64
	for fam, b := range fs.buckets {
		c += bucketCost(*b, fam)
	}
	return c
}

// topRawModel returns the full model string with the most tokens.
func (fs *fileScan) topRawModel() string {
	var best string
	var bestTok int64 = -1
	for m, tok := range fs.rawModels {
		if tok > bestTok {
			best, bestTok = m, tok
		}
	}
	return best
}

// contextWindow guesses the window for a family; only fable runs at 1M here.
func contextWindow(family string) int64 {
	if family == "fable" {
		return 1_000_000
	}
	return 200_000
}

// familiesByUse returns the model families seen, most tokens first.
func (fs *fileScan) familiesByUse() []string {
	fams := make([]string, 0, len(fs.buckets))
	for f := range fs.buckets {
		fams = append(fams, f)
	}
	sort.Slice(fams, func(i, j int) bool {
		return fs.buckets[fams[i]].Total() > fs.buckets[fams[j]].Total()
	})
	return fams
}

// maxLine caps a single transcript line; lines embedding base64 images run
// to several MB, so allow plenty. A line over the cap is SKIPPED (and
// counted), never fatal — see lineReader.
const maxLine = 64 << 20

// lineReader yields one transcript line at a time, discarding — and counting —
// lines longer than max instead of aborting the scan the way bufio.Scanner's
// ErrTooLong does. The abort was disproportionate: one pathological line (a
// giant pasted blob) made the whole session vanish from the index while its
// transcript sat right there on disk.
type lineReader struct {
	r       *bufio.Reader
	buf     []byte
	max     int
	skipped int
}

func newLineReader(f *os.File, max int) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(f, 1<<20), buf: make([]byte, 0, 1<<20), max: max}
}

// next returns the next line without its trailing newline, io.EOF at the end.
// Oversized lines are consumed and counted internally; callers never see them.
// A final unterminated line (a transcript mid-append) is returned as a line,
// matching the old Scanner behavior — its JSON simply fails to decode.
func (lr *lineReader) next() ([]byte, error) {
	for {
		lr.buf = lr.buf[:0]
		tooLong := false
		for {
			chunk, err := lr.r.ReadSlice('\n')
			if !tooLong {
				if len(lr.buf)+len(chunk) > lr.max {
					tooLong = true
				} else {
					lr.buf = append(lr.buf, chunk...)
				}
			}
			if err == bufio.ErrBufferFull {
				continue // the same line keeps going in the next chunk
			}
			if err != nil && err != io.EOF {
				return nil, err
			}
			if tooLong {
				lr.skipped++
				if err == io.EOF {
					return nil, io.EOF
				}
				break // this line is discarded; move on to the next one
			}
			line := bytes.TrimSuffix(lr.buf, []byte("\n"))
			line = bytes.TrimSuffix(line, []byte("\r"))
			if err == io.EOF && len(line) == 0 {
				return nil, io.EOF
			}
			return line, nil
		}
	}
}

// scanFile does one pass over a transcript JSONL file. Usage is deduped by
// (message.id, requestId) — Claude Code re-logs the same API response many
// times during long turns — falling back to the line uuid; `<synthetic>`
// messages are skipped.
func scanFile(path string) (*fileScan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fs := &fileScan{
		buckets:     map[string]*TokenBuckets{},
		spawns:      map[string]spawnInfo{},
		rollups:     map[string]*rollupJSON{},
		rawModels:   map[string]int64{},
		editedFiles: map[string]*fileEdit{},
		toolCounts:  map[string]int{},
		denials:     map[string]int{},
	}
	seen := map[string]bool{}
	seenPatch := map[string]bool{}    // dedup re-logged edit results by line uuid
	seenFriction := map[string]bool{} // dedup re-logged interrupt/denial lines by line uuid
	countedTool := map[string]bool{}  // dedup re-logged tool_use blocks by tool id
	seenText := map[string]bool{}     // dedup re-logged text lines by line uuid

	lr := newLineReader(f, maxLine)
	for {
		raw, rerr := lr.next()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
		if len(raw) < 2 || raw[0] != '{' {
			continue
		}
		var ln lineJSON
		if err := json.Unmarshal(raw, &ln); err != nil {
			continue // tolerate truncated/foreign lines
		}
		ts, hasTS := parseTS(ln.Timestamp)
		if hasTS {
			if fs.firstTS.IsZero() || ts.Before(fs.firstTS) {
				fs.firstTS = ts
			}
			if ts.After(fs.lastTS) {
				fs.lastTS = ts
			}
		}
		if fs.slug == "" && ln.Slug != "" {
			fs.slug = ln.Slug
		}
		if fs.cwd == "" && ln.CWD != "" {
			fs.cwd = ln.CWD
		}
		if ln.GitBranch != "" {
			fs.gitBranch = ln.GitBranch
		}

		switch ln.Type {
		case "custom-title":
			if ln.CustomTitle != "" {
				fs.title = ln.CustomTitle
			}
			continue
		case "pr-link":
			if ln.PRURL != "" {
				fs.prURL = ln.PRURL
				fs.milestones = append(fs.milestones, Milestone{
					Kind: "pr", Label: prNumber(ln.PRURL), URL: ln.PRURL, Ts: ts,
				})
			}
			continue
		case "system":
			if ln.Subtype == "compact_boundary" {
				fs.compactCount++
			}
			continue
		}

		if ln.ToolUseResult != nil && ln.ToolUseResult.AgentID != "" {
			fs.rollups[ln.ToolUseResult.AgentID] = ln.ToolUseResult
		}
		// Main-thread edits (no agentId) carry a structuredPatch; sum their
		// +/- lines, skipping re-logged duplicates of the same result line.
		if tur := ln.ToolUseResult; tur != nil && tur.AgentID == "" && len(tur.StructuredPatch) > 0 {
			if ln.UUID == "" || !seenPatch[ln.UUID] {
				if ln.UUID != "" {
					seenPatch[ln.UUID] = true
				}
				a, r := patchLineDelta(tur.StructuredPatch)
				fs.linesAdded += a
				fs.linesRemoved += r
				if tur.FilePath != "" {
					fe := fs.file(tur.FilePath)
					fe.patchAdded += a
					fe.patchRemoved += r
				}
			}
		}
		// Both markers only ever land on a user line, and that type is already
		// decoded — so the byte scan never runs against the assistant lines that
		// carry the bulk of the corpus. Candidates then pay for the narrow
		// re-decode that confirms the marker structurally.
		//
		// Deduped by line uuid, like every other counter here: Claude Code
		// re-logs a line during long turns, and a real transcript repeats the
		// same interrupt three times — counting the repeats tripled the number.
		// (Lines without a uuid aren't re-logged, so they always count.)
		if ln.Type == "user" && (bytes.Contains(raw, interruptMarker) || bytes.Contains(raw, denialMarker)) {
			if ln.UUID == "" || !seenFriction[ln.UUID] {
				if ln.UUID != "" {
					seenFriction[ln.UUID] = true
				}
				fs.noteFriction(raw, ts, hasTS)
			}
		}
		if ln.Message == nil {
			continue
		}
		// Searchable text: user/assistant text blocks, deduped by line uuid like
		// every other counter here (a re-logged line would double its rows in the
		// search index). Tool inputs/results stay out on purpose — they'd bury
		// "where did we discuss X" under the contents of everything ever read.
		if (ln.Type == "user" || ln.Type == "assistant") && !seenText[ln.UUID] {
			if ln.UUID != "" {
				seenText[ln.UUID] = true
			}
			for i := range ln.Message.Content {
				if c := &ln.Message.Content[i]; c.Type == "text" && c.Text != "" {
					fs.texts = append(fs.texts, textTuple{Ts: ts, Role: ln.Type, Text: c.Text})
				}
			}
		}
		for i := range ln.Message.Content {
			c := &ln.Message.Content[i]
			if c.Type != "tool_use" || c.Input == nil {
				continue
			}
			// Assistant lines get re-logged during long turns; count each tool
			// use once by its id (empty-id blocks aren't re-logged, so count them).
			if c.ID != "" {
				if countedTool[c.ID] {
					continue
				}
				countedTool[c.ID] = true
			}
			fs.countTool(c.Name)
			if hasTS {
				fs.toolEvents = append(fs.toolEvents, toolEvent{name: toolDisplayName(c.Name), toolID: c.ID, ts: ts})
			}
			switch c.Name {
			case "Agent", "Task":
				fs.spawns[c.ID] = spawnInfo{
					description:  c.Input.Description,
					subagentType: c.Input.SubagentType,
					ts:           ts,
				}
			case "Edit", "Write", "MultiEdit":
				a, r := editLineDelta(c)
				fs.editLinesAdded += a
				fs.editLinesRemoved += r
				if c.Input.FilePath != "" {
					fe := fs.file(c.Input.FilePath)
					fe.edits++
					fe.editAdded += a
					fe.editRemoved += r
				}
			case "Bash":
				if hasTS && c.Input.Command != "" {
					fs.addBashMilestones(c.Input.Command, ts)
				}
			case "ExitPlanMode":
				if hasTS && c.Input.Plan != "" {
					fs.milestones = append(fs.milestones, Milestone{
						Kind: "plan", Label: planLabel(c.Input.Plan), Ts: ts,
					})
				}
			}
		}
		u := ln.Message.Usage
		if u == nil || ln.Message.Model == "" || ln.Message.Model == "<synthetic>" {
			continue
		}
		key := ln.Message.ID + "|" + ln.RequestID
		if ln.Message.ID == "" || ln.RequestID == "" {
			// Fall back to the line uuid. A line with no identity at all is
			// never re-logged, so it always counts (the rule every counter
			// here follows) — a shared "" key would keep only the first one.
			key = "uuid|" + ln.UUID
			if ln.UUID == "" {
				key = ""
			}
		}
		if key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}

		var b TokenBuckets
		b.Input = u.InputTokens
		b.Output = u.OutputTokens
		b.CacheRead = u.CacheReadInputTokens
		if u.CacheCreation != nil {
			b.CacheWrite5m = u.CacheCreation.Ephemeral5m
			b.CacheWrite1h = u.CacheCreation.Ephemeral1h
		} else {
			b.CacheWrite5m = u.CacheCreationInputTokens
		}
		fam := modelFamily(ln.Message.Model)
		if fs.buckets[fam] == nil {
			fs.buckets[fam] = &TokenBuckets{}
		}
		fs.buckets[fam].add(b)
		fs.rawModels[ln.Message.Model] += b.Total()
		fs.messageCount++
		if hasTS && !ts.Before(fs.lastCtxTS) {
			fs.lastCtxTS = ts
			fs.lastCtx = b.Input + b.CacheRead + b.CacheWrite5m + b.CacheWrite1h
			fs.lastFam = fam
		}
	}
	if lr.skipped > 0 {
		log.Printf("parse %s: skipped %d oversized line(s) (> %dMB)", filepath.Base(path), lr.skipped, maxLine>>20)
	}
	return fs, nil
}

// computeModelBreakdown splits a session's tokens + cost by model family,
// combining the main thread's per-family buckets with each agent's own usage.
func computeModelBreakdown(main *fileScan, agents []AgentRun) []ModelUsage {
	acc := map[string]*ModelUsage{}
	add := func(fam string, tokens int64, cost float64) {
		if fam == "" {
			fam = "other"
		}
		mu := acc[fam]
		if mu == nil {
			mu = &ModelUsage{Model: fam}
			acc[fam] = mu
		}
		mu.Tokens += tokens
		mu.CostUSD += cost
	}
	for fam, b := range main.buckets {
		add(fam, b.Total(), bucketCost(*b, fam))
	}
	for i := range agents {
		add(agents[i].Model, agents[i].TotalTokens, agents[i].CostUSD)
	}
	out := make([]ModelUsage, 0, len(acc))
	for _, mu := range acc {
		out = append(out, *mu)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tokens > out[j].Tokens })
	return out
}

// activityBuckets is the resolution of the main-thread activity strip shown on
// the timeline (tool uses bucketed evenly across the main thread's span).
const activityBuckets = 48

// mainToolStats packages the main thread's own tool usage as a ToolStats (the
// same shape subagents report), carrying its structuredPatch line deltas.
func mainToolStats(main *fileScan) *ToolStats {
	ts := main.coarse
	ts.LinesAdded = main.linesAdded
	ts.LinesRemoved = main.linesRemoved
	return &ts
}

// sortedToolCounts flattens the per-tool map into a count-descending slice.
func sortedToolCounts(m map[string]int) []ToolCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]ToolCount, 0, len(m))
	for name, n := range m {
		out = append(out, ToolCount{Name: name, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// Milestone signals mined from Bash command strings. Best-effort: the agent
// wrote these git invocations itself, so a match is a real checkpoint, but a
// failed command (e.g. nothing to commit) still reads as one — acceptable for
// a bird's-eye arc.
//
// cmdStart anchors detection to a command boundary (string start, or after a
// shell separator / subshell open) so a `git commit` mentioned *inside* an
// argument — a grep pattern, an echo — isn't mistaken for an actual commit.
const cmdStart = `(?:^|[\n;&|(])\s*`

// gitCmd matches `git` at a command boundary, tolerating value-taking global
// options (`-C <path>`, `-c <cfg>`; the path may be quoted) between it and the
// subcommand — the repo convention here is `git -C <repo> <subcommand>` rather
// than a cd prefix, so bare `git commit` never appears.
const gitCmd = cmdStart + `git(?:[ \t]+-[Cc][ \t]+(?:"[^"]*"|'[^']*'|\S+))*[ \t]+`

// Args are matched with [ \t] (never \s) so a bare list-form `git tag` on one
// line can't reach across a newline and swallow the next command's token as a
// "tag". Tag/release refs must start with a word char, excluding pipes/redirects.
var (
	reGitCommit  = regexp.MustCompile(gitCmd + `commit\b`)
	reGitBranch  = regexp.MustCompile(gitCmd + `(?:checkout[ \t]+-b|switch[ \t]+-c)[ \t]+(\S+)`)
	reGitTag     = regexp.MustCompile(gitCmd + `tag[ \t]+(?:-a[ \t]+)?["']?([\w][\w./-]*)`)
	reGhRelease  = regexp.MustCompile(cmdStart + `gh[ \t]+release[ \t]+create[ \t]+["']?([\w][\w./-]*)`)
	reCommitMsgD = regexp.MustCompile(`(?:-m|--message)[ \t]+"([^"]*)"`)
	reCommitMsgS = regexp.MustCompile(`(?:-m|--message)[ \t]+'([^']*)'`)
)

// addBashMilestones appends any branch/commit/release checkpoints found in one
// Bash command. A single command may carry more than one (e.g. checkout && commit).
func (fs *fileScan) addBashMilestones(cmd string, ts time.Time) {
	// Detect on the portion before any heredoc: a heredoc body carries *data*
	// (a commit message, a PR body, release notes) that can itself mention
	// `git commit` / `gh release create` and must not be read as a command. The
	// real invocation always precedes its heredoc argument. The commit *subject*
	// still comes from the full command (that heredoc IS the message).
	det := cmd
	if i := strings.Index(det, "<<"); i >= 0 {
		det = det[:i]
	}
	if m := reGitBranch.FindStringSubmatch(det); m != nil {
		if name := cleanRef(m[1]); name != "" {
			fs.milestones = append(fs.milestones, Milestone{Kind: "branch", Label: name, Ts: ts})
		}
	}
	if reGitCommit.MatchString(det) {
		fs.milestones = append(fs.milestones, Milestone{Kind: "commit", Label: commitSubject(cmd), Ts: ts})
	}
	if m := reGhRelease.FindStringSubmatch(det); m != nil {
		if name := cleanRef(m[1]); name != "" {
			fs.milestones = append(fs.milestones, Milestone{Kind: "release", Label: name, Ts: ts})
		}
	} else if m := reGitTag.FindStringSubmatch(det); m != nil {
		fs.milestones = append(fs.milestones, Milestone{Kind: "release", Label: m[1], Ts: ts})
	}
}

// commitSubject pulls the first line of a commit message out of a `git commit`
// command — handling both the heredoc form (`-m "$(cat <<'EOF' … EOF)"`, which
// the agent commonly uses for multi-line messages) and a plain quoted `-m`.
func commitSubject(cmd string) string {
	if s := heredocSubject(cmd); s != "" {
		return s
	}
	if m := reCommitMsgD.FindStringSubmatch(cmd); m != nil {
		if s := firstLine(m[1]); s != "" {
			return s
		}
	}
	if m := reCommitMsgS.FindStringSubmatch(cmd); m != nil {
		if s := firstLine(m[1]); s != "" {
			return s
		}
	}
	return "commit"
}

// heredocSubject returns the first non-empty body line of a `<<MARKER … MARKER`
// heredoc, or "" if there's no heredoc / the body is empty.
func heredocSubject(cmd string) string {
	i := strings.Index(cmd, "<<")
	if i < 0 {
		return ""
	}
	rest := cmd[i+2:]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return ""
	}
	marker := strings.TrimSpace(rest[:nl]) // e.g. 'EOF' or -EOF or "EOF"
	marker = strings.TrimPrefix(marker, "-")
	marker = strings.Trim(marker, `'"`)
	for _, line := range strings.Split(rest[nl+1:], "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if marker != "" && t == marker {
			return "" // reached the closing marker before any content
		}
		return t
	}
	return ""
}

// planLabel is the first meaningful line of an ExitPlanMode plan, stripped of
// markdown heading / bold markers.
func planLabel(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		t := strings.TrimSpace(line)
		t = strings.TrimLeft(t, "#")
		t = strings.TrimSpace(strings.Trim(strings.TrimSpace(t), "*"))
		if t != "" {
			return t
		}
	}
	return "plan"
}

// prNumber renders a PR URL as "#<n>" from its trailing path segment.
func prNumber(url string) string {
	if i := strings.LastIndex(url, "/"); i >= 0 && i+1 < len(url) {
		return "#" + url[i+1:]
	}
	return "PR"
}

func cleanRef(s string) string { return strings.Trim(s, `"'`) }

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// dedupMilestones sorts checkpoints by time and drops exact duplicates (a
// pr-link line can be re-logged; a tool_use is already deduped upstream).
func dedupMilestones(ms []Milestone) []Milestone {
	if len(ms) == 0 {
		return nil
	}
	sort.Slice(ms, func(i, j int) bool {
		if !ms[i].Ts.Equal(ms[j].Ts) {
			return ms[i].Ts.Before(ms[j].Ts)
		}
		return ms[i].Kind < ms[j].Kind
	})
	seen := map[string]bool{}
	out := make([]Milestone, 0, len(ms))
	for _, m := range ms {
		// A PR's link line can re-log at create + merge time; collapse by URL so
		// one PR is one node (the earliest, since we sorted by time).
		key := m.Kind + "|" + m.Label + "|" + m.URL + "|" + m.Ts.Format(time.RFC3339Nano)
		if m.Kind == "pr" && m.URL != "" {
			key = "pr|" + m.URL
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, m)
	}
	return out
}

// groupMilestones folds a time-ordered milestone list into branch-scoped work
// units: a branch milestone opens a new group, a release closes the current one
// after joining it. PRs deliberately don't close a group — a mid-branch PR
// (e.g. feature → dev, or back-to-back PRs) would split one unit of work in
// two. Milestones before the first branch form their own leading group;
// consecutive releases stay together.
func groupMilestones(ms []Milestone) []MilestoneGroup {
	if len(ms) == 0 {
		return nil
	}
	var groups []MilestoneGroup
	var cur []Milestone
	closed := false
	flush := func() {
		if len(cur) > 0 {
			groups = append(groups, MilestoneGroup{Title: groupTitle(cur), Milestones: cur})
			cur = nil
		}
	}
	for _, m := range ms {
		if m.Kind == "branch" || (closed && m.Kind != "release") {
			flush()
			closed = false
		}
		cur = append(cur, m)
		if m.Kind == "release" {
			closed = true
		}
	}
	flush()
	return groups
}

// groupTitle names a group after its ends: "branch → release" when both are
// present, else whichever exists, else the first milestone's label (a leading
// plan-only group reads as its plan).
func groupTitle(ms []Milestone) string {
	var branch, release string
	for _, m := range ms {
		if branch == "" && m.Kind == "branch" {
			branch = m.Label
		}
		if m.Kind == "release" {
			release = m.Label // last release wins: the version that closed the group
		}
	}
	switch {
	case branch != "" && release != "":
		return branch + " → " + release
	case branch != "":
		return branch
	case release != "":
		return release
	default:
		return ms[0].Label
	}
}

// flowKind maps a tool name to a coarse action-flow phase kind.
func flowKind(name string) string {
	switch name {
	case "Read", "Grep", "Glob":
		return "explore"
	case "Edit", "Write", "MultiEdit":
		return "edit"
	case "Bash":
		return "run"
	case "Agent", "Task":
		return "delegate"
	default:
		return "other"
	}
}

// addTool folds one tool use into a phase node's per-tool breakdown (MCP tools
// grouped by server, matching the tool-usage list).
func (n *FlowNode) addTool(name string) {
	dn := toolDisplayName(name)
	for i := range n.Tools {
		if n.Tools[i].Name == dn {
			n.Tools[i].Count++
			return
		}
	}
	n.Tools = append(n.Tools, ToolCount{Name: dn, Count: 1})
}

// buildFlow collapses an ordered tool-use stream into the main thread's action
// flow: consecutive uses of the same kind fold into one phase node, while each
// subagent spawn ("delegate") is its own node linked, when resolvable, to the
// spawned agent (toolUseID -> agentID) and labelled with its type.
func buildFlow(events []toolEvent, spawnedAgent, agentTypeByID map[string]string) []FlowNode {
	var nodes []FlowNode
	for _, e := range events {
		kind := flowKind(e.name)
		if kind == "delegate" {
			n := FlowNode{Kind: kind, Label: "delegate", Count: 1, StartTs: e.ts, EndTs: e.ts}
			if aid := spawnedAgent[e.toolID]; aid != "" {
				n.AgentID = aid
				if t := agentTypeByID[aid]; t != "" {
					n.Label = t
				}
			}
			nodes = append(nodes, n)
			continue
		}
		if k := len(nodes); k > 0 && nodes[k-1].Kind == kind {
			last := &nodes[k-1]
			last.Count++
			last.EndTs = e.ts
			last.addTool(e.name)
			continue
		}
		n := FlowNode{Kind: kind, Label: kind, Count: 1, StartTs: e.ts, EndTs: e.ts}
		n.addTool(e.name)
		nodes = append(nodes, n)
	}
	return nodes
}

// macroFlow collapses the fine action-flow spine into a few dominant-activity
// macro-phases, so the card reads as "explored → implemented → tested" instead
// of dozens of alternating micro-phases. Delegate nodes (subagent spawns) are
// meaningful anchors, never merged and never crossed; between them, same-kind
// phases coalesce and the least-significant phase is repeatedly absorbed into
// its larger neighbour until only a handful of phases remain. Each surviving
// node keeps its dominant kind and the summed tool counts of what it absorbed.
func macroFlow(nodes []FlowNode) []FlowNode {
	const maxPhases = 6
	segs := coalesceFlow(nodes)
	for countPhases(segs) > maxPhases {
		best := -1 // smallest absorbable non-delegate phase
		for i, n := range segs {
			if n.Kind == "delegate" || !hasPhaseNeighbor(segs, i) {
				continue
			}
			if best < 0 || n.Count < segs[best].Count {
				best = i
			}
		}
		if best < 0 {
			break // every phase is walled off by delegates
		}
		into := heavierPhaseNeighbor(segs, best)
		mergeFlowNode(&segs[into], segs[best])
		segs = append(segs[:best], segs[best+1:]...)
		segs = coalesceFlow(segs)
	}
	return segs
}

// coalesceFlow merges adjacent same-kind (non-delegate) phases into one.
func coalesceFlow(in []FlowNode) []FlowNode {
	var out []FlowNode
	for _, n := range in {
		if k := len(out); k > 0 && n.Kind != "delegate" && out[k-1].Kind == n.Kind {
			mergeFlowNode(&out[k-1], n)
			continue
		}
		out = append(out, n)
	}
	return out
}

func countPhases(segs []FlowNode) int {
	n := 0
	for _, s := range segs {
		if s.Kind != "delegate" {
			n++
		}
	}
	return n
}

// hasPhaseNeighbor reports whether an immediately adjacent node is a
// non-delegate phase (delegates are hard boundaries and can't be merged into).
func hasPhaseNeighbor(segs []FlowNode, i int) bool {
	return (i > 0 && segs[i-1].Kind != "delegate") ||
		(i < len(segs)-1 && segs[i+1].Kind != "delegate")
}

// heavierPhaseNeighbor returns the adjacent non-delegate phase with the larger
// tool count, so noise joins the dominant side (left preferred on a tie).
func heavierPhaseNeighbor(segs []FlowNode, i int) int {
	left, right := -1, -1
	if i > 0 && segs[i-1].Kind != "delegate" {
		left = i - 1
	}
	if i < len(segs)-1 && segs[i+1].Kind != "delegate" {
		right = i + 1
	}
	if left < 0 {
		return right
	}
	if right < 0 || segs[left].Count >= segs[right].Count {
		return left
	}
	return right
}

func mergeFlowNode(dst *FlowNode, src FlowNode) {
	dst.Count += src.Count
	if dst.StartTs.IsZero() || (!src.StartTs.IsZero() && src.StartTs.Before(dst.StartTs)) {
		dst.StartTs = src.StartTs
	}
	if src.EndTs.After(dst.EndTs) {
		dst.EndTs = src.EndTs
	}
	for _, t := range src.Tools {
		dst.mergeTool(t)
	}
}

func (n *FlowNode) mergeTool(t ToolCount) {
	for i := range n.Tools {
		if n.Tools[i].Name == t.Name {
			n.Tools[i].Count += t.Count
			return
		}
	}
	n.Tools = append(n.Tools, ToolCount{Name: t.Name, Count: t.Count})
}

// bucketActivity distributes tool-use events into n even buckets over
// [start, end], recording each bucket's tool count and per-tool breakdown.
// Returns nil when there's nothing to show.
func bucketActivity(events []toolEvent, start, end time.Time, n int) []ActivitySlot {
	span := end.Sub(start).Nanoseconds()
	if len(events) == 0 || span <= 0 || n <= 0 {
		return nil
	}
	out := make([]ActivitySlot, n)
	for _, e := range events {
		off := e.ts.Sub(start).Nanoseconds()
		if off < 0 {
			off = 0
		}
		idx := int(int64(n) * off / span)
		if idx >= n {
			idx = n - 1
		}
		out[idx].Count++
		if out[idx].Tools == nil {
			out[idx].Tools = map[string]int{}
		}
		out[idx].Tools[e.name]++
	}
	return out
}

// parseSession builds the full Session (with agent runs) for one main
// transcript file. sessionID is the filename stem; subagent files live in
// the sibling directory <stem>/subagents/.
// agentFiles is one subagent's pair of files: its transcript and its metadata.
type agentFiles struct{ jsonl, meta string }

// collectAgentFiles finds every subagent under a session's subagents/ tree,
// keyed by agent id, at ANY depth.
//
// The depth is the whole point. Most subagents sit directly in subagents/, but
// Workflow-spawned ones live in subagents/workflows/<runId>/ — and a
// single-level ReadDir never saw them. That was 562 transcripts across 31
// sessions, $725 and 378M tokens the app simply didn't know about, including
// sessions whose agent graph drew a lone main node while they had in fact
// spawned a hundred agents. If a future layout nests deeper still, this keeps
// working; the id encoded in the filename is what matters, not where it sits.
func collectAgentFiles(subDir string) map[string]*agentFiles {
	agents := map[string]*agentFiles{}
	at := func(id string) *agentFiles {
		af := agents[id]
		if af == nil {
			af = &agentFiles{}
			agents[id] = af
		}
		return af
	}
	// A read error on one entry shouldn't lose the rest of the tree.
	_ = filepath.WalkDir(subDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // skip what we can't read, keep walking
		}
		name := d.Name()
		if !strings.HasPrefix(name, "agent-") {
			return nil
		}
		switch {
		case strings.HasSuffix(name, ".meta.json"):
			at(strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".meta.json")).meta = path
		case strings.HasSuffix(name, ".jsonl"):
			at(strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")).jsonl = path
		}
		return nil
	})
	return agents
}

// parseSession builds one Session from its transcript tree. The second return
// is the main transcript's searchable text blocks — they ride to the index
// cache and deliberately never onto Session itself, which is an API payload.
func parseSession(mainPath string) (*Session, []textTuple, error) {
	main, err := scanFile(mainPath)
	if err != nil {
		return nil, nil, err
	}
	sessionID := strings.TrimSuffix(filepath.Base(mainPath), ".jsonl")

	s := &Session{
		ID:           sessionID,
		Project:      projectName(main.cwd),
		CWD:          main.cwd,
		Slug:         main.slug,
		Title:        main.title,
		GitBranch:    main.gitBranch,
		PRURL:        main.prURL,
		StartedAt:    main.firstTS,
		EndedAt:      main.lastTS,
		Models:       main.familiesByUse(),
		MessageCount: main.messageCount,
		CompactCount: main.compactCount,
		Tokens:       main.totals(),
		CostUSD:      main.cost(),
	}
	if s.Title == "" {
		s.Title = s.Slug
	}
	if !s.StartedAt.IsZero() && !s.EndedAt.IsZero() {
		s.DurationMs = s.EndedAt.Sub(s.StartedAt).Milliseconds()
	}
	s.TotalTokens = s.Tokens.Total()
	s.ContextTokens = main.lastCtx
	s.ContextWindow = contextWindow(main.lastFam)

	// Agent runs: one per agent-<id>.jsonl anywhere under subagents/,
	// self-measured from its own file, enriched from rollups, parented via
	// meta.toolUseId matched against the tool_use blocks of whichever file
	// spawned it.
	subDir := filepath.Join(strings.TrimSuffix(mainPath, ".jsonl"), "subagents")
	if _, err := os.Stat(subDir); err != nil {
		if os.IsNotExist(err) {
			s.LinesAdded, s.LinesRemoved = main.linesAdded, main.linesRemoved
			s.FileEdits = mergeFileEdits(main, nil)
			s.FilesChanged = len(s.FileEdits)
			s.ModelBreakdown = computeModelBreakdown(main, nil)
			s.setMainBreakdown(main)
			s.setFriction(main, nil)
			s.MainFlow = macroFlow(buildFlow(main.toolEvents, nil, nil))
			s.Milestones = dedupMilestones(main.milestones)
			s.MilestoneGroups = groupMilestones(s.Milestones)
			return s, main.texts, nil // session without subagents
		}
		return nil, nil, err
	}

	agents := collectAgentFiles(subDir)

	// toolUseID -> owning agentID ("" = main session), for parent resolution.
	spawnOwner := map[string]string{}
	spawnAt := map[string]spawnInfo{}
	for id, si := range main.spawns {
		spawnOwner[id] = ""
		spawnAt[id] = si
	}
	rollups := map[string]*rollupJSON{}
	for id, r := range main.rollups {
		rollups[id] = r
	}

	scans := map[string]*fileScan{}
	metas := map[string]*agentMetaJSON{}
	for id, af := range agents {
		if af.jsonl != "" {
			sub, err := scanFile(af.jsonl)
			if err != nil {
				// The agent still gets its AgentRun (from meta/rollup), but its
				// self-measured numbers are gone — say so instead of silently
				// dropping tokens and cost from the session totals.
				log.Printf("parse subagent %s: %v", filepath.Base(af.jsonl), err)
			} else {
				scans[id] = sub
				for tid, si := range sub.spawns {
					spawnOwner[tid] = id
					spawnAt[tid] = si
				}
				for rid, r := range sub.rollups {
					rollups[rid] = r
				}
			}
		}
		if af.meta != "" {
			if raw, err := os.ReadFile(af.meta); err != nil {
				log.Printf("parse subagent meta %s: %v", filepath.Base(af.meta), err)
			} else {
				var m agentMetaJSON
				if err := json.Unmarshal(raw, &m); err != nil {
					log.Printf("parse subagent meta %s: %v", filepath.Base(af.meta), err)
				} else {
					metas[id] = &m
				}
			}
		}
	}

	for id := range agents {
		run := AgentRun{ID: id, SessionID: sessionID, Status: "completed"}
		meta := metas[id]
		if meta != nil {
			run.AgentType = meta.AgentType
			run.Description = meta.Description
			run.SpawnDepth = meta.SpawnDepth
			if owner, ok := spawnOwner[meta.ToolUseID]; ok {
				run.ParentAgentID = owner
				if si := spawnAt[meta.ToolUseID]; run.Description == "" {
					run.Description = si.description
				}
			}
		}
		if sub := scans[id]; sub != nil {
			run.Tokens = sub.totals()
			run.CostUSD = sub.cost()
			run.StartedAt = sub.firstTS
			run.EndedAt = sub.lastTS
			run.MessageCount = sub.messageCount
			run.ModelID = sub.topRawModel()
			run.LinesAdded = sub.editLinesAdded
			run.LinesRemoved = sub.editLinesRemoved
			run.FilesChanged = len(sub.editedFiles)
			run.Tools = sortedToolCounts(sub.toolCounts)
			run.ToolEvents, run.ToolEventsDropped = boundedToolEvents(sub.toolEvents)
			if !sub.firstTS.IsZero() && !sub.lastTS.IsZero() {
				run.DurationMs = sub.lastTS.Sub(sub.firstTS).Milliseconds()
			}
			if fams := sub.familiesByUse(); len(fams) > 0 {
				run.Model = fams[0]
			}
		}
		run.TotalTokens = run.Tokens.Total()
		if r := rollups[id]; r != nil {
			run.Status = r.Status
			run.ToolStats = r.ToolStats
			run.ToolUseCount = r.TotalToolUse
			if r.ResolvedModel != "" {
				run.Model = modelFamily(r.ResolvedModel)
				run.ModelID = r.ResolvedModel
			}
			if r.TotalDurationMs > 0 {
				run.DurationMs = r.TotalDurationMs
			}
			if run.TotalTokens == 0 && r.TotalTokens > 0 {
				run.TotalTokens = r.TotalTokens
			}
		}
		s.Agents = append(s.Agents, run)
		s.AgentTokens += run.TotalTokens
		s.AgentCostUSD += run.CostUSD
	}
	sort.Slice(s.Agents, func(i, j int) bool {
		return s.Agents[i].StartedAt.Before(s.Agents[j].StartedAt)
	})
	s.AgentCount = len(s.Agents)

	// A running subagent can outlive the parent transcript's last write.
	for i := range s.Agents {
		if s.Agents[i].EndedAt.After(s.EndedAt) {
			s.EndedAt = s.Agents[i].EndedAt
			s.DurationMs = s.EndedAt.Sub(s.StartedAt).Milliseconds()
		}
	}

	// Session total = main thread (structuredPatch) + each subagent's own edits
	// (approximated from their tool inputs).
	s.LinesAdded, s.LinesRemoved = main.linesAdded, main.linesRemoved
	for i := range s.Agents {
		s.LinesAdded += s.Agents[i].LinesAdded
		s.LinesRemoved += s.Agents[i].LinesRemoved
	}
	// Distinct files across the main thread and every subagent.
	s.FileEdits = mergeFileEdits(main, scans)
	s.FilesChanged = len(s.FileEdits)
	s.ModelBreakdown = computeModelBreakdown(main, s.Agents)
	s.setMainBreakdown(main)
	s.setFriction(main, scans)
	// Action flow of the main thread: delegate nodes resolve to the subagent
	// they spawned (meta.toolUseId -> agentID) and carry its type as the label.
	spawnedAgent := map[string]string{}
	for id, m := range metas {
		if m.ToolUseID != "" {
			spawnedAgent[m.ToolUseID] = id
		}
	}
	agentTypeByID := make(map[string]string, len(s.Agents))
	for i := range s.Agents {
		agentTypeByID[s.Agents[i].ID] = s.Agents[i].AgentType
	}
	s.MainFlow = macroFlow(buildFlow(main.toolEvents, spawnedAgent, agentTypeByID))
	// Milestones: the semantic arc, merged from the main thread + every subagent
	// (delegated commits/PRs live in the subagent transcripts), time-ordered.
	ms := append([]Milestone(nil), main.milestones...)
	for _, sub := range scans {
		ms = append(ms, sub.milestones...)
	}
	s.Milestones = dedupMilestones(ms)
	s.MilestoneGroups = groupMilestones(s.Milestones)
	return s, main.texts, nil
}

// maxToolEvents bounds each timeline lane's per-tool payload; a long session
// can hold thousands of tool calls, so histories over the cap are downsampled
// evenly — the visual distribution survives, the byte count doesn't explode.
const maxToolEvents = 400

// boundedToolEvents converts the scanner's ordered tool uses into the public
// timeline events, keeping every step-th one when over the cap. The second
// return is how many events the downsampling skipped.
func boundedToolEvents(events []toolEvent) ([]ToolEvent, int) {
	if len(events) == 0 {
		return nil, 0
	}
	step := 1
	if len(events) > maxToolEvents {
		step = (len(events) + maxToolEvents - 1) / maxToolEvents
	}
	out := make([]ToolEvent, 0, (len(events)+step-1)/step)
	for i := 0; i < len(events); i += step {
		out = append(out, ToolEvent{Name: events[i].name, Ts: events[i].ts})
	}
	return out, len(events) - len(out)
}

// mergeFileEdits folds every transcript's per-file footprint into one list,
// path-keyed and path-sorted. It mirrors how the session's line totals are
// built: the main thread's lines come from its edit results' structuredPatch
// (exact), each subagent's from its own tool inputs (approximated) — so the
// per-file numbers add up to Session.LinesAdded/Removed rather than drifting
// from them. Its length is the distinct-file count.
func mergeFileEdits(main *fileScan, subs map[string]*fileScan) []FileEdit {
	acc := map[string]*FileEdit{}
	get := func(path string) *FileEdit {
		fe := acc[path]
		if fe == nil {
			fe = &FileEdit{Path: path}
			acc[path] = fe
		}
		return fe
	}
	for path, fe := range main.editedFiles {
		d := get(path)
		d.Edits += fe.edits
		d.LinesAdded += fe.patchAdded
		d.LinesRemoved += fe.patchRemoved
	}
	for _, sub := range subs {
		for path, fe := range sub.editedFiles {
			d := get(path)
			d.Edits += fe.edits
			d.LinesAdded += fe.editAdded
			d.LinesRemoved += fe.editRemoved
		}
	}
	out := make([]FileEdit, 0, len(acc))
	for _, fe := range acc {
		out = append(out, *fe)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// setFriction attaches when the user stopped this session and which tools they
// refused. Both merge across every transcript: ESC hit while a subagent holds
// the turn lands the marker in that agent's file (17 of this corpus's 295), and
// it is still the user stopping the session — counting only the main thread
// would quietly undercount by 6%.
func (s *Session) setFriction(main *fileScan, subs map[string]*fileScan) {
	s.InterruptTimes = append(s.InterruptTimes, main.interrupts...)
	for _, sub := range subs {
		s.InterruptTimes = append(s.InterruptTimes, sub.interrupts...)
	}
	sort.Slice(s.InterruptTimes, func(i, j int) bool { return s.InterruptTimes[i].Before(s.InterruptTimes[j]) })
	s.Interrupts = len(s.InterruptTimes)

	denials := make(map[string]int, len(main.denials))
	for tool, n := range main.denials {
		denials[tool] += n
	}
	for _, sub := range subs {
		for tool, n := range sub.denials {
			denials[tool] += n
		}
	}
	s.DenialTools = sortedToolCounts(denials)
	for _, tc := range s.DenialTools {
		s.Denials += tc.Count
	}
}

// setMainBreakdown attaches the main thread's own tool usage, file count, and
// activity strip so the detail view can show the main agent as a first-class
// row/card/timeline. Bucketed over the main thread's own span.
func (s *Session) setMainBreakdown(main *fileScan) {
	s.MainToolStats = mainToolStats(main)
	s.MainFilesChanged = len(main.editedFiles)
	s.MainTools = sortedToolCounts(main.toolCounts)
	s.MainActivity = bucketActivity(main.toolEvents, main.firstTS, main.lastTS, activityBuckets)
	s.MainToolEvents, s.MainToolEventsDropped = boundedToolEvents(main.toolEvents)
}
