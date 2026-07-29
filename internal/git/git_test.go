package git

import (
	"strings"
	"testing"
	"time"
)

func TestParseGitStatus(t *testing.T) {
	cases := []struct {
		name                        string
		in                          string
		staged, unstaged, untracked int
	}{
		{"empty", "", 0, 0, 0},
		// M (staged), space-M (unstaged), ?? (untracked), MM (both), A (staged).
		{"mixed", "M  a.go\n M b.go\n?? c.go\nMM d.go\nA  e.go", 3, 2, 1},
		{"untracked only", "?? a.go\n?? b.go", 0, 0, 2},
		{"staged only", "M  a.go\nA  b.go", 2, 0, 0},
	}
	for _, c := range cases {
		staged, unstaged, untracked := parseGitStatus(c.in)
		if staged != c.staged || unstaged != c.unstaged || untracked != c.untracked {
			t.Errorf("%s: parseGitStatus(%q) = (%d,%d,%d), want (%d,%d,%d)",
				c.name, c.in, staged, unstaged, untracked, c.staged, c.unstaged, c.untracked)
		}
	}
}

func TestParseAheadBehind(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		behind, ahead int
		ok            bool
	}{
		{"typical", "2\t3", 2, 3, true},
		{"zero", "0\t0", 0, 0, true},
		{"empty", "", 0, 0, false},
		{"one field", "5", 0, 0, false},
		{"non-numeric", "a\tb", 0, 0, false},
		{"three fields", "1 2 3", 0, 0, false},
	}
	for _, c := range cases {
		behind, ahead, ok := parseAheadBehind(c.in)
		if behind != c.behind || ahead != c.ahead || ok != c.ok {
			t.Errorf("%s: parseAheadBehind(%q) = (%d,%d,%v), want (%d,%d,%v)",
				c.name, c.in, behind, ahead, ok, c.behind, c.ahead, c.ok)
		}
	}
}

func TestParseGitLog(t *testing.T) {
	if got := parseGitLog(""); got != nil {
		t.Errorf("empty input = %+v, want nil", got)
	}

	const when = "2026-07-21T10:00:00Z"
	wantTime, err := time.Parse(time.RFC3339, when)
	if err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		strings.Join([]string{"abc123", "fix: thing", "Ada", when}, "\x1f"),
		strings.Join([]string{"only", "three", "fields"}, "\x1f"), // wrong field count, skipped
		strings.Join([]string{"def456", "garbage date", "Ada", "not-a-date"}, "\x1f"),
	}, "\n")

	got := parseGitLog(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (the 3-field line is skipped): %+v", len(got), got)
	}
	c0 := got[0]
	if c0.Hash != "abc123" || c0.Subject != "fix: thing" || c0.Author != "Ada" {
		t.Errorf("commit 0 = %+v, want abc123/fix: thing/Ada", c0)
	}
	if c0.When.IsZero() || !c0.When.Equal(wantTime) {
		t.Errorf("commit 0 When = %v, want %v", c0.When, wantTime)
	}
	c1 := got[1]
	if c1.Hash != "def456" || c1.Subject != "garbage date" || c1.Author != "Ada" {
		t.Errorf("commit 1 = %+v, want def456/garbage date/Ada", c1)
	}
	if !c1.When.IsZero() {
		t.Errorf("commit 1 When = %v, want zero (unparseable date is still included)", c1.When)
	}
}

func TestParseNumstat(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []gitFileChange
	}{
		{"empty", "", nil},
		{"typical", "12\t3\tfoo.go", []gitFileChange{{Path: "foo.go", Add: 12, Del: 3}}},
		{"binary", "-\t-\timg.png", []gitFileChange{{Path: "img.png", Add: -1, Del: -1}}},
		{"multiple lines", "5\t0\ta.go\n0\t8\tb.go", []gitFileChange{
			{Path: "a.go", Add: 5, Del: 0},
			{Path: "b.go", Add: 0, Del: 8},
		}},
		{"malformed line skipped", "5\t0\ta.go\ngarbage\n0\t8\tb.go", []gitFileChange{
			{Path: "a.go", Add: 5, Del: 0},
			{Path: "b.go", Add: 0, Del: 8},
		}},
	}
	for _, c := range cases {
		got := parseNumstat(c.in)
		if len(got) != len(c.want) {
			t.Errorf("%s: parseNumstat(%q) = %+v, want %+v", c.name, c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: parseNumstat(%q)[%d] = %+v, want %+v", c.name, c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestParseBranchRefs(t *testing.T) {
	if got := parseBranchRefs(""); got != nil {
		t.Errorf("empty input = %+v, want nil", got)
	}

	const when = "2026-07-21T10:00:00Z"
	wantTime, err := time.Parse(time.RFC3339, when)
	if err != nil {
		t.Fatal(err)
	}
	in := strings.Join([]string{
		strings.Join([]string{"main", "*", "fix: thing", when, "refs/heads/main"}, "\x1f"),
		strings.Join([]string{"origin/dev", "", "wip", when, "refs/remotes/origin/dev"}, "\x1f"),
		strings.Join([]string{"origin", "", "", when, "refs/remotes/origin/HEAD"}, "\x1f"), // origin/HEAD alias shortens to "origin"; skipped by full refname
		strings.Join([]string{"only", "three", "fields"}, "\x1f"),                          // wrong field count, skipped
		strings.Join([]string{"broken", "", "garbage date", "not-a-date", "refs/heads/broken"}, "\x1f"),
	}, "\n")

	got := parseBranchRefs(in)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (origin/HEAD and the 3-field line are skipped): %+v", len(got), got)
	}

	b0 := got[0]
	if b0.Name != "main" || b0.Subject != "fix: thing" {
		t.Errorf("branch 0 = %+v, want name=main subject=\"fix: thing\"", b0)
	}
	if !b0.IsCurrent {
		t.Errorf("branch 0 IsCurrent = false, want true (HEAD marker was \"*\")")
	}
	if b0.IsRemote {
		t.Errorf("branch 0 IsRemote = true, want false (refs/heads/main)")
	}
	if b0.When.IsZero() || !b0.When.Equal(wantTime) {
		t.Errorf("branch 0 When = %v, want %v", b0.When, wantTime)
	}

	b1 := got[1]
	if b1.Name != "origin/dev" || b1.Subject != "wip" {
		t.Errorf("branch 1 = %+v, want name=origin/dev subject=wip", b1)
	}
	if b1.IsCurrent {
		t.Errorf("branch 1 IsCurrent = true, want false (HEAD marker was empty)")
	}
	if !b1.IsRemote {
		t.Errorf("branch 1 IsRemote = false, want true (refs/remotes/origin/dev)")
	}

	b2 := got[2]
	if b2.Name != "broken" {
		t.Errorf("branch 2 Name = %q, want broken", b2.Name)
	}
	if !b2.When.IsZero() {
		t.Errorf("branch 2 When = %v, want zero (unparseable date is still included)", b2.When)
	}
}
