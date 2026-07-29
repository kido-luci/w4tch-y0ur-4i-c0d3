package index

import (
	"encoding/json"
	"time"
)

// TokenBuckets mirrors Claude Code's per-message usage breakdown:
// cache_creation.ephemeral_5m/1h split when present, falling back to the
// flat cache_creation_input_tokens (counted as 5m) on older lines.
type TokenBuckets struct {
	Input        int64 `json:"inputTokens"`
	Output       int64 `json:"outputTokens"`
	CacheRead    int64 `json:"cacheReadTokens"`
	CacheWrite5m int64 `json:"cacheWrite5mTokens"`
	CacheWrite1h int64 `json:"cacheWrite1hTokens"`
}

func (b TokenBuckets) Total() int64 {
	return b.Input + b.Output + b.CacheRead + b.CacheWrite5m + b.CacheWrite1h
}

func (b *TokenBuckets) add(o TokenBuckets) {
	b.Input += o.Input
	b.Output += o.Output
	b.CacheRead += o.CacheRead
	b.CacheWrite5m += o.CacheWrite5m
	b.CacheWrite1h += o.CacheWrite1h
}

// ToolStats is the per-agent tool-usage rollup Claude Code writes into the
// parent transcript's toolUseResult line.
type ToolStats struct {
	ReadCount      int `json:"readCount"`
	SearchCount    int `json:"searchCount"`
	BashCount      int `json:"bashCount"`
	EditFileCount  int `json:"editFileCount"`
	LinesAdded     int `json:"linesAdded"`
	LinesRemoved   int `json:"linesRemoved"`
	OtherToolCount int `json:"otherToolCount"`
}

// ModelUsage is one model family's share of a session's tokens + cost, summed
// across the main thread and every subagent.
type ModelUsage struct {
	Model   string  `json:"model"`
	Tokens  int64   `json:"tokens"`
	CostUSD float64 `json:"costUsd"`
}

// ToolCount is one tool's use count on the main thread (MCP tools grouped by
// server), for the detail view's tool-usage breakdown.
type ToolCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// ActivitySlot is one time bucket of the main thread's activity strip: how many
// tools ran in it and which (name -> count), for the timeline tooltip.
type ActivitySlot struct {
	Count int            `json:"count"`
	Tools map[string]int `json:"tools,omitempty"`
}

// ToolEvent is one tool use on the timeline: which tool ran and when — name
// and timestamp only, never inputs. Detail-only; long histories are
// downsampled evenly (see boundedToolEvents) to bound the payload.
type ToolEvent struct {
	Name string    `json:"name"`
	Ts   time.Time `json:"ts"`
}

// FlowNode is one phase in the main thread's action flow: a run of consecutive
// tool uses of the same kind, folded into a single node for a bird's-eye
// "what is this session doing" overview. Categories + counts only — never
// prompt or file content. A "delegate" node links to the subagent it spawned.
type FlowNode struct {
	Kind    string      `json:"kind"`  // explore | edit | run | delegate | other
	Label   string      `json:"label"` // the kind, or a subagent type for delegate
	Count   int         `json:"count"` // tool uses folded into this phase
	Tools   []ToolCount `json:"tools,omitempty"`
	StartTs time.Time   `json:"startTs"`
	EndTs   time.Time   `json:"endTs"`
	AgentID string      `json:"agentId,omitempty"` // delegate -> spawned subagent
}

// Milestone is one semantic checkpoint in a session's arc — a plan presented, a
// branch cut, a commit, a PR opened, a release tagged. Mined mechanically from
// the transcript's own tool calls (git commands in Bash, ExitPlanMode) and
// pr-link lines: no LLM, no message text beyond the commit subject the agent
// itself wrote. Merged across the main thread and every subagent, time-ordered,
// so a long session reads as a narrative instead of just its last chat turn.
type Milestone struct {
	Kind  string    `json:"kind"` // plan | branch | commit | pr | release
	Label string    `json:"label"`
	URL   string    `json:"url,omitempty"` // pr node -> the PR link
	Ts    time.Time `json:"ts"`
}

