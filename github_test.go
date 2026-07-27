package main

import "testing"

func TestReGitHubRemote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" means no match
	}{
		{"ssh", "git@github.com:acme/example-repo.git", "acme/example-repo"},
		{"https no .git", "https://github.com/acme/example-repo", "acme/example-repo"},
		{"https with .git", "https://github.com/acme/example-repo.git", "acme/example-repo"},
		{"gitlab ssh", "git@gitlab.com:foo/bar.git", ""},
		{"other host https", "https://xddlabs.com/x/y.git", ""},
	}
	for _, c := range cases {
		m := reGitHubRemote.FindStringSubmatch(c.in)
		if c.want == "" {
			if m != nil {
				t.Errorf("%s: FindStringSubmatch(%q) = %v, want no match", c.name, c.in, m)
			}
			continue
		}
		if m == nil || m[1] != c.want {
			t.Errorf("%s: FindStringSubmatch(%q) = %v, want submatch %q", c.name, c.in, m, c.want)
		}
	}
}

func TestRollupChecks(t *testing.T) {
	// Identical to the anonymous struct type rollupChecks takes — a true
	// alias, so []entry is the exact same type as its parameter.
	type entry = struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	}
	cases := []struct {
		name string
		in   []entry
		want string
	}{
		{"empty", nil, ""},
		{"completed success", []entry{{Status: "COMPLETED", Conclusion: "SUCCESS"}}, "success"},
		{"failure conclusion", []entry{{Conclusion: "FAILURE"}}, "failure"},
		{"in-progress status", []entry{{Status: "IN_PROGRESS"}}, "pending"},
		{"state success", []entry{{State: "SUCCESS"}}, "success"},
		{"mix success and failure", []entry{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Conclusion: "FAILURE"},
		}, "failure"},
		{"mix success and in-progress", []entry{
			{Status: "COMPLETED", Conclusion: "SUCCESS"},
			{Status: "IN_PROGRESS"},
		}, "pending"},
	}
	for _, c := range cases {
		if got := rollupChecks(c.in); got != c.want {
			t.Errorf("%s: rollupChecks(%+v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestParseGHPRs(t *testing.T) {
	data := []byte(`[{"number":5,"title":"t","author":{"login":"me"},"state":"OPEN","isDraft":false,"headRefName":"feat/x","reviewDecision":"APPROVED","url":"u","createdAt":"2026-07-21T10:00:00Z","updatedAt":"2026-07-21T11:00:00Z","statusCheckRollup":[{"status":"COMPLETED","conclusion":"SUCCESS"}]}]`)
	prs, ok := parseGHPRs(data)
	if !ok {
		t.Fatalf("parseGHPRs ok = false, want true")
	}
	if len(prs) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(prs), prs)
	}
	p := prs[0]
	if p.Number != 5 || p.Author != "me" || p.Branch != "feat/x" || p.Review != "approved" || p.Checks != "success" {
		t.Errorf("pr = %+v, want number=5 author=me branch=feat/x review=approved checks=success", p)
	}

	if _, ok := parseGHPRs([]byte(`[`)); ok {
		t.Error("invalid JSON: ok = true, want false")
	}
}

func TestParseGHIssues(t *testing.T) {
	data := []byte(`[{"number":9,"title":"bug","author":{"login":"a"},"labels":[{"name":"p1"},{"name":"ui"}],"url":"u","updatedAt":"2026-07-21T10:00:00Z"}]`)
	issues, ok := parseGHIssues(data)
	if !ok {
		t.Fatalf("parseGHIssues ok = false, want true")
	}
	if len(issues) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(issues), issues)
	}
	is := issues[0]
	if is.Number != 9 || is.Author != "a" || len(is.Labels) != 2 || is.Labels[0] != "p1" || is.Labels[1] != "ui" {
		t.Errorf("issue = %+v, want number=9 author=a labels=[p1 ui]", is)
	}

	if _, ok := parseGHIssues([]byte(`[`)); ok {
		t.Error("invalid JSON: ok = true, want false")
	}
}

func TestParseGHRuns(t *testing.T) {
	data := []byte(`[{"displayTitle":"CI","workflowName":"build","status":"completed","conclusion":"success","headBranch":"main","url":"u","createdAt":"2026-07-21T10:00:00Z"}]`)
	runs, ok := parseGHRuns(data)
	if !ok {
		t.Fatalf("parseGHRuns ok = false, want true")
	}
	if len(runs) != 1 {
		t.Fatalf("len = %d, want 1: %+v", len(runs), runs)
	}
	r := runs[0]
	if r.Title != "CI" || r.Workflow != "build" || r.Branch != "main" {
		t.Errorf("run = %+v, want title=CI workflow=build branch=main", r)
	}

	if _, ok := parseGHRuns([]byte(`[`)); ok {
		t.Error("invalid JSON: ok = true, want false")
	}
}
