package search

// Transcript search, over the FTS5 table in <config-dir>/index.db.
//
// This was a query-time grep until v0.43.0, and the spec used to promise that
// message text never entered the index. That promise was retired deliberately
// (see docs/spec.md, "Search index"): user/assistant text blocks of main
// transcripts are indexed locally — same machine, same config dir, still zero
// egress — because token matching with diacritics folding ("loi" finds "lỗi")
// is what searching a Vietnamese corpus actually needs, and the grep could
// never rank or fold. Tool inputs and results stay out of the index, exactly
// as before: they are where whole files and command output live, and indexing
// them would bury "where did we discuss X" under everything ever read.

import (
	"database/sql"
	"log"
	"strings"
	"time"

	"watch-your-ai-code/internal/index"
)

// SearchHit is one match inside one session's transcript: where it was, when,
// and the text around it. Snippets are cut at a fixed width — enough to
// recognise the moment, never the whole message.
type SearchHit struct {
	SessionID string    `json:"sessionId"`
	Title     string    `json:"title"`
	Project   string    `json:"project"`
	Ts        time.Time `json:"ts"`
	Role      string    `json:"role"` // user | assistant
	Snippet   string    `json:"snippet"`
}

// SearchResult is a query-time grep: nothing here is indexed, which is what
// keeps the spec's "message text is never read into the index" literally true.
// Matched counts every hit found, Hits carries at most `limit` of them — a
// capped list must never read as the whole answer.
type SearchResult struct {
	Hits      []SearchHit `json:"hits"`
	Matched   int         `json:"matched"`
	Files     int         `json:"files"` // transcripts actually opened
	Truncated bool        `json:"truncated"`
	TookMs    int64       `json:"tookMs"`
}

// Search runs q against the message index, filtered by window and project,
// newest first, at most limit hits. Matching is FTS5 token matching — every
// term must appear, the last term also matches as a prefix ("tok" finds
// "token") — not substring: a mid-word fragment no longer matches.
// Sessions is the slice of the session index search reads: the title
// and project of the session a hit belongs to. SessionRef rather than Session
// so labelling a hit doesn't copy the whole parse, agent runs included.
type Sessions interface {
	SessionRef(id string) *index.Session
}

// Searcher queries the message FTS table of index.db (whose schema the index
// owns, see internal/index/db.go). A nil db means the cache is disabled — an empty result,
// never an error.
type Searcher struct {
	db       *sql.DB
	sessions Sessions
}

func New(db *sql.DB, ss Sessions) *Searcher {
	return &Searcher{db: db, sessions: ss}
}

func (se *Searcher) Search(q string, days int, project string, limit int) SearchResult {
	start := time.Now()
	res := SearchResult{Hits: []SearchHit{}}
	match := ftsQuery(q)
	if match == "" || se.db == nil {
		return res
	}
	var cutoff int64
	if days > 0 {
		cutoff = time.Now().AddDate(0, 0, -days).UnixNano()
	}
	if limit <= 0 {
		limit = -1 // SQLite: no limit
	}

	// The project param may carry several comma-separated names (a group
	// scope), so the filter is an IN list built to size — absent entirely
	// when unfiltered.
	projFilter := ""
	args := []any{match, cutoff}
	if projects := index.SplitProjects(project); projects != nil {
		ph := strings.TrimPrefix(strings.Repeat(",?", len(projects)), ",")
		projFilter = ` AND session_id IN (SELECT id FROM sessions WHERE project IN (` + ph + `))`
		for _, p := range projects {
			args = append(args, p)
		}
	}

	// Matched counts every hit, Files every session holding one — a capped
	// list must never read as the whole answer. The window filter is now
	// per-message (the grep filtered whole sessions by EndedAt), so with days
	// set, undated lines drop out instead of riding along.
	err := se.db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT session_id) FROM messages
		WHERE messages MATCH ? AND ts >= ?`+projFilter,
		args...).Scan(&res.Matched, &res.Files)
	if err != nil {
		log.Printf("search %q: %v", q, err)
		return res
	}

	rows, err := se.db.Query(`
		SELECT session_id, role, ts, snippet(messages, 0, '', '', '…', 24)
		FROM messages
		WHERE messages MATCH ? AND ts >= ?`+projFilter+`
		ORDER BY ts DESC LIMIT ?`,
		append(append([]any{}, args...), limit)...)
	if err != nil {
		log.Printf("search %q: %v", q, err)
		return res
	}
	defer rows.Close()

	type rawHit struct {
		sid, role, snippet string
		ns                 int64
	}
	var raw []rawHit
	for rows.Next() {
		var h rawHit
		if rows.Scan(&h.sid, &h.role, &h.ns, &h.snippet) == nil {
			raw = append(raw, h)
		}
	}

	// Title/Project come from the in-memory index; a row whose session is gone
	// (pruned between write and read) is dropped rather than served half-blank.
	for _, h := range raw {
		s := se.sessions.SessionRef(h.sid)
		if s == nil {
			continue
		}
		hit := SearchHit{
			SessionID: h.sid,
			Title:     s.Title,
			Project:   s.Project,
			Role:      h.role,
			Snippet:   h.snippet,
		}
		if h.ns != 0 {
			hit.Ts = time.Unix(0, h.ns)
		}
		res.Hits = append(res.Hits, hit)
	}

	res.Truncated = res.Matched > len(res.Hits)
	res.TookMs = time.Since(start).Milliseconds()
	return res
}

// ftsQuery turns free text into an FTS5 query: every whitespace token is
// quoted (operators like AND/NEAR and stray punctuation must not reach the
// query parser raw), all terms are required, and the last one also matches as
// a prefix so a half-typed word still finds itself.
func ftsQuery(q string) string {
	toks := strings.Fields(q)
	if len(toks) == 0 {
		return ""
	}
	for i, t := range toks {
		t = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		if i == len(toks)-1 {
			t += "*"
		}
		toks[i] = t
	}
	return strings.Join(toks, " ")
}
