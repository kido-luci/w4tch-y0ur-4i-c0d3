// Package cfanalytics fetches zone/account analytics from the Cloudflare
// GraphQL API for the admin dashboard: traffic rollups, firewall (security)
// events, and Web Analytics (RUM) pageviews. Read-only; uses a dedicated
// analytics token, never the cache-purge token.
package cfanalytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

const graphqlEndpoint = "https://api.cloudflare.com/client/v4/graphql"

// Free-plan zones may not query firewall events over a window wider than one
// day (the GraphQL API rejects it with "cannot request a time range wider than
// 1d"), so the security section is always clamped to this window.
const maxSecurityWindow = 24 * time.Hour

// Client queries the Cloudflare GraphQL Analytics API. Construct via New; a
// client missing zone or token config reports Enabled()==false and the handler
// answers 503 (same convention as the CDN purger / usage ingest).
type Client struct {
	zoneTag    string
	accountTag string
	apiToken   string
	rumSiteTag string
	endpoint   string
	client     *http.Client
}

// New constructs a Client. accountTag and rumSiteTag are only needed for the
// RUM (Web Analytics) section; when accountTag is empty that section reports a
// config error while traffic + security still work.
func New(zoneTag, accountTag, apiToken, rumSiteTag string) *Client {
	return &Client{
		zoneTag:    zoneTag,
		accountTag: accountTag,
		apiToken:   apiToken,
		rumSiteTag: rumSiteTag,
		endpoint:   graphqlEndpoint,
		client:     &http.Client{Timeout: 15 * time.Second},
	}
}

// Enabled reports whether the client has the minimum config (zone + token).
func (c *Client) Enabled() bool {
	return c != nil && c.zoneTag != "" && c.apiToken != ""
}

// Query is the validated request from the handler. Range is one of
// "24h" | "72h" | "30d"; Host optionally narrows the security + RUM sections
// to one hostname (traffic rollups are always zone-wide).
type Query struct {
	Range string
	Host  string
}

// TimePoint is one bucket (hour or day) of the traffic series.
type TimePoint struct {
	TS       string `json:"ts"` // RFC3339 hour or YYYY-MM-DD day
	Requests uint64 `json:"requests"`
	Cached   uint64 `json:"cached"`
	Bytes    uint64 `json:"bytes"`
	Err4xx   uint64 `json:"err4xx"`
	Err5xx   uint64 `json:"err5xx"`
	Uniques  uint64 `json:"uniques"`
	Threats  uint64 `json:"threats"`
}

// NameCount is a generic top-N row (country, host, rule, path, ...).
type NameCount struct {
	Name  string `json:"name"`
	Count uint64 `json:"count"`
}

// CodeCount is one HTTP status code aggregate.
type CodeCount struct {
	Code     int    `json:"code"`
	Requests uint64 `json:"requests"`
}

// Traffic is the zone-wide requests/bandwidth/errors section.
type Traffic struct {
	Totals struct {
		Requests    uint64 `json:"requests"`
		Cached      uint64 `json:"cached"`
		Bytes       uint64 `json:"bytes"`
		CachedBytes uint64 `json:"cachedBytes"`
		Uniques     uint64 `json:"uniques"` // sum of per-bucket uniques — an approximation
		Threats     uint64 `json:"threats"`
	} `json:"totals"`
	Series       []TimePoint `json:"series"`
	StatusCodes  []CodeCount `json:"statusCodes"` // ≥400 only, top 10
	TopCountries []NameCount `json:"topCountries"`
}

// SecEvent is one raw firewall event row.
type SecEvent struct {
	Datetime string `json:"datetime"`
	Action   string `json:"action"`
	ClientIP string `json:"clientIP"`
	Country  string `json:"country"`
	Host     string `json:"host"`
	Path     string `json:"path"`
	RuleID   string `json:"ruleId"`
	Source   string `json:"source"`
}

// Security is the firewall-events section.
type Security struct {
	WindowHours int         `json:"windowHours"`
	Total       uint64      `json:"total"`
	ByAction    []NameCount `json:"byAction"`
	ByCountry   []NameCount `json:"byCountry"`
	ByHost      []NameCount `json:"byHost"`
	ByRule      []NameCount `json:"byRule"`
	Recent      []SecEvent  `json:"recent"`
}

