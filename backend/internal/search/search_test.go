package search

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"watch-your-ai-code/internal/index"
)

// newSearchIndex builds an Index over root with a real index.db in a temp
// config dir, scanned and persisted — the full path a live search takes.
func newSearchIndex(t *testing.T, root string) *index.Index {
	t.Helper()
	db, err := index.OpenDB(filepath.Join(t.TempDir(), "cfg"), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ix := index.New(root)
	ix.UseCache(db)
	if _, err := ix.Rescan(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func searchRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "proj-s")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		// A user asking about something — searchable.
		`{"type":"user","uuid":"s1","timestamp":"2026-07-17T10:00:00.000Z","cwd":"/Users/x/dev/proj-s","message":{"role":"user","content":[{"type":"text","text":"can we use PostgreSQL for this"}]}}`,
		// An assistant answering — searchable, and case must not matter.
		`{"type":"assistant","uuid":"s2","timestamp":"2026-07-17T10:01:00.000Z","requestId":"r1","message":{"id":"m1","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":5,"output_tokens":5},"content":[{"type":"text","text":"postgres would work here"}]}}`,
		// A tool_use whose input mentions it. NOT searchable: tool inputs are
		// where whole files and command output live.
		`{"type":"assistant","uuid":"s3","timestamp":"2026-07-17T10:02:00.000Z","requestId":"r2","message":{"id":"m2","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":5,"output_tokens":5},"content":[{"type":"tool_use","id":"tu_1","name":"Bash","input":{"command":"psql postgres -c 'select 1'"}}]}}`,
		// A tool_result echoing it. Also not searchable.
		`{"type":"user","uuid":"s4","timestamp":"2026-07-17T10:03:00.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"postgres says hi"}]}}`,
		// Vietnamese with diacritics — the reason FTS replaced the grep.
		`{"type":"assistant","uuid":"s5","timestamp":"2026-07-17T10:04:00.000Z","requestId":"r3","message":{"id":"m3","role":"assistant","model":"claude-sonnet-5","usage":{"input_tokens":5,"output_tokens":5},"content":[{"type":"text","text":"đang gặp lỗi khi build frontend"}]}}`,
	}
	path := filepath.Join(dir, "44444444-aaaa-bbbb-cccc-000000000004.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// newSearchFixture is newSearchIndex plus the searcher built over it — what the
// search tests actually exercise.
func newSearchFixture(t *testing.T, root string) *Searcher {
	t.Helper()
	ix := newSearchIndex(t, root)
	return New(ix.DB(), ix)
}

func TestSearchMatchesConversationNotTooling(t *testing.T) {
	se := newSearchFixture(t, searchRoot(t))

	res := se.Search("postgres", 0, "", 10)

	// The user's question and the assistant's reply — not the Bash command
	// that ran it, nor the output it printed.
	if len(res.Hits) != 2 {
		t.Fatalf("hits = %d, want 2 (user + assistant text only): %+v", len(res.Hits), res.Hits)
	}
	// Newest first: the assistant reply (10:01) before the user question (10:00).
	if res.Hits[0].Role != "assistant" || res.Hits[1].Role != "user" {
		t.Errorf("roles = %s, %s — want assistant then user (newest first)",
			res.Hits[0].Role, res.Hits[1].Role)
	}
	// The user's line says "PostgreSQL" — matching it proves case folding.
	if !strings.Contains(res.Hits[1].Snippet, "PostgreSQL") {
		t.Errorf("snippet = %q, want the user's original casing", res.Hits[1].Snippet)
	}
	if res.Hits[0].SessionID != "44444444-aaaa-bbbb-cccc-000000000004" {
		t.Errorf("hit not tagged with its session: %+v", res.Hits[0])
	}
	if res.Hits[0].Project != "proj-s" {
		t.Errorf("project = %q, want proj-s", res.Hits[0].Project)
	}
	if res.Hits[0].Ts.IsZero() {
		t.Error("hit lost its timestamp")
	}
	if res.Matched != 2 || res.Files != 1 {
		t.Errorf("Matched=%d Files=%d, want 2 and 1", res.Matched, res.Files)
	}
	if res.Truncated {
		t.Error("nothing was cut, must not read as truncated")
	}
}

func TestSearchFoldsVietnameseDiacritics(t *testing.T) {
	se := newSearchFixture(t, searchRoot(t))

	// Typing without diacritics must find the accented text …
	res := se.Search("loi", 0, "", 10)
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Snippet, "lỗi") {
		t.Fatalf("Search(loi) = %+v, want the 'gặp lỗi' line", res.Hits)
	}
	// … and so must the accented text itself, in any case.
	if res := se.Search("LỖI", 0, "", 10); len(res.Hits) != 1 {
		t.Errorf("Search(LỖI) = %d hits, want 1", len(res.Hits))
	}
}

func TestSearchPrefixOnLastToken(t *testing.T) {
	se := newSearchFixture(t, searchRoot(t))
	if res := se.Search("postg", 0, "", 10); len(res.Hits) != 2 {
		t.Errorf("half-typed word should prefix-match: got %d hits, want 2", len(res.Hits))
	}
}

func TestSearchFiltersAndHostileInput(t *testing.T) {
	se := newSearchFixture(t, searchRoot(t))

	if res := se.Search("postgres", 0, "proj-s", 10); len(res.Hits) != 2 {
		t.Errorf("project filter dropped its own project: %d hits", len(res.Hits))
	}
	if res := se.Search("postgres", 0, "other-proj", 10); len(res.Hits) != 0 || res.Matched != 0 {
		t.Errorf("project filter leaked: %+v", res)
	}
	// A group scope sends several comma-separated names — any member matches.
	if res := se.Search("postgres", 0, "other-proj,proj-s", 10); len(res.Hits) != 2 {
		t.Errorf("multi-project filter dropped a member's hits: %d hits", len(res.Hits))
	}
	if res := se.Search("postgres", 0, "other-a,other-b", 10); len(res.Hits) != 0 || res.Matched != 0 {
		t.Errorf("multi-project filter leaked: %+v", res)
	}
	if res := se.Search("mysql", 0, "", 10); len(res.Hits) != 0 {
		t.Errorf("want no hits, got %+v", res.Hits)
	}
	if res := se.Search("", 0, "", 10); len(res.Hits) != 0 {
		t.Errorf("empty query must return nothing, got %+v", res.Hits)
	}
	// FTS5 operators and stray quotes must reach the parser quoted, not raw.
	for _, hostile := range []string{`AND OR NOT NEAR(`, `"unbalanced`, `a*b (c)`} {
		if res := se.Search(hostile, 0, "", 10); res.Hits == nil {
			t.Errorf("hostile input %q must yield an empty result, not an error", hostile)
		}
	}
}

func TestSearchWithoutDB(t *testing.T) {
	ix := index.New(t.TempDir())
	se := New(ix.DB(), ix)
	if res := se.Search("anything", 0, "", 10); len(res.Hits) != 0 {
		t.Errorf("nil db must mean empty search, got %+v", res.Hits)
	}
}

func TestFtsQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"  ", ""},
		{"one", `"one"*`},
		{"two words", `"two" "words"*`},
		{`quo"te`, `"quo""te"*`},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
