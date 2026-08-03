package httpapi

// Read-only analytics and search: the derived views over the session index and
// the ship records — activity, churn, friction, sizing, ledger, ships, search,
// stats. Nothing here writes. Split out of api.go's Register; see drawings.go
// for why the parameters carry the names of the locals they replaced.

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"watch-your-ai-code/internal/board"
	"watch-your-ai-code/internal/httpx"
	"watch-your-ai-code/internal/index"
	"watch-your-ai-code/internal/search"
	"watch-your-ai-code/internal/ships"

	"github.com/go-chi/chi/v5"
)

func routeAnalytics(
	router chi.Router,
	ix *index.Index,
	shipStore *ships.Store,
	searchIdx *search.Searcher,
	todos *board.TodoStore,
	events *board.EventStore,
	scopeOf func(*http.Request) board.ScopeSet,
	privateNames func() map[string]bool,
) {
	// Per-day activity buckets for the last `weeks` weeks (default 26), bucketed
	// by the local calendar day of each session's start. Powers the heatmap; its
	// window is independent of the sessions list's day filter.
	router.Get("/api/activity", func(w http.ResponseWriter, r *http.Request) {
		weeks, _ := strconv.Atoi(r.URL.Query().Get("weeks"))
		if weeks <= 0 {
			weeks = 26
		}
		if weeks > 53 {
			weeks = 53
		}
		now := time.Now()
		startToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
		cutoff := startToday.AddDate(0, 0, -(weeks*7 - 1)) // inclusive of today

		// The heatmap is a full-history view: archived sessions are still activity
		// that happened, so it ignores the status filter (status "" = all).
		buckets := map[string]*activityDay{}
		for _, s := range ix.Sessions(0, r.URL.Query().Get("project"), "") {
			d := s.StartedAt.Local()
			if d.Before(cutoff) {
				continue
			}
			key := d.Format("2006-01-02")
			b := buckets[key]
			if b == nil {
				b = &activityDay{Date: key}
				buckets[key] = b
			}
			b.Sessions++
			b.Tokens += s.TotalTokens + s.AgentTokens
			b.CostUSD += s.CostUSD + s.AgentCostUSD
		}

		days := make([]*activityDay, 0, len(buckets))
		for _, b := range buckets {
			days = append(days, b)
		}
		sort.Slice(days, func(i, j int) bool { return days[i].Date < days[j].Date })
		httpx.WriteJSON(w, map[string]any{"weeks": weeks, "days": days})
	})

	// Rework radar: the sessions index pivoted by file. `min` (default 2) drops
	// files only one session ever touched — one edit isn't rework — and `limit`
	// (default 50) bounds the payload; the response's totalFiles says how many
	// passed min, so the UI never implies it showed everything.
	router.Get("/api/churn", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		min := 2
		if v, err := strconv.Atoi(q.Get("min")); err == nil && v > 0 {
			min = v
		}
		limit := 50
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		httpx.WriteJSON(w, ix.Churn(days, q.Get("project"), min, limit))
	})

	// Friction: the sessions you kept stopping. `limit` (default 20) bounds the
	// list; totalSessions says how many had friction at all.
	router.Get("/api/friction", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 20
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 200 {
			limit = 200
		}
		httpx.WriteJSON(w, ix.Friction(days, q.Get("project"), limit))
	})

	// Work sizing: the sessions that outgrew their context. `limit` (default 20)
	// bounds the list; totalSessions says how many compacted at all.
	router.Get("/api/sizing", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 20
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 200 {
			limit = 200
		}
		httpx.WriteJSON(w, ix.Sizing(days, q.Get("project"), limit))
	})

	// Cost per outcome, week by week. No limit: a window holds a handful of
	// weeks, and dropping any would break the trend the block exists to show.
	router.Get("/api/ledger", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		httpx.WriteJSON(w, ix.Ledger(days, r.URL.Query().Get("project")))
	})

	// Ship history: recorded make check / make release runs from the drop dir
	// (see internal/ships). Distinct from /api/ledger, the cost-per-outcome insights.
	// `log=1` includes each run's captured log tail.
	router.Get("/api/ships", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 100
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		out := shipStore.List(q.Get("project"), days, limit, q.Get("log") == "1")
		// Ship records carry Makefile-reported project names — a different
		// namespace from Claude folders, so PresentationGuard skips this path
		// and the private-NAME subtraction happens here instead.
		if private := privateNames(); len(private) > 0 {
			kept := make([]ships.ShipRecord, 0, len(out.Ships))
			for _, s := range out.Ships {
				if !private[s.Project] {
					kept = append(kept, s)
				}
			}
			out.Total -= len(out.Ships) - len(kept)
			out.Ships = kept
		}
		httpx.WriteJSON(w, out)
	})

	// Transcript search over the FTS5 index (see internal/search). `limit` (default
	// 100) bounds the response; `matched` reports what the cap left out.
	router.Get("/api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		days, _ := strconv.Atoi(q.Get("days"))
		limit := 100
		if v, err := strconv.Atoi(q.Get("limit")); err == nil && v > 0 {
			limit = v
		}
		if limit > 500 {
			limit = 500
		}
		httpx.WriteJSON(w, searchIdx.Search(q.Get("q"), days, q.Get("project"), limit))
	})

	router.Get("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		days, _ := strconv.Atoi(r.URL.Query().Get("days"))
		var st statsResponse
		for _, s := range ix.Sessions(days, r.URL.Query().Get("project"), r.URL.Query().Get("status")) {
			st.Sessions++
			st.TotalTokens += s.TotalTokens + s.AgentTokens
			st.TotalCost += s.CostUSD + s.AgentCostUSD
			st.AgentSpawns += s.AgentCount
			if s.Running {
				st.Running++
			}
		}
		httpx.WriteJSON(w, st)
	})
}