// RUMPoint is one bucket of the Web Analytics series.
type RUMPoint struct {
	TS        string `json:"ts"`
	Pageviews uint64 `json:"pageviews"`
	Visits    uint64 `json:"visits"`
}

// RUM is the Web Analytics (browser pageview) section.
type RUM struct {
	Pageviews    uint64      `json:"pageviews"`
	Visits       uint64      `json:"visits"`
	Series       []RUMPoint  `json:"series"`
	TopPaths     []NameCount `json:"topPaths"`
	TopHosts     []NameCount `json:"topHosts"`
	TopCountries []NameCount `json:"topCountries"`
	TopReferers  []NameCount `json:"topReferers"`
}

// Result is the assembled response for the admin dashboard. Sections are
// independent: a nil section has its failure reason under Errors, so one
// unavailable dataset (e.g. retention exceeded) never blanks the whole page.
type Result struct {
	Range    string            `json:"range"`
	Host     string            `json:"host,omitempty"`
	Traffic  *Traffic          `json:"traffic"`
	Security *Security         `json:"security"`
	RUM      *RUM              `json:"rum"`
	Errors   map[string]string `json:"errors,omitempty"`
}

// Fetch runs the three section queries concurrently and assembles the result.
// It returns an error only when every section failed (e.g. bad token).
func (c *Client) Fetch(ctx context.Context, q Query) (*Result, error) {
	res := &Result{Range: q.Range, Host: q.Host, Errors: map[string]string{}}

	var wg sync.WaitGroup
	var mu sync.Mutex
	fail := func(section string, err error) {
		mu.Lock()
		res.Errors[section] = err.Error()
		mu.Unlock()
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		t, err := c.fetchTraffic(ctx, q)
		if err != nil {
			fail("traffic", err)
			return
		}
		res.Traffic = t
	}()
	go func() {
		defer wg.Done()
		s, err := c.fetchSecurity(ctx, q)
		if err != nil {
			fail("security", err)
			return
		}
		res.Security = s
	}()
	go func() {
		defer wg.Done()
		r, err := c.fetchRUM(ctx, q)
		if err != nil {
			fail("rum", err)
			return
		}
		res.RUM = r
	}()
	wg.Wait()

	if res.Traffic == nil && res.Security == nil && res.RUM == nil {
		return nil, fmt.Errorf("all sections failed: %v", res.Errors)
	}
	if len(res.Errors) == 0 {
		res.Errors = nil
	}
	return res, nil
}

// rangeWindow resolves a validated range name to its time window and whether
// the traffic/RUM series use hourly buckets (daily otherwise).
func rangeWindow(rng string, now time.Time) (since, until time.Time, hourly bool) {
	until = now
	switch rng {
	case "72h":
		return now.Add(-72 * time.Hour), until, true
	case "30d":
		return now.AddDate(0, 0, -29), until, false
	default: // "24h"
		return now.Add(-24 * time.Hour), until, true
	}
}

// ---- GraphQL plumbing ----------------------------------------------------

