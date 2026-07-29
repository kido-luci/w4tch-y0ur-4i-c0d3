package index

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// sessionStoreDir is where the Claude Code app records per-session metadata,
// including each session's archived flag. This lives outside the transcripts
// root (which the rest of the viewer reads) — it's the only place the
// active/archived status exists. macOS path; absent on other platforms, or if
// the app has never run.
func sessionStoreDir() string {
	return filepath.Join(os.Getenv("HOME"),
		"Library", "Application Support", "Claude", "claude-code-sessions")
}

// loadActiveIDs reads the app's session store and returns the set of session
// IDs the app currently treats as active (present with isArchived=false).
//
// The second result is false when the store is absent or holds no entries, in
// which case archived status is simply unknown — callers must then treat every
// session as active rather than hiding everything ("no entry = archived" only
// applies on a machine where the store actually exists).
func loadActiveIDs() (map[string]bool, bool) {
	// Files live at <store>/<account>/<device>/local_<uuid>.json; the app keys
	// each by cliSessionId, which equals the transcript's <uuid>.jsonl basename.
	matches, err := filepath.Glob(
		filepath.Join(sessionStoreDir(), "*", "*", "local_*.json"))
	if err != nil || len(matches) == 0 {
		return nil, false
	}
	active := make(map[string]bool, len(matches))
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var e struct {
			CLISessionID string `json:"cliSessionId"`
			IsArchived   bool   `json:"isArchived"`
		}
		if json.Unmarshal(b, &e) != nil || e.CLISessionID == "" {
			continue
		}
		if !e.IsArchived {
			active[e.CLISessionID] = true
		}
	}
	return active, true
}