// MilestoneGroup is one branch-scoped unit of work: the milestones from a
// branch cut (or session start) through the release that closes it. Derived
// deterministically from the milestone list — see groupMilestones.
type MilestoneGroup struct {
	Title      string      `json:"title"` // heuristic: "branch → release" / branch / release / first label
	Milestones []Milestone `json:"milestones"`
}

// FileEdit is one file's edit footprint inside a session: how many Edit/Write
// calls landed on it and the lines they moved. The scanner already collected
// these paths to count FilesChanged; keeping them lets the churn view pivot by
// file instead of by session. Never serialized — a session payload has no use
// for the paths, and /api/churn reads them straight from the index.
type FileEdit struct {
	Path         string
	Edits        int
	LinesAdded   int
	LinesRemoved int
}

// ChurnSessionRef is one session that edited a churned file: its footprint on
// that file, plus the session's own total cost — NOT a per-file share of it.
// Splitting a session's cost across its files would invent precision the
// transcripts don't carry.
type ChurnSessionRef struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Project      string    `json:"project"`
	EndedAt      time.Time `json:"endedAt"`
	Edits        int       `json:"edits"`
	LinesAdded   int       `json:"linesAdded"`
	LinesRemoved int       `json:"linesRemoved"`
	CostUSD      float64   `json:"costUsd"`
}

// ChurnFile is one file's rework footprint across sessions — a file whose lines
// keep getting written and then unwritten is where the loop went in circles.
type ChurnFile struct {
	Path         string `json:"path"`
	Sessions     int    `json:"sessions"`
	Edits        int    `json:"edits"`
	LinesAdded   int    `json:"linesAdded"`
	LinesRemoved int    `json:"linesRemoved"`
	// ChurnedLines = min(added, removed): how much of the writing was later
	// unwritten. It ranks the radar because session count doesn't measure
	// rework — an append-only journal (a MEMORY.md, a changelog) is touched by
	// every session by design and would otherwise bury the rewritten code.
	ChurnedLines int               `json:"churnedLines"`
	LastTouched  time.Time         `json:"lastTouched"`
	Refs         []ChurnSessionRef `json:"refs"` // newest first
}

// ChurnResult is the churn view's payload: the ranked files plus how many
// passed the filter in total, so the UI can say "top 50 of 312" instead of
// silently truncating.
type ChurnResult struct {
	Files      []ChurnFile `json:"files"`
	TotalFiles int         `json:"totalFiles"`
}

// FrictionSession is one session that fought back: how often it was stopped or
// refused, next to what it cost. Ranked by interrupts — a session you kept
// hitting ESC in is a session whose prompt (or CLAUDE.md) was wrong.
type FrictionSession struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Project     string    `json:"project"`
	EndedAt     time.Time `json:"endedAt"`
	Interrupts  int       `json:"interrupts"`
	Denials     int       `json:"denials"`
	CostUSD     float64   `json:"costUsd"`
	TotalTokens int64     `json:"totalTokens"`
	DurationMs  int64     `json:"durationMs"`
}

// FrictionResult is the friction block's payload: the worst sessions plus the
// window's totals. DenialTools is the secondary stat — permission refusals are
// rare enough (17 in this corpus) to be a footnote, not a ranking.
type FrictionResult struct {
	Sessions      []FrictionSession `json:"sessions"`      // most interrupted first
	TotalSessions int               `json:"totalSessions"` // sessions with any friction in the window
	Interrupts    int               `json:"interrupts"`
	Denials       int               `json:"denials"`
	DenialTools   []ToolCount       `json:"denialTools,omitempty"`
}

// SizingSession is one session that outgrew its context: how many times it
// compacted, next to what it cost and how long it ran.
type SizingSession struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Project     string    `json:"project"`
	EndedAt     time.Time `json:"endedAt"`
	Compactions int       `json:"compactions"`
	CostUSD     float64   `json:"costUsd"`
	TotalTokens int64     `json:"totalTokens"`
	DurationMs  int64     `json:"durationMs"`
}