// exec POSTs one GraphQL query and unmarshals the `data` object into out.
// Filter values are embedded as literals: every value is either produced here
// (timestamps, env-configured tags) or validated by the handler (hostname), so
// variable plumbing for Cloudflare's per-dataset filter input types is not
// worth its fragility.
func (c *Client) exec(ctx context.Context, query string, out any) error {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		excerpt, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("cloudflare HTTP %d: %s", resp.StatusCode, excerpt)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode cloudflare response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return errors.New(envelope.Errors[0].Message)
	}
	if out != nil && len(envelope.Data) > 0 {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func topN(m map[string]uint64, n int) []NameCount {
	out := make([]NameCount, 0, len(m))
	for k, v := range m {
		out = append(out, NameCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// ---- Traffic (zone HTTP rollups) ------------------------------------------

type trafficGroup struct {
	Dimensions struct {
		Datetime string `json:"datetime"`
		Date     string `json:"date"`
	} `json:"dimensions"`
	Sum struct {
		Requests          uint64 `json:"requests"`
		CachedRequests    uint64 `json:"cachedRequests"`
		Bytes             uint64 `json:"bytes"`
		CachedBytes       uint64 `json:"cachedBytes"`
		Threats           uint64 `json:"threats"`
		ResponseStatusMap []struct {
			EdgeResponseStatus int    `json:"edgeResponseStatus"`
			Requests           uint64 `json:"requests"`
		} `json:"responseStatusMap"`
		CountryMap []struct {
			ClientCountryName string `json:"clientCountryName"`
			Requests          uint64 `json:"requests"`
		} `json:"countryMap"`
	} `json:"sum"`
	Uniq struct {
		Uniques uint64 `json:"uniques"`
	} `json:"uniq"`
}

func (c *Client) fetchTraffic(ctx context.Context, q Query) (*Traffic, error) {
	since, until, hourly := rangeWindow(q.Range, time.Now().UTC())

	var query string
	if hourly {
		query = fmt.Sprintf(`{
  viewer {
    zones(filter: { zoneTag: "%s" }) {
      series: httpRequests1hGroups(
        limit: 100
        orderBy: [datetime_ASC]
        filter: { datetime_geq: "%s", datetime_leq: "%s" }
      ) {
        dimensions { datetime }
        sum {
          requests cachedRequests bytes cachedBytes threats
          responseStatusMap { edgeResponseStatus requests }
          countryMap { clientCountryName requests }
        }
        uniq { uniques }
      }
    }
  }
}`, c.zoneTag, since.Format(time.RFC3339), until.Format(time.RFC3339))
	} else {
		query = fmt.Sprintf(`{
  viewer {
    zones(filter: { zoneTag: "%s" }) {
      series: httpRequests1dGroups(
        limit: 40
        orderBy: [date_ASC]
        filter: { date_geq: "%s", date_leq: "%s" }
      ) {
        dimensions { date }
        sum {
          requests cachedRequests bytes cachedBytes threats
          responseStatusMap { edgeResponseStatus requests }
          countryMap { clientCountryName requests }
        }
        uniq { uniques }
      }
    }
  }
}`, c.zoneTag, since.Format("2006-01-02"), until.Format("2006-01-02"))
	}

	var data struct {
		Viewer struct {
			Zones []struct {
				Series []trafficGroup `json:"series"`
			} `json:"zones"`
		} `json:"viewer"`
	}
	if err := c.exec(ctx, query, &data); err != nil {
		return nil, err
	}
	if len(data.Viewer.Zones) == 0 {
		return nil, errors.New("zone not found (check CF_ZONE_ID / token zone scope)")
	}

	t := &Traffic{Series: []TimePoint{}}
	statusAgg := map[int]uint64{}
	countryAgg := map[string]uint64{}
	for _, g := range data.Viewer.Zones[0].Series {
		p := TimePoint{
			Requests: g.Sum.Requests,
			Cached:   g.Sum.CachedRequests,
			Bytes:    g.Sum.Bytes,
			Uniques:  g.Uniq.Uniques,
			Threats:  g.Sum.Threats,
		}
		if g.Dimensions.Datetime != "" {
			p.TS = g.Dimensions.Datetime
		} else {
			p.TS = g.Dimensions.Date
		}
		for _, s := range g.Sum.ResponseStatusMap {
			switch {
			case s.EdgeResponseStatus >= 500:
				p.Err5xx += s.Requests
			case s.EdgeResponseStatus >= 400:
				p.Err4xx += s.Requests
			}
			if s.EdgeResponseStatus >= 400 {
				statusAgg[s.EdgeResponseStatus] += s.Requests
			}
		}
		for _, cm := range g.Sum.CountryMap {
			countryAgg[cm.ClientCountryName] += cm.Requests
		}
		t.Totals.Requests += g.Sum.Requests
		t.Totals.Cached += g.Sum.CachedRequests
		t.Totals.Bytes += g.Sum.Bytes
		t.Totals.CachedBytes += g.Sum.CachedBytes
		t.Totals.Uniques += g.Uniq.Uniques
		t.Totals.Threats += g.Sum.Threats
		t.Series = append(t.Series, p)
	}

	t.StatusCodes = []CodeCount{}
	for code, n := range statusAgg {
		t.StatusCodes = append(t.StatusCodes, CodeCount{Code: code, Requests: n})
	}
	sort.Slice(t.StatusCodes, func(i, j int) bool {
		if t.StatusCodes[i].Requests != t.StatusCodes[j].Requests {
			return t.StatusCodes[i].Requests > t.StatusCodes[j].Requests
		}
		return t.StatusCodes[i].Code < t.StatusCodes[j].Code
	})
	if len(t.StatusCodes) > 10 {
		t.StatusCodes = t.StatusCodes[:10]
	}
	t.TopCountries = topN(countryAgg, 10)
	return t, nil
}

// ---- Security (firewall events) --------------------------------------------

// maxSecurityEvents caps the raw-event fetch the aggregates are computed from.
const maxSecurityEvents = 1000

// fetchSecurity reads the RAW firewallEventsAdaptive list and aggregates it
// here. The pre-aggregated firewallEventsAdaptiveGroups dataset is NOT plan-
// accessible on Free zones (authz-denied even with Firewall Services Read,
// verified live 2026-07-10), while the raw list works with Analytics Read +
// Firewall Services Read.
func (c *Client) fetchSecurity(ctx context.Context, q Query) (*Security, error) {
	now := time.Now().UTC()
	since, until, _ := rangeWindow(q.Range, now)
	if until.Sub(since) > maxSecurityWindow {
		since = until.Add(-maxSecurityWindow)
	}

	hostClause := ""
	if q.Host != "" {
		hostClause = fmt.Sprintf(`, clientRequestHTTPHost: "%s"`, q.Host)
	}
	filter := fmt.Sprintf(`{ datetime_geq: "%s", datetime_leq: "%s"%s }`,
		since.Format(time.RFC3339), until.Format(time.RFC3339), hostClause)

	query := fmt.Sprintf(`{
  viewer {
    zones(filter: { zoneTag: "%s" }) {
      events: firewallEventsAdaptive(limit: %d, orderBy: [datetime_DESC], filter: %s) {
        datetime action clientIP clientCountryName clientRequestHTTPHost clientRequestPath ruleId source
      }
    }
  }
}`, c.zoneTag, maxSecurityEvents, filter)

	var data struct {
		Viewer struct {
			Zones []struct {
				Events []struct {
					Datetime              string `json:"datetime"`
					Action                string `json:"action"`
					ClientIP              string `json:"clientIP"`
					ClientCountryName     string `json:"clientCountryName"`
					ClientRequestHTTPHost string `json:"clientRequestHTTPHost"`
					ClientRequestPath     string `json:"clientRequestPath"`
					RuleID                string `json:"ruleId"`
					Source                string `json:"source"`
				} `json:"events"`
			} `json:"zones"`
		} `json:"viewer"`
	}
	if err := c.exec(ctx, query, &data); err != nil {
		return nil, err
	}
	if len(data.Viewer.Zones) == 0 {
		return nil, errors.New("zone not found (check CF_ZONE_ID / token zone scope)")
	}

	events := data.Viewer.Zones[0].Events
	s := &Security{
		WindowHours: int(until.Sub(since) / time.Hour),
		Total:       uint64(len(events)),
		Recent:      []SecEvent{},
	}
	actionAgg := map[string]uint64{}
	countryAgg := map[string]uint64{}
	hostAgg := map[string]uint64{}
	ruleAgg := map[string]uint64{}
	for i, e := range events {
		actionAgg[e.Action]++
		countryAgg[e.ClientCountryName]++
		hostAgg[e.ClientRequestHTTPHost]++
		rule := e.RuleID
		if e.Source != "" {
			rule = e.Source + ": " + e.RuleID
		}
		ruleAgg[rule]++
		if i < 25 { // events arrive newest-first
			s.Recent = append(s.Recent, SecEvent{
				Datetime: e.Datetime,
				Action:   e.Action,
				ClientIP: e.ClientIP,
				Country:  e.ClientCountryName,
				Host:     e.ClientRequestHTTPHost,
				Path:     e.ClientRequestPath,
				RuleID:   e.RuleID,
				Source:   e.Source,
			})
		}
	}
	s.ByAction = topN(actionAgg, 10)
	s.ByCountry = topN(countryAgg, 8)
	s.ByHost = topN(hostAgg, 8)
	s.ByRule = topN(ruleAgg, 8)
	return s, nil
}

// ---- RUM (Web Analytics pageviews) ------------------------------------------

type rumGroup struct {
	Count uint64 `json:"count"`
	Sum   struct {
		Visits uint64 `json:"visits"`
	} `json:"sum"`
	Dimensions struct {
		DatetimeHour string `json:"datetimeHour"`
		Date         string `json:"date"`
		RequestPath  string `json:"requestPath"`
		RequestHost  string `json:"requestHost"`
		CountryName  string `json:"countryName"`
		RefererHost  string `json:"refererHost"`
	} `json:"dimensions"`
}

func (c *Client) fetchRUM(ctx context.Context, q Query) (*RUM, error) {
	if c.accountTag == "" {
		return nil, errors.New("CF_ACCOUNT_ID not set")
	}
	since, until, hourly := rangeWindow(q.Range, time.Now().UTC())

	extra := ""
	if c.rumSiteTag != "" {
		extra += fmt.Sprintf(`, siteTag: "%s"`, c.rumSiteTag)
	}
	if q.Host != "" {
		extra += fmt.Sprintf(`, requestHost: "%s"`, q.Host)
	}
	filter := fmt.Sprintf(`{ datetime_geq: "%s", datetime_leq: "%s"%s }`,
		since.Format(time.RFC3339), until.Format(time.RFC3339), extra)

	seriesDim, seriesOrder := "date", "date_ASC"
	if hourly {
		seriesDim, seriesOrder = "datetimeHour", "datetimeHour_ASC"
	}

	query := fmt.Sprintf(`{
  viewer {
    accounts(filter: { accountTag: "%s" }) {
      series: rumPageloadEventsAdaptiveGroups(limit: 100, orderBy: [%s], filter: %s) {
        count
        sum { visits }
        dimensions { %s }
      }
      topPaths: rumPageloadEventsAdaptiveGroups(limit: 10, orderBy: [count_DESC], filter: %s) {
        count
        sum { visits }
        dimensions { requestPath }
      }
      topHosts: rumPageloadEventsAdaptiveGroups(limit: 8, orderBy: [count_DESC], filter: %s) {
        count
        dimensions { requestHost }
      }
      topCountries: rumPageloadEventsAdaptiveGroups(limit: 8, orderBy: [count_DESC], filter: %s) {
        count
        dimensions { countryName }
      }
      topReferers: rumPageloadEventsAdaptiveGroups(limit: 8, orderBy: [count_DESC], filter: %s) {
        count
        dimensions { refererHost }
      }
    }
  }
}`, c.accountTag, seriesOrder, filter, seriesDim, filter, filter, filter, filter)

	var data struct {
		Viewer struct {
			Accounts []struct {
				Series       []rumGroup `json:"series"`
				TopPaths     []rumGroup `json:"topPaths"`
				TopHosts     []rumGroup `json:"topHosts"`
				TopCountries []rumGroup `json:"topCountries"`
				TopReferers  []rumGroup `json:"topReferers"`
			} `json:"accounts"`
		} `json:"viewer"`
	}
	if err := c.exec(ctx, query, &data); err != nil {
		return nil, err
	}
	if len(data.Viewer.Accounts) == 0 {
		return nil, errors.New("account not found (check CF_ACCOUNT_ID / token account scope)")
	}

	a := data.Viewer.Accounts[0]
	r := &RUM{
		Series:       []RUMPoint{},
		TopPaths:     []NameCount{},
		TopHosts:     []NameCount{},
		TopCountries: []NameCount{},
		TopReferers:  []NameCount{},
	}
	for _, g := range a.Series {
		ts := g.Dimensions.Date
		if g.Dimensions.DatetimeHour != "" {
			ts = g.Dimensions.DatetimeHour
		}
		r.Pageviews += g.Count
		r.Visits += g.Sum.Visits
		r.Series = append(r.Series, RUMPoint{TS: ts, Pageviews: g.Count, Visits: g.Sum.Visits})
	}
	for _, g := range a.TopPaths {
		r.TopPaths = append(r.TopPaths, NameCount{Name: g.Dimensions.RequestPath, Count: g.Count})
	}
	for _, g := range a.TopHosts {
		r.TopHosts = append(r.TopHosts, NameCount{Name: g.Dimensions.RequestHost, Count: g.Count})
	}
	for _, g := range a.TopCountries {
		r.TopCountries = append(r.TopCountries, NameCount{Name: g.Dimensions.CountryName, Count: g.Count})
	}
	for _, g := range a.TopReferers {
		r.TopReferers = append(r.TopReferers, NameCount{Name: g.Dimensions.RefererHost, Count: g.Count})
	}
	return r, nil
}
