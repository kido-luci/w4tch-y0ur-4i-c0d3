package main

import (
	"sync"
	"testing"
	"time"

	"watch-your-ai-code/internal/index"
)

// The cache round-trips through store/Cached, and freshness requires BOTH the
// milestone hash and the generating model to still match — a model bump must
// read as stale, not serve the old model's output as current forever.
func TestSummaryCacheFreshness(t *testing.T) {
	su := &Summarizer{dir: t.TempDir(), locks: map[string]*sync.Mutex{}}
	ms := []index.Milestone{{Kind: "branch", Label: "feat/a"}}

	if sums, fresh := su.Cached("s1", ms); sums != nil || fresh {
		t.Fatalf("empty cache should read as nothing: %v, %v", sums, fresh)
	}

	su.store("s1", &summaryFile{Hash: milestonesHash(ms), Model: summaryModel,
		CreatedAt: time.Now(), Summaries: []string{"did a thing"}})
	if sums, fresh := su.Cached("s1", ms); !fresh || len(sums) != 1 || sums[0] != "did a thing" {
		t.Fatalf("stored summaries should read back fresh: %v, %v", sums, fresh)
	}

	// Same milestones, different generating model → served but stale.
	su.store("s1", &summaryFile{Hash: milestonesHash(ms), Model: "other-model",
		CreatedAt: time.Now(), Summaries: []string{"old model's take"}})
	if sums, fresh := su.Cached("s1", ms); fresh || len(sums) != 1 {
		t.Fatalf("model mismatch should read stale-but-served: %v, %v", sums, fresh)
	}

	// Grown milestones → stale too.
	su.store("s1", &summaryFile{Hash: milestonesHash(ms), Model: summaryModel,
		CreatedAt: time.Now(), Summaries: []string{"did a thing"}})
	grown := append(ms, index.Milestone{Kind: "release", Label: "v1.0.0"})
	if _, fresh := su.Cached("s1", grown); fresh {
		t.Fatal("grown milestone list should read stale")
	}
}

func TestParseSummaries(t *testing.T) {
	// Fences and prose around the array are tolerated.
	got, err := parseSummaries("Here you go:\n```json\n[\"a\", \"b\"]\n```\n", 2)
	if err != nil || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("parse = %v, %v", got, err)
	}

	if _, err := parseSummaries(`["only one"]`, 2); err == nil {
		t.Error("count mismatch should fail")
	}
	if _, err := parseSummaries("no array here", 1); err == nil {
		t.Error("missing array should fail")
	}
	if _, err := parseSummaries(`[1, 2]`, 2); err == nil {
		t.Error("non-string array should fail")
	}
}

func TestMilestonesHash(t *testing.T) {
	ms := []index.Milestone{{Kind: "branch", Label: "feat/a"}, {Kind: "commit", Label: "feat: x"}}
	if milestonesHash(ms) == milestonesHash(ms[:1]) {
		t.Error("hash must change when milestones are added")
	}
	if milestonesHash(ms) != milestonesHash(ms) {
		t.Error("hash must be deterministic")
	}
}
