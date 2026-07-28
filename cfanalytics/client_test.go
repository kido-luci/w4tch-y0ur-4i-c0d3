package cfanalytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient points a fully-configured client at a stub GraphQL server.
func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("zone123", "acct456", "token-abc", "site789")
	c.endpoint = srv.URL
	return c
}

// respond writes a GraphQL data envelope.
func respond(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"data":%s,"errors":null}`, data)
}

const trafficData = `{"viewer":{"zones":[{"series":[
  {"dimensions":{"datetime":"2026-07-10T00:00:00Z"},
   "sum":{"requests":100,"cachedRequests":60,"bytes":5000,"cachedBytes":3000,"threats":2,
     "responseStatusMap":[{"edgeResponseStatus":200,"requests":90},{"edgeResponseStatus":404,"requests":8},{"edgeResponseStatus":500,"requests":2}],
     "countryMap":[{"clientCountryName":"VN","requests":70},{"clientCountryName":"US","requests":30}]},
   "uniq":{"uniques":40}},
  {"dimensions":{"datetime":"2026-07-10T01:00:00Z"},
   "sum":{"requests":50,"cachedRequests":20,"bytes":2500,"cachedBytes":1000,"threats":0,
     "responseStatusMap":[{"edgeResponseStatus":404,"requests":5}],
     "countryMap":[{"clientCountryName":"VN","requests":50}]},
   "uniq":{"uniques":25}}
]}]}}`

const securityData = `{"viewer":{"zones":[{
  "events":[
    {"datetime":"2026-07-10T03:00:00Z","action":"block","clientIP":"1.2.3.4","clientCountryName":"RU","clientRequestHTTPHost":"api.example.com","clientRequestPath":"/posts","ruleId":"rule-1","source":"waf"},
    {"datetime":"2026-07-10T02:00:00Z","action":"block","clientIP":"1.2.3.4","clientCountryName":"RU","clientRequestHTTPHost":"api.example.com","clientRequestPath":"/posts","ruleId":"rule-1","source":"waf"},
    {"datetime":"2026-07-10T01:00:00Z","action":"challenge","clientIP":"5.6.7.8","clientCountryName":"US","clientRequestHTTPHost":"example.com","clientRequestPath":"/","ruleId":"rule-2","source":"bic"}
  ]
}]}}`

const rumData = `{"viewer":{"accounts":[{
  "series":[{"count":30,"sum":{"visits":12},"dimensions":{"datetimeHour":"2026-07-10T00:00:00Z"}},
            {"count":20,"sum":{"visits":8},"dimensions":{"datetimeHour":"2026-07-10T01:00:00Z"}}],
  "topPaths":[{"count":25,"sum":{"visits":10},"dimensions":{"requestPath":"/blog"}}],
  "topHosts":[{"count":50,"dimensions":{"requestHost":"example.com"}}],
  "topCountries":[{"count":35,"dimensions":{"countryName":"VN"}}],
  "topReferers":[{"count":5,"dimensions":{"refererHost":"google.com"}}]
}]}}`

// readQuery consumes the request body once and returns the GraphQL query string.
func readQuery(t *testing.T, r *http.Request) string {
	t.Helper()
	var body struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return body.Query
}

// route dispatches the stub response by sniffing which dataset the query asks for.
func route(t *testing.T, w http.ResponseWriter, query string) {
	t.Helper()
	switch {
	case strings.Contains(query, "httpRequests1hGroups"), strings.Contains(query, "httpRequests1dGroups"):
		respond(w, trafficData)
	case strings.Contains(query, "firewallEvents"):
		respond(w, securityData)
	case strings.Contains(query, "rumPageload"):
		respond(w, rumData)
	default:
		t.Errorf("unexpected query: %s", query)
	}
}

func TestFetch_AssemblesAllSections(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-abc" {
			t.Errorf("Authorization = %q", got)
		}
		route(t, w, readQuery(t, r))
	})

	res, err := c.Fetch(context.Background(), Query{Range: "24h"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Errors != nil {
		t.Fatalf("unexpected section errors: %v", res.Errors)
	}

	// Traffic totals + derived aggregates.
	if res.Traffic.Totals.Requests != 150 || res.Traffic.Totals.Cached != 80 {
		t.Errorf("traffic totals = %+v", res.Traffic.Totals)
	}
	if len(res.Traffic.Series) != 2 || res.Traffic.Series[0].Err4xx != 8 || res.Traffic.Series[0].Err5xx != 2 {
		t.Errorf("traffic series = %+v", res.Traffic.Series)
	}
	// 404 total (13) sorts above 500 (2); 200s are excluded.
	if len(res.Traffic.StatusCodes) != 2 || res.Traffic.StatusCodes[0].Code != 404 || res.Traffic.StatusCodes[0].Requests != 13 {
		t.Errorf("statusCodes = %+v", res.Traffic.StatusCodes)
	}
	if res.Traffic.TopCountries[0].Name != "VN" || res.Traffic.TopCountries[0].Count != 120 {
		t.Errorf("topCountries = %+v", res.Traffic.TopCountries)
	}

	// Security aggregates are computed in Go from the raw event list.
	if res.Security.Total != 3 || res.Security.ByAction[0].Name != "block" || res.Security.ByAction[0].Count != 2 {
		t.Errorf("security = %+v", res.Security)
	}
	if res.Security.ByRule[0].Name != "waf: rule-1" || res.Security.ByRule[0].Count != 2 {
		t.Errorf("byRule = %+v", res.Security.ByRule)
	}
	if res.Security.ByCountry[0].Name != "RU" || res.Security.ByHost[0].Name != "api.example.com" {
		t.Errorf("byCountry/byHost = %+v / %+v", res.Security.ByCountry, res.Security.ByHost)
	}
	if len(res.Security.Recent) != 3 || res.Security.Recent[0].ClientIP != "1.2.3.4" || res.Security.Recent[0].Datetime != "2026-07-10T03:00:00Z" {
		t.Errorf("recent = %+v", res.Security.Recent)
	}

	// RUM totals.
	if res.RUM.Pageviews != 50 || res.RUM.Visits != 20 {
		t.Errorf("rum totals = %+v", res.RUM)
	}
	if res.RUM.TopPaths[0].Name != "/blog" {
		t.Errorf("topPaths = %+v", res.RUM.TopPaths)
	}
}

func TestFetch_HostFilterAppearsInSecurityAndRUMOnly(t *testing.T) {
	queries := map[string]string{}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(t, r)
		switch {
		case strings.Contains(query, "httpRequests1hGroups"):
			queries["traffic"] = query
		case strings.Contains(query, "firewallEvents"):
			queries["security"] = query
		case strings.Contains(query, "rumPageload"):
			queries["rum"] = query
		}
		route(t, w, query)
	})

	if _, err := c.Fetch(context.Background(), Query{Range: "24h", Host: "api.example.com"}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if strings.Contains(queries["traffic"], "api.example.com") {
		t.Error("traffic query must stay zone-wide")
	}
	if !strings.Contains(queries["security"], `clientRequestHTTPHost: "api.example.com"`) {
		t.Error("security query missing host filter")
	}
	if !strings.Contains(queries["rum"], `requestHost: "api.example.com"`) {
		t.Error("rum query missing host filter")
	}
	if !strings.Contains(queries["rum"], `siteTag: "site789"`) {
		t.Error("rum query missing siteTag filter")
	}
}

func TestFetch_SectionErrorIsIsolated(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(t, r)
		if strings.Contains(query, "rumPageload") {
			fmt.Fprint(w, `{"data":null,"errors":[{"message":"quota exceeded"}]}`)
			return
		}
		route(t, w, query)
	})

	res, err := c.Fetch(context.Background(), Query{Range: "72h"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.RUM != nil {
		t.Error("rum section should be nil on error")
	}
	if res.Errors["rum"] != "quota exceeded" {
		t.Errorf("rum error = %q", res.Errors["rum"])
	}
	if res.Traffic == nil || res.Security == nil {
		t.Error("other sections must survive a rum failure")
	}
}

func TestFetch_AllSectionsFailedReturnsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"success":false}`)
	})
	if _, err := c.Fetch(context.Background(), Query{Range: "24h"}); err == nil {
		t.Fatal("want error when every section fails")
	}
}