// SizingResult is the work-sizing block's payload. The medians are the point:
// compaction is a *marker* of work that was already too big for one sitting,
// not a cause of cost — the block reports both numbers and lets them speak.
type SizingResult struct {
	Sessions      []SizingSession `json:"sessions"`      // most compactions first
	TotalSessions int             `json:"totalSessions"` // sessions that compacted at all
	Scanned       int             `json:"scanned"`       // every session in the window
	// Median (not mean) cost of sessions that never compacted vs those that hit
	// HeavyCompactions. A handful of $400 sessions would drag a mean somewhere
	// no real session sits.
	MedianCostClean float64 `json:"medianCostClean"`
	MedianCostHeavy float64 `json:"medianCostHeavy"`
	CleanCount      int     `json:"cleanCount"`
	HeavyCount      int     `json:"heavyCount"`
	HeavyThreshold  int     `json:"heavyThreshold"`
}

// LedgerWeek is one week of spend measured against what came out of it.
// CostPerPR is the week's WHOLE spend divided by the PRs it opened — not the
// price of a PR. Exploring, debugging and arguing about a plan all cost money
// and open nothing; that's the point of dividing by outcomes rather than
// costing them individually.
type LedgerWeek struct {
	Week           string    `json:"week"` // ISO, e.g. "2026-W29"
	StartsOn       time.Time `json:"startsOn"`
	Sessions       int       `json:"sessions"`
	CostUSD        float64   `json:"costUsd"`
	Lines          int       `json:"lines"` // added + removed
	PRs            int       `json:"prs"`
	Releases       int       `json:"releases"`
	CostPerPR      float64   `json:"costPerPr"`      // 0 when the week opened none
	CostPer1kLines float64   `json:"costPer1kLines"` // 0 when nothing moved
}

// LedgerResult is the cost-per-outcome block: week by week, newest first, plus
// the window's totals. No $/ticket — see the note in docs/plan.md: the board has
// one done card, so that number would be an average of one.
type LedgerResult struct {
	Weeks []LedgerWeek `json:"weeks"`
	Total LedgerWeek   `json:"total"` // Week/StartsOn empty; the window summed
}

// AgentRun is one subagent invocation, reconstructed from its own
// subagents/agent-<id>.jsonl + .meta.json, enriched from the parent
// transcript's toolUseResult rollup when present.
type AgentRun struct {
	ID            string       `json:"id"`
	SessionID     string       `json:"sessionId"`
	ParentAgentID string       `json:"parentAgentId"` // "" = spawned by the main session
	AgentType     string       `json:"agentType"`
	Description   string       `json:"description"`
	Model         string       `json:"model"`   // family, for coloring
	ModelID       string       `json:"modelId"` // full model string, e.g. claude-sonnet-5
	Status        string       `json:"status"`
	MessageCount  int          `json:"messageCount"`
	SpawnDepth    int          `json:"spawnDepth"`
	StartedAt     time.Time    `json:"startedAt"`
	EndedAt       time.Time    `json:"endedAt"`
	DurationMs    int64        `json:"durationMs"`
	Tokens        TokenBuckets `json:"tokens"`
	TotalTokens   int64        `json:"totalTokens"`
	ToolUseCount  int          `json:"toolUseCount"`
	ToolStats     *ToolStats   `json:"toolStats,omitempty"`
	CostUSD       float64      `json:"costUsd"`
	// LinesAdded/LinesRemoved: this agent's own edits, approximated from its
	// Edit/Write tool inputs (subagent transcripts carry no structuredPatch).
	LinesAdded   int         `json:"linesAdded"`
	LinesRemoved int         `json:"linesRemoved"`
	FilesChanged int         `json:"filesChanged"`    // distinct files this agent edited
	Tools        []ToolCount `json:"tools,omitempty"` // per-tool counts from its own transcript (detail-only)
	// ToolEvents is this agent's per-tool timeline (detail-only, downsampled
	// to a cap; ToolEventsDropped counts what the downsampling skipped).
	ToolEvents        []ToolEvent `json:"toolEvents,omitempty"`
	ToolEventsDropped int         `json:"toolEventsDropped,omitempty"`
	Running           bool        `json:"running"`
}

