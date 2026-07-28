package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

// webstatsConfig is the optional web-analytics credentials file, read once at
// startup from webstats.json in the config dir. It powers the #/web view's two
// sections; a missing file just leaves both disabled — the endpoints answer
// 503 and the view shows setup hints instead of data.
//
//	{
//	  "cloudflare": {
//	    "zoneId": "…", "accountId": "…",
//	    "analyticsToken": "…",   // read-only Analytics token, never cache-purge
//	    "rumSiteTag": "…"        // optional: Web Analytics (RUM) site tag
//	  },
//	  "searchConsole": {
//	    "property": "sc-domain:example.com",
//	    "saKeyFile": "/path/to/service-account.json"
//	  }
//	}
type webstatsConfig struct {
	Cloudflare struct {
		ZoneID         string `json:"zoneId"`
		AccountID      string `json:"accountId"`
		AnalyticsToken string `json:"analyticsToken"`
		RumSiteTag     string `json:"rumSiteTag"`
	} `json:"cloudflare"`
	SearchConsole struct {
		Property  string `json:"property"`
		SAKeyFile string `json:"saKeyFile"`
	} `json:"searchConsole"`
	// Sites maps zone hostnames to the repos that ship them — the glue the
	// #/web view needs to follow the project scope and to mark releases on
	// the traffic chart. Optional and non-secret; unknown hosts simply stay
	// zone-wide. `"sites": [{"host": "a.example.com", "project": "repo-a"}]`
	Sites []WebSite `json:"sites"`
}

// WebSite is one hostname↔repo mapping entry (see webstatsConfig.Sites).
type WebSite struct {
	Host    string `json:"host"`
	Project string `json:"project"`
}

// loadWebstats never fails the boot: a malformed or unreadable file logs its
// reason and disables the sections, same convention as the index cache.
func loadWebstats(cfgDir string) webstatsConfig {
	var cfg webstatsConfig
	raw, err := os.ReadFile(filepath.Join(cfgDir, "webstats.json"))
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("webstats.json: %v", err)
		}
		return cfg
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Printf("webstats.json: %v", err)
	}
	return cfg
}
