package gsc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeGoogle serves the token endpoint plus searchanalytics/sitemaps routes.
// Handlers may be overridden per test via the exported fields.
type fakeGoogle struct {
	srv          *httptest.Server
	tokenCalls   atomic.Int64
	tokenFail    bool
	sitemapsFail bool
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{}
	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.tokenCalls.Add(1)
		if f.tokenFail {
			http.Error(w, "invalid_grant", http.StatusInternalServerError)
			return
		}
		if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" ||
			r.FormValue("assertion") == "" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-1", "expires_in": 3600})
	})

	mux.HandleFunc("/sites/sc-domain:example.com/searchAnalytics/query", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			StartDate  string   `json:"startDate"`
			EndDate    string   `json:"endDate"`
			Dimensions []string `json:"dimensions"`
			RowLimit   int      `json:"rowLimit"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			len(req.Dimensions) == 0 || req.StartDate == "" || req.EndDate == "" || req.RowLimit <= 0 {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}

		row := func(keys []string, clicks, impressions, ctr, position float64) map[string]any {
			return map[string]any{
				"keys": keys, "clicks": clicks, "impressions": impressions,
				"ctr": ctr, "position": position,
			}
		}
		key := func(k string) []string { return []string{k} }
		// The real API sorts by clicks descending only; zero-click ties come
		// back in arbitrary order — mirrored below so the re-rank is exercised.
		var rows []map[string]any
		switch strings.Join(req.Dimensions, ",") {
		case "date":
			rows = []map[string]any{
				row(key("2026-07-01"), 10, 100, 0.1, 5),
				row(key("2026-07-02"), 30, 300, 0.1, 10),
			}
		case "query":
			rows = []map[string]any{
				row(key("puzzle game"), 12, 60, 0.2, 3.5),
				row(key("zero low"), 0, 5, 0, 40),
				row(key("zero high"), 0, 90, 0, 30),
			}
		case "page,query":
			rows = []map[string]any{
				row([]string{"https://example.com/blog/a/", "flutter firebase"}, 4, 200, 0.02, 9),
				row([]string{"https://example.com/blog/a/", "zero low"}, 0, 10, 0, 20),
				row([]string{"https://example.com/blog/b/", "zero high"}, 0, 150, 0, 12),
			}
		case "page":
			rows = []map[string]any{
				row(key("https://example.com/blog/a/"), 8, 80, 0.1, 4),
				row(key("https://app.example.com/"), 5, 50, 0.1, 6),
				row(key("https://example.com/blog/b/"), 2, 40, 0.05, 9),
			}
		case "device":
			rows = []map[string]any{row(key("MOBILE"), 25, 250, 0.1, 7)}
		case "country":
			rows = []map[string]any{row(key("vnm"), 30, 280, 0.107, 6.5)}
		}
		json.NewEncoder(w).Encode(map[string]any{"rows": rows})
	})

	mux.HandleFunc("/sites/sc-domain:example.com/sitemaps", func(w http.ResponseWriter, r *http.Request) {
		if f.sitemapsFail {
			http.Error(w, "backend error", http.StatusInternalServerError)
			return
		}
		// int64 counters arrive as JSON strings — mirror the real API.
		json.NewEncoder(w).Encode(map[string]any{
			"sitemap": []map[string]any{{
				"path":           "https://example.com/sitemap.xml",
				"lastSubmitted":  "2026-07-01T00:00:00.000Z",
				"lastDownloaded": "2026-07-08T00:00:00.000Z",
				"isPending":      false,
				"errors":         "0",
				"warnings":       "2",
			}},
		})
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTestClient(t *testing.T, f *fakeGoogle) *Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{
		property: "sc-domain:example.com",
		saEmail:  "sa@test.iam.gserviceaccount.com",
		saKey:    key,
		tokenURL: f.srv.URL + "/token",
		apiBase:  f.srv.URL,
		client:   f.srv.Client(),
	}
}

func TestNewKeyFileParsing(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	saJSON, _ := json.Marshal(map[string]string{
		"client_email": "sa@test.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
	})
	keyFile := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(keyFile, saJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	if c := New("sc-domain:example.com", keyFile); !c.Enabled() {
		t.Error("valid property + key file: Enabled() = false, want true")
	}
	if c := New("sc-domain:example.com", filepath.Join(t.TempDir(), "missing.json")); c.Enabled() {
		t.Error("missing key file: Enabled() = true, want false")
	}
	if c := New("", keyFile); c.Enabled() {
		t.Error("empty property: Enabled() = true, want false")
	}
	if c := New("sc-domain:example.com", ""); c.Enabled() {
		t.Error("empty key path: Enabled() = true, want false")
	}
}

func TestRangeWindow(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		rng        string
		start, end string
	}{
		{"7d", "2026-07-02", "2026-07-08"},
		{"28d", "2026-06-11", "2026-07-08"},
		{"90d", "2026-04-10", "2026-07-08"},
	}
	for _, tc := range cases {
		start, end := rangeWindow(tc.rng, now)
		if got := start.Format("2006-01-02"); got != tc.start {
			t.Errorf("%s start = %s, want %s", tc.rng, got, tc.start)
		}
		if got := end.Format("2006-01-02"); got != tc.end {
			t.Errorf("%s end = %s, want %s", tc.rng, got, tc.end)
		}
	}
}

func TestFetchHappyPath(t *testing.T) {
	c := newTestClient(t, newFakeGoogle(t))
	res, err := c.Fetch(context.Background(), Query{Range: "28d"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors != nil {
		t.Fatalf("errors = %v, want none", res.Errors)
	}

	if res.Summary == nil {
		t.Fatal("summary missing")
	}
	if res.Summary.Clicks != 40 || res.Summary.Impressions != 400 {
		t.Errorf("summary totals = %d/%d, want 40/400", res.Summary.Clicks, res.Summary.Impressions)
	}
	if got := res.Summary.CTR; got != 0.1 {
		t.Errorf("summary ctr = %v, want 0.1", got)
	}
	// Impression-weighted: (5*100 + 10*300) / 400 = 8.75
	if got := res.Summary.Position; got != 8.75 {
		t.Errorf("summary position = %v, want 8.75", got)
	}
	if len(res.Series) != 2 || res.Series[0].Date != "2026-07-01" {
		t.Errorf("series = %+v", res.Series)
	}

	// Re-ranked: clicked row pinned first, zero-click ties by impressions.
	if len(res.TopQueries) != 3 || res.TopQueries[0].Name != "puzzle game" ||
		res.TopQueries[1].Name != "zero high" || res.TopQueries[2].Name != "zero low" {
		t.Errorf("topQueries = %+v", res.TopQueries)
	}
	if len(res.TopPages) != 3 {
		t.Errorf("topPages len = %d, want 3", len(res.TopPages))
	}

	// page×query drill-down: same clicks-then-impressions ranking, keys split
	// into page + query.
	if len(res.QueryPages) != 3 {
		t.Fatalf("queryPages = %+v, want 3 rows", res.QueryPages)
	}
	if qp := res.QueryPages[0]; qp.Page != "https://example.com/blog/a/" ||
		qp.Query != "flutter firebase" || qp.Clicks != 4 || qp.Impressions != 200 {
		t.Errorf("queryPages[0] = %+v", qp)
	}
	if res.QueryPages[1].Query != "zero high" || res.QueryPages[2].Query != "zero low" {
		t.Errorf("queryPages tie order = %+v", res.QueryPages)
	}

	// Host aggregation from page URLs: example.com = 10 clicks / 120 impr.
	if len(res.ByHost) != 2 {
		t.Fatalf("byHost = %+v, want 2 hosts", res.ByHost)
	}
	if res.ByHost[0].Name != "example.com" || res.ByHost[0].Clicks != 10 || res.ByHost[0].Impressions != 120 {
		t.Errorf("byHost[0] = %+v", res.ByHost[0])
	}
	if res.ByHost[1].Name != "app.example.com" || res.ByHost[1].Clicks != 5 {
		t.Errorf("byHost[1] = %+v", res.ByHost[1])
	}

	if len(res.Sitemaps) != 1 {
		t.Fatalf("sitemaps = %+v", res.Sitemaps)
	}
	if s := res.Sitemaps[0]; s.Errors != 0 || s.Warnings != 2 || s.IsPending {
		t.Errorf("sitemap counters = %+v, want errors=0 warnings=2", s)
	}
}

func TestRankRows(t *testing.T) {
	rows := []Row{
		{Name: "zero-low", Impressions: 5},
		{Name: "clicked", Clicks: 1, Impressions: 2},
		{Name: "zero-high", Impressions: 500},
	}
	got := rankRows(rows, 2)
	if len(got) != 2 || got[0].Name != "clicked" || got[1].Name != "zero-high" {
		t.Errorf("rankRows = %+v, want clicked then zero-high", got)
	}
}

func TestFetchSectionFailureIsIsolated(t *testing.T) {
	f := newFakeGoogle(t)
	f.sitemapsFail = true
	c := newTestClient(t, f)

	res, err := c.Fetch(context.Background(), Query{Range: "7d"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Errors["sitemaps"] == "" {
		t.Errorf("errors = %v, want sitemaps entry", res.Errors)
	}
	if res.Summary == nil || len(res.TopQueries) == 0 {
		t.Error("healthy sections should survive a sitemaps failure")
	}
	if res.Sitemaps != nil {
		t.Errorf("sitemaps = %+v, want nil", res.Sitemaps)
	}
}

func TestFetchAllSectionsFailed(t *testing.T) {
	f := newFakeGoogle(t)
	f.tokenFail = true
	c := newTestClient(t, f)

	if _, err := c.Fetch(context.Background(), Query{Range: "28d"}); err == nil {
		t.Error("want error when every section failed")
	}
}

func TestAccessTokenIsCached(t *testing.T) {
	f := newFakeGoogle(t)
	c := newTestClient(t, f)

	for i := 0; i < 2; i++ {
		if _, err := c.Fetch(context.Background(), Query{Range: "7d"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.tokenCalls.Load(); got != 1 {
		t.Errorf("token exchanges = %d, want 1 (cached across sections + fetches)", got)
	}
}