// Session is one Claude Code session: the main transcript's own usage plus
// summary numbers over its agent runs. Main-thread tokens and agent tokens
// are kept separate so the UI can show both.
type Session struct {
	ID      string `json:"id"`
	Project string `json:"project"`
	// CWD: the session's working directory from its first transcript line.
	// Index-internal — the code-graph endpoints hop folder → cwd → repo with
	// it (see codegraph.go); it never rides an API payload.
	CWD          string    `json:"-"`
	Slug         string    `json:"slug"`
	Title        string    `json:"title"`
	GitBranch    string    `json:"gitBranch"`
	PRURL        string    `json:"prUrl,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
	EndedAt      time.Time `json:"endedAt"`
	DurationMs   int64     `json:"durationMs"`
	Models       []string  `json:"models"` // families, main thread, most-used first
	MessageCount int       `json:"messageCount"`
	CompactCount int       `json:"compactCount"`
	// Interrupts/Denials: how often the user stopped this session mid-flight, and
	// how often they refused a tool permission. Cheap counts, so both ride the
	// list payload alongside CompactCount.
	Interrupts int `json:"interrupts"`
	Denials    int `json:"denials"`
	// InterruptTimes: when each interrupt landed, for the timeline's ticks.
	// Detail-only. No tool is attached on purpose — see noteFriction.
	InterruptTimes []time.Time `json:"interruptTimes,omitempty"`
	// DenialTools: which tools were refused, most first. Index-internal; the
	// friction aggregate sums them across sessions.
	DenialTools []ToolCount  `json:"-"`
	Tokens      TokenBuckets `json:"tokens"` // main thread only
	TotalTokens int64        `json:"totalTokens"`
	CostUSD     float64      `json:"costUsd"`
	// LinesAdded/LinesRemoved: edited lines across the main thread (from each
	// edit's structuredPatch) and its agents (from their tool-stat rollups).
	LinesAdded   int `json:"linesAdded"`
	LinesRemoved int `json:"linesRemoved"`
	// FilesChanged: distinct files edited/written across the main thread + agents.
	FilesChanged int `json:"filesChanged"`
	// FileEdits: the same edits, per file, path-keyed. Index-internal (see FileEdit).
	FileEdits []FileEdit `json:"-"`
	// ContextTokens ≈ prompt size of the most recent request (fresh input +
	// cache reads + cache writes), i.e. current context fill.
	ContextTokens int64   `json:"contextTokens"`
	ContextWindow int64   `json:"contextWindow"`
	AgentCount    int     `json:"agentCount"`
	AgentTokens   int64   `json:"agentTokens"`
	AgentCostUSD  float64 `json:"agentCostUsd"`
	// ModelBreakdown splits total tokens + cost by model family (main + agents),
	// most tokens first. Included on both list and detail responses.
	ModelBreakdown []ModelUsage `json:"modelBreakdown,omitempty"`
	// Main-thread-only breakdowns for the detail view, so the main agent shows
	// as a first-class row/card/timeline alongside its subagents. Detail-only.
	MainToolStats    *ToolStats     `json:"mainToolStats,omitempty"`
	MainFilesChanged int            `json:"mainFilesChanged"`
	MainTools        []ToolCount    `json:"mainTools,omitempty"`
	MainActivity     []ActivitySlot `json:"mainActivity,omitempty"` // tool uses bucketed over the main thread's span
	// MainFlow is the main thread's action flow: an ordered spine of phase
	// nodes (consecutive same-kind tool uses folded together). Detail-only.
	MainFlow []FlowNode `json:"mainFlow,omitempty"`
	// MainToolEvents is the main thread's per-tool timeline (detail-only,
	// downsampled to a cap; MainToolEventsDropped counts what was skipped).
	MainToolEvents        []ToolEvent `json:"mainToolEvents,omitempty"`
	MainToolEventsDropped int         `json:"mainToolEventsDropped,omitempty"`
	// Milestones is the session's semantic arc (plans, branches, commits, PRs,
	// releases) across the main thread + every subagent, time-ordered. Detail-only.
	Milestones []Milestone `json:"milestones,omitempty"`
	// MilestoneGroups is the same arc folded into branch-scoped work units
	// (branch cut → … → release); the UI renders these as collapsible blocks.
	MilestoneGroups []MilestoneGroup `json:"milestoneGroups,omitempty"`
	Running         bool             `json:"running"`
	// Archived mirrors the Claude Code app's per-session archive flag (from its
	// session store); a session with no store entry counts as archived too. A
	// running session is always reported active.
	Archived bool `json:"archived"`

	// Agents is populated on the detail endpoint / SSE detail events only.
	Agents []AgentRun `json:"agents,omitempty"`
}

// --- transcript line shapes (only the fields the scanner reads) ---

type usageJSON struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

type messageJSON struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Usage   *usageJSON    `json:"usage"`
	Content []contentJSON `json:"content"`
}

type contentJSON struct {
	Type  string `json:"type"`
	Text  string `json:"text"` // text block: the message text, mined for the search index
	ID    string `json:"id"`   // tool_use
	Name  string `json:"name"` // tool_use
	Input *struct {
		Description  string `json:"description"`
		SubagentType string `json:"subagent_type"`
		Model        string `json:"model"`
		// Bash command + ExitPlanMode plan, mined for session milestones.
		Command string `json:"command"`
		Plan    string `json:"plan"`
		// Edit / Write / MultiEdit payloads, for line + file counting.
		FilePath  string `json:"file_path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
		Content   string `json:"content"`
		Edits     []struct {
			OldString string `json:"old_string"`
			NewString string `json:"new_string"`
		} `json:"edits"`
	} `json:"input"`
}

