// Package gsc fetches Google Search Console search-analytics data for the
// admin dashboard: clicks/impressions series, top queries/pages, a page×query
// drill-down, device and country splits, and sitemap status. Read-only;
// authenticates as a service
// account (OAuth2 JWT-bearer grant signed with golang-jwt, no SDK dependency)
// that is added as a Restricted user on the GSC property.
package gsc

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	tokenEndpoint = "https://oauth2.googleapis.com/token"
	apiEndpoint   = "https://searchconsole.googleapis.com/webmasters/v3"
	tokenScope    = "https://www.googleapis.com/auth/webmasters.readonly"
)

// GSC only exposes finalized data, which trails real time by about two days;
// every window ends this far back so the last buckets are never half-empty.
const dataLagDays = 2

// Client queries the Search Console API for one property. Construct via New;
// a client missing property or key config reports Enabled()==false and the
// handler answers 503 (same convention as cfanalytics / the CDN purger).
type Client struct {
	property string
	saEmail  string
	saKey    *rsa.PrivateKey
	tokenURL string
	apiBase  string
	client   *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New constructs a Client from the GSC property identifier (e.g.
// "sc-domain:example.com") and the path to the service-account JSON key
// file. An unset property or unreadable/invalid key file yields a disabled
// client; the parse failure is logged once here so a bad path doesn't turn
// into a silent 503.
func New(property, keyFile string) *Client {
	c := &Client{
		property: property,
		tokenURL: tokenEndpoint,
		apiBase:  apiEndpoint,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
	if property == "" || keyFile == "" {
		return c
	}

	raw, err := os.ReadFile(keyFile)
	if err != nil {
		log.Printf("[gsc] cannot read GSC_SA_KEY_FILE: %v", err)
		return c
	}
	var sa struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(raw, &sa); err != nil {
		log.Printf("[gsc] cannot parse GSC_SA_KEY_FILE: %v", err)
		return c
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(sa.PrivateKey))
	if err != nil || sa.ClientEmail == "" {
		log.Printf("[gsc] invalid service-account key (email=%q): %v", sa.ClientEmail, err)
		return c
	}
	c.saEmail = sa.ClientEmail
	c.saKey = key
	return c
}

// Enabled reports whether the client has a property and a parsed key.
func (c *Client) Enabled() bool {
	return c != nil && c.property != "" && c.saKey != nil
}

// Query is the validated request from the handler. Range is one of
// "7d" | "28d" | "90d".
type Query struct {
	Range string
}

// Row is one search-analytics aggregate (query, page, device, country, host).
type Row struct {
	Name        string  `json:"name"`
	Clicks      uint64  `json:"clicks"`
	Impressions uint64  `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// DayPoint is one day of the clicks/impressions series.
type DayPoint struct {
	Date        string  `json:"date"`
	Clicks      uint64  `json:"clicks"`
	Impressions uint64  `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// Summary is the window total; Position is impression-weighted.
type Summary struct {
	Clicks      uint64  `json:"clicks"`
	Impressions uint64  `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// QueryPage is one page×query aggregate — which queries a page appears for,
// the input for title/meta work that the separate query and page lists can't
// answer.
type QueryPage struct {
	Page        string  `json:"page"`
	Query       string  `json:"query"`
	Clicks      uint64  `json:"clicks"`
	Impressions uint64  `json:"impressions"`
	CTR         float64 `json:"ctr"`
	Position    float64 `json:"position"`
}

// Sitemap is one submitted sitemap's processing status.
type Sitemap struct {
	Path           string `json:"path"`
	LastSubmitted  string `json:"lastSubmitted"`
	LastDownloaded string `json:"lastDownloaded"`
	IsPending      bool   `json:"isPending"`
	Errors         uint64 `json:"errors"`
	Warnings       uint64 `json:"warnings"`
}

// Result is the assembled response for the admin dashboard. Sections are
// independent: a failed section is nil with its reason under Errors, so one
// unavailable dataset never blanks the whole page (cfanalytics convention).
type Result struct {
	Range      string            `json:"range"`
	Property   string            `json:"property"`
	StartDate  string            `json:"startDate"`
	EndDate    string            `json:"endDate"`
	Summary    *Summary          `json:"summary"`
	Series     []DayPoint        `json:"series"`
	TopQueries []Row             `json:"topQueries"`
	TopPages   []Row             `json:"topPages"`
	QueryPages []QueryPage       `json:"queryPages"`
	ByHost     []Row             `json:"byHost"`
	Devices    []Row             `json:"devices"`
	Countries  []Row             `json:"countries"`
	Sitemaps   []Sitemap         `json:"sitemaps"`
	Errors     map[string]string `json:"errors,omitempty"`
}

// rangeWindow resolves a validated range name to its date window, ending
// dataLagDays back from now.
func rangeWindow(rng string, now time.Time) (start, end time.Time) {
	end = now.AddDate(0, 0, -dataLagDays)
	days := 28
	switch rng {
	case "7d":
		days = 7
	case "90d":
		days = 90
	}
	return end.AddDate(0, 0, -(days - 1)), end
}

// Fetch runs the section queries concurrently and assembles the result.
// It returns an error only when every section failed (e.g. bad key).
func (c *Client) Fetch(ctx context.Context, q Query) (*Result, error) {
	start, end := rangeWindow(q.Range, time.Now().UTC())
	res := &Result{
		Range:     q.Range,
		Property:  c.property,
		StartDate: start.Format("2006-01-02"),
		EndDate:   end.Format("2006-01-02"),
		Errors:    map[string]string{},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	fail := func(section string, err error) {
		mu.Lock()
		res.Errors[section] = err.Error()
		mu.Unlock()
	}

	// Displayed pages are capped lower than the fetch: the extra rows only
	// feed the by-host aggregation (the API has no host dimension, so the
	// domain property's subdomain split is derived from page URLs here).
	const pageFetchLimit, pageShowLimit = 100, 25
	// Queries and page×query rows are also fetched wider than shown: the API
	// only orders by clicks, so while clicks are scarce most rows are ties in
	// arbitrary order — rankRows re-breaks them by impressions before the trim.
	const queryFetchLimit, queryShowLimit = 100, 25
	const queryPageFetchLimit, queryPageShowLimit = 250, 100

	sections := []struct {
		name string
		run  func() error
	}{
		{"series", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"date"}, res.StartDate, res.EndDate, 500)
			if err != nil {
				return err
			}
			series := make([]DayPoint, 0, len(rows))
			sum := Summary{}
			var posWeight float64
			for _, r := range rows {
				series = append(series, DayPoint{
					Date: r.key(), Clicks: r.clicks(), Impressions: r.impressions(),
					CTR: r.CTR, Position: r.Position,
				})
				sum.Clicks += r.clicks()
				sum.Impressions += r.impressions()
				posWeight += r.Position * r.ImpressionsF
			}
			if sum.Impressions > 0 {
				sum.CTR = float64(sum.Clicks) / float64(sum.Impressions)
				sum.Position = posWeight / float64(sum.Impressions)
			}
			res.Series = series
			res.Summary = &sum
			return nil
		}},
		{"queries", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"query"}, res.StartDate, res.EndDate, queryFetchLimit)
			if err != nil {
				return err
			}
			res.TopQueries = rankRows(toRows(rows), queryShowLimit)
			return nil
		}},
		{"queryPages", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"page", "query"}, res.StartDate, res.EndDate, queryPageFetchLimit)
			if err != nil {
				return err
			}
			qp := make([]QueryPage, 0, len(rows))
			for _, r := range rows {
				var page, query string
				if len(r.Keys) > 1 {
					page, query = r.Keys[0], r.Keys[1]
				}
				qp = append(qp, QueryPage{
					Page: page, Query: query, Clicks: r.clicks(), Impressions: r.impressions(),
					CTR: r.CTR, Position: r.Position,
				})
			}
			sort.SliceStable(qp, func(i, j int) bool {
				if qp[i].Clicks != qp[j].Clicks {
					return qp[i].Clicks > qp[j].Clicks
				}
				return qp[i].Impressions > qp[j].Impressions
			})
			if len(qp) > queryPageShowLimit {
				qp = qp[:queryPageShowLimit]
			}
			res.QueryPages = qp
			return nil
		}},
		{"pages", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"page"}, res.StartDate, res.EndDate, pageFetchLimit)
			if err != nil {
				return err
			}
			pages := toRows(rows)
			res.ByHost = aggregateHosts(pages, 10)
			if len(pages) > pageShowLimit {
				pages = pages[:pageShowLimit]
			}
			res.TopPages = pages
			return nil
		}},
		{"devices", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"device"}, res.StartDate, res.EndDate, 10)
			if err != nil {
				return err
			}
			res.Devices = toRows(rows)
			return nil
		}},
		{"countries", func() error {
			rows, err := c.searchAnalytics(ctx, []string{"country"}, res.StartDate, res.EndDate, 10)
			if err != nil {
				return err
			}
			res.Countries = toRows(rows)
			return nil
		}},
		{"sitemaps", func() error {
			maps, err := c.sitemaps(ctx)
			if err != nil {
				return err
			}
			res.Sitemaps = maps
			return nil
		}},
	}

	wg.Add(len(sections))
	for _, s := range sections {
		go func(name string, run func() error) {
			defer wg.Done()
			if err := run(); err != nil {
				fail(name, err)
			}
		}(s.name, s.run)
	}
	wg.Wait()

	if len(res.Errors) == len(sections) {
		return nil, fmt.Errorf("all sections failed: %v", res.Errors)
	}
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	return res, nil
}

// ---- OAuth2 JWT-bearer token ------------------------------------------------

// accessToken returns a cached service-account access token, minting a new one
// when fewer than five minutes remain. The mutex intentionally spans the
// exchange so concurrent section fetches on a cold cache mint one token, not
// six.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp.Add(-5*time.Minute)) {
		return c.token, nil
	}

	now := time.Now()
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss":   c.saEmail,
		"scope": tokenScope,
		"aud":   c.tokenURL,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}).SignedString(c.saKey)
	if err != nil {
		return "", fmt.Errorf("sign assertion: %w", err)
	}

	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token exchange HTTP %d: %s", resp.StatusCode, excerpt)
	}

	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("token response missing access_token")
	}
	c.token = tok.AccessToken
	c.tokenExp = now.Add(time.Duration(tok.ExpiresIn) * time.Second)
	return c.token, nil
}

// ---- Search Console API plumbing ---------------------------------------------

// saRow is one searchAnalytics.query response row. Clicks/impressions arrive
// as JSON doubles; keep the float impressions for weighted-position math.
type saRow struct {
	Keys         []string `json:"keys"`
	ClicksF      float64  `json:"clicks"`
	ImpressionsF float64  `json:"impressions"`
	CTR          float64  `json:"ctr"`
	Position     float64  `json:"position"`
}

func (r saRow) key() string {
	if len(r.Keys) > 0 {
		return r.Keys[0]
	}
	return ""
}
func (r saRow) clicks() uint64      { return uint64(r.ClicksF) }
func (r saRow) impressions() uint64 { return uint64(r.ImpressionsF) }

// searchAnalytics runs one searchanalytics.query call. Rows are grouped by
// the given dimensions, keyed in the same order; Google always sorts them by
// clicks descending (the API has no sort parameter).
func (c *Client) searchAnalytics(ctx context.Context, dimensions []string, start, end string, limit int) ([]saRow, error) {
	body, err := json.Marshal(map[string]any{
		"startDate":  start,
		"endDate":    end,
		"dimensions": dimensions,
		"rowLimit":   limit,
	})
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/sites/%s/searchAnalytics/query",
		c.apiBase, url.PathEscape(c.property))
	var data struct {
		Rows []saRow `json:"rows"`
	}
	if err := c.exec(ctx, http.MethodPost, endpoint, bytes.NewReader(body), &data); err != nil {
		return nil, err
	}
	return data.Rows, nil
}

// sitemaps lists the property's submitted sitemaps. The API encodes int64
// counters (errors/warnings) as JSON strings.
func (c *Client) sitemaps(ctx context.Context) ([]Sitemap, error) {
	endpoint := fmt.Sprintf("%s/sites/%s/sitemaps", c.apiBase, url.PathEscape(c.property))
	var data struct {
		Sitemap []struct {
			Path           string `json:"path"`
			LastSubmitted  string `json:"lastSubmitted"`
			LastDownloaded string `json:"lastDownloaded"`
			IsPending      bool   `json:"isPending"`
			Errors         string `json:"errors"`
			Warnings       string `json:"warnings"`
		} `json:"sitemap"`
	}
	if err := c.exec(ctx, http.MethodGet, endpoint, nil, &data); err != nil {
		return nil, err
	}

	out := make([]Sitemap, 0, len(data.Sitemap))
	for _, s := range data.Sitemap {
		errs, _ := strconv.ParseUint(s.Errors, 10, 64)
		warns, _ := strconv.ParseUint(s.Warnings, 10, 64)
		out = append(out, Sitemap{
			Path:           s.Path,
			LastSubmitted:  s.LastSubmitted,
			LastDownloaded: s.LastDownloaded,
			IsPending:      s.IsPending,
			Errors:         errs,
			Warnings:       warns,
		})
	}
	return out, nil
}

// exec sends one authenticated API request and unmarshals the JSON body.
func (c *Client) exec(ctx context.Context, method, endpoint string, body io.Reader, out any) error {
	token, err := c.accessToken(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("search console HTTP %d: %s", resp.StatusCode, excerpt)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// ---- aggregation helpers ------------------------------------------------------

func toRows(rows []saRow) []Row {
	out := make([]Row, 0, len(rows))
	for _, r := range rows {
		out = append(out, Row{
			Name: r.key(), Clicks: r.clicks(), Impressions: r.impressions(),
			CTR: r.CTR, Position: r.Position,
		})
	}
	return out
}

// rankRows re-sorts rows clicks-first with impressions breaking the ties —
// the API's own clicks-only ordering leaves the zero-click tail (most of the
// list while clicks are scarce) in arbitrary order — then trims to n. Clicked
// rows stay pinned on top so a low-impression winner never drops off the list.
func rankRows(rows []Row, n int) []Row {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Clicks != rows[j].Clicks {
			return rows[i].Clicks > rows[j].Clicks
		}
		return rows[i].Impressions > rows[j].Impressions
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// aggregateHosts folds page rows into per-host clicks/impressions (CTR
// derived; position left zero — it can't be meaningfully averaged across a
// truncated page list).
func aggregateHosts(pages []Row, n int) []Row {
	type agg struct{ clicks, impressions uint64 }
	hosts := map[string]agg{}
	for _, p := range pages {
		u, err := url.Parse(p.Name)
		if err != nil || u.Host == "" {
			continue
		}
		a := hosts[u.Host]
		a.clicks += p.Clicks
		a.impressions += p.Impressions
		hosts[u.Host] = a
	}

	out := make([]Row, 0, len(hosts))
	for h, a := range hosts {
		r := Row{Name: h, Clicks: a.clicks, Impressions: a.impressions}
		if a.impressions > 0 {
			r.CTR = float64(a.clicks) / float64(a.impressions)
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Clicks != out[j].Clicks {
			return out[i].Clicks > out[j].Clicks
		}
		if out[i].Impressions != out[j].Impressions {
			return out[i].Impressions > out[j].Impressions
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}