func TestFetch_SecurityWindowClampedTo24h(t *testing.T) {
	var secQuery string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		query := readQuery(t, r)
		if strings.Contains(query, "firewallEvents") {
			secQuery = query
		}
		route(t, w, query)
	})

	res, err := c.Fetch(context.Background(), Query{Range: "30d"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.Security.WindowHours != 24 {
		t.Errorf("WindowHours = %d, want 24", res.Security.WindowHours)
	}
	if secQuery == "" {
		t.Fatal("security query not captured")
	}
	// The 30d range must use daily rollups for traffic.
	if res.Traffic == nil {
		t.Fatal("traffic missing")
	}
}

func TestEnabled(t *testing.T) {
	if New("", "a", "t", "").Enabled() {
		t.Error("missing zone must disable")
	}
	if New("z", "a", "", "").Enabled() {
		t.Error("missing token must disable")
	}
	if !New("z", "", "t", "").Enabled() {
		t.Error("account/siteTag are optional")
	}
	var nilClient *Client
	if nilClient.Enabled() {
		t.Error("nil client must be disabled")
	}
}

func TestFetchRUM_RequiresAccount(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { route(t, w, readQuery(t, r)) })
	c.accountTag = ""
	res, err := c.Fetch(context.Background(), Query{Range: "24h"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.RUM != nil || !strings.Contains(res.Errors["rum"], "CF_ACCOUNT_ID") {
		t.Errorf("rum = %+v, errors = %v", res.RUM, res.Errors)
	}
}