// patchHunk is one hunk of an Edit/Write tool result's structuredPatch; its
// Lines carry the unified-diff prefixes (" ", "+", "-").
type patchHunk struct {
	Lines []string `json:"lines"`
}

// rollupJSON is the polymorphic toolUseResult line: an agent rollup (AgentID +
// ToolStats) for Task tools, or a main-thread edit (StructuredPatch) otherwise.
type rollupJSON struct {
	Status string `json:"status"`
	// FilePath is set on a main-thread edit result, naming the file its
	// StructuredPatch belongs to — the only link from exact +/- lines to a path.
	FilePath        string      `json:"filePath"`
	AgentID         string      `json:"agentId"`
	AgentType       string      `json:"agentType"`
	ResolvedModel   string      `json:"resolvedModel"`
	TotalDurationMs int64       `json:"totalDurationMs"`
	TotalTokens     int64       `json:"totalTokens"`
	TotalToolUse    int         `json:"totalToolUseCount"`
	ToolStats       *ToolStats  `json:"toolStats"`
	StructuredPatch []patchHunk `json:"structuredPatch"`
}

// UnmarshalJSON tolerates a toolUseResult that isn't an object. Claude Code
// writes one for most tools but a bare string for others ("User rejected tool
// use") and a block list for some — 13.5k lines of this corpus. Because the
// field was typed as an object, any such line failed to decode and the scanner
// skipped it whole, which is why permission denials counted zero: their line is
// exactly the one that says "User rejected tool use". Nothing else was lost —
// none of those lines carry usage or tool_use blocks — so this unlocks the
// rejection without moving a single existing number.
func (r *rollupJSON) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || b[0] != '{' {
		return nil // not an object: no rollup fields to read, and not an error
	}
	type plain rollupJSON // shed the method, or this recurses
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*r = rollupJSON(p)
	return nil
}

type lineJSON struct {
	Type          string       `json:"type"`
	Subtype       string       `json:"subtype"`
	UUID          string       `json:"uuid"`
	Timestamp     string       `json:"timestamp"`
	CWD           string       `json:"cwd"`
	Slug          string       `json:"slug"`
	GitBranch     string       `json:"gitBranch"`
	RequestID     string       `json:"requestId"`
	CustomTitle   string       `json:"customTitle"`
	PRURL         string       `json:"prUrl"`
	Message       *messageJSON `json:"message"`
	ToolUseResult *rollupJSON  `json:"toolUseResult"`
}

type agentMetaJSON struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
}
