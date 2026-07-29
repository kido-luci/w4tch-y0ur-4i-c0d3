package main

// AI one-liners for milestone groups. Generated on demand (button-triggered,
// never automatic) by shelling out to the user's own `claude` CLI in headless
// mode on haiku — no API key to manage, no transcript left behind
// (--no-session-persistence). The only data that leaves the machine is the
// milestone labels themselves: branch names, commit subjects, tag names — text
// the agent wrote in the first place. Results are cached on disk keyed by a
// hash of the milestone list, so a finished session is summarized exactly once.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"watch-your-ai-code/internal/index"
)

const (
	summaryModel   = "haiku"
	summaryTimeout = 120 * time.Second
)

// summaryFile is the on-disk cache for one session's group summaries.
type summaryFile struct {
	Hash      string    `json:"hash"` // milestonesHash of the list these came from
	Model     string    `json:"model"`
	CreatedAt time.Time `json:"createdAt"`
	Summaries []string  `json:"summaries"` // aligned to milestoneGroups by index
}

// Summarizer generates and caches milestone-group summaries. One generation
// runs per session at a time; a concurrent request blocks, then finds the
// fresh cache and returns without a second claude call.
type Summarizer struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-session generation locks
}

func NewSummarizer() *Summarizer {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "watch-your-ai-code", "summaries")
	_ = os.MkdirAll(dir, 0o755)
	return &Summarizer{dir: dir, locks: map[string]*sync.Mutex{}}
}

// milestonesHash pins a summary set to the exact milestone list it was
// generated from; groups derive deterministically from that list, so the one
// hash covers both.
func milestonesHash(ms []index.Milestone) string {
	h := sha256.New()
	for _, m := range ms {
		fmt.Fprintf(h, "%s|%s\n", m.Kind, m.Label)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (su *Summarizer) path(id string) string {
	return filepath.Join(su.dir, filepath.Base(id)+".json")
}

func (su *Summarizer) load(id string) *summaryFile {
	b, err := os.ReadFile(su.path(id))
	if err != nil {
		return nil
	}
	var sf summaryFile
	if json.Unmarshal(b, &sf) != nil {
		return nil
	}
	return &sf
}

func (su *Summarizer) store(id string, sf *summaryFile) {
	b, err := json.Marshal(sf)
	if err != nil {
		return
	}
	// Write-then-rename, like every other store here: Cached reads without
	// the generation lock, so the final name must never hold a half-written
	// file (a torn read would just cost a pointless re-summarize, but the
	// atomic replace costs nothing).
	path := su.path(id)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Cached returns any stored summaries plus whether they still match the
// session's current milestones. Stale summaries are returned too — the UI
// keeps showing them (prefix-aligned; groups only grow at the end) while
// offering to re-summarize. Freshness also requires the model that wrote
// them to still be the configured one, so a model bump re-summarizes
// instead of serving the old model's output as current forever.
func (su *Summarizer) Cached(id string, ms []index.Milestone) (summaries []string, fresh bool) {
	sf := su.load(id)
	if sf == nil {
		return nil, false
	}
	return sf.Summaries, sf.Hash == milestonesHash(ms) && sf.Model == summaryModel
}

func (su *Summarizer) lockFor(id string) *sync.Mutex {
	su.mu.Lock()
	defer su.mu.Unlock()
	l := su.locks[id]
	if l == nil {
		l = &sync.Mutex{}
		su.locks[id] = l
	}
	return l
}

// Summarize returns one sentence per milestone group, generating and caching
// them when the cache is missing or stale.
func (su *Summarizer) Summarize(ctx context.Context, id string, groups []index.MilestoneGroup, ms []index.Milestone) ([]string, error) {
	l := su.lockFor(id)
	l.Lock()
	defer l.Unlock()

	hash := milestonesHash(ms)
	if sf := su.load(id); sf != nil && sf.Hash == hash && sf.Model == summaryModel {
		return sf.Summaries, nil
	}

	raw, err := runClaude(ctx, summaryPrompt(groups))
	if err != nil {
		return nil, err
	}
	sums, err := parseSummaries(raw, len(groups))
	if err != nil {
		return nil, err
	}
	su.store(id, &summaryFile{Hash: hash, Model: summaryModel, CreatedAt: time.Now(), Summaries: sums})
	return sums, nil
}

func summaryPrompt(groups []index.MilestoneGroup) string {
	var b strings.Builder
	b.WriteString("Below are groups of git milestones from one AI coding session, in order.\n")
	b.WriteString("For each group, write one plain sentence (max 14 words) stating what that group accomplished.\n")
	b.WriteString("Reply with ONLY a JSON array of strings — one per group, same order, no markdown fences.\n")
	for i, g := range groups {
		fmt.Fprintf(&b, "\nGroup %d:\n", i+1)
		for _, m := range g.Milestones {
			fmt.Fprintf(&b, "- %s: %s\n", m.Kind, m.Label)
		}
	}
	return b.String()
}

// findClaude resolves the claude binary: $PATH first, then the usual installs.
// The launchd agent runs with a minimal PATH, so LookPath alone isn't enough.
func findClaude() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".claude", "local", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
		filepath.Join(home, ".local", "bin", "claude"),
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// cleanEnv strips nested-Claude pollution (ANTHROPIC_*, CLAUDE*) so a viewer
// started from inside a Claude Code session still reaches the user's own CLI
// credentials rather than the parent session's proxy.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		up := strings.ToUpper(kv)
		if strings.HasPrefix(up, "ANTHROPIC_") || strings.HasPrefix(up, "CLAUDE") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func runClaude(ctx context.Context, prompt string) (string, error) {
	bin := findClaude()
	if bin == "" {
		return "", fmt.Errorf("claude CLI not found (looked in PATH, ~/.claude/local, /opt/homebrew/bin, /usr/local/bin, ~/.local/bin)")
	}
	ctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-p", prompt, "--model", summaryModel, "--no-session-persistence")
	cmd.Env = cleanEnv()
	cmd.Dir = os.TempDir() // neutral cwd, not a project dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return "", fmt.Errorf("claude failed: %s", msg)
	}
	return out.String(), nil
}

// parseSummaries extracts the JSON array of strings from claude's reply,
// tolerating fences or stray prose around it.
func parseSummaries(raw string, want int) ([]string, error) {
	start := strings.Index(raw, "[")
	end := strings.LastIndex(raw, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array in claude reply")
	}
	var out []string
	if err := json.Unmarshal([]byte(raw[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("bad JSON in claude reply: %w", err)
	}
	if len(out) != want {
		return nil, fmt.Errorf("claude returned %d summaries for %d groups", len(out), want)
	}
	return out, nil
}
