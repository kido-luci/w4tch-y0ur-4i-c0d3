package board

// Settings — the tiny kv table behind app-wide switches. Its first (and so far
// only) consumer is presentation mode: the toggle that hides private projects
// everywhere while you demo or screenshot. The state lives server-side on
// purpose — one switch has to cover every open tab, every endpoint family and
// MCP at once, or a "hidden" project leaks through whichever surface kept its
// own copy of the flag.

import (
	"database/sql"
	"fmt"
	"log"
	"sync"
)

const presentationKey = "presentation_hide_private"

// SettingsStore persists settings to data.db (settings). Same write model as
// the other stores: single writer, in-memory serving copy, DB-first.
type SettingsStore struct {
	db *sql.DB

	mu     sync.Mutex
	values map[string]string
}

func NewSettingsStore(db *sql.DB) *SettingsStore {
	ss := &SettingsStore{db: db, values: map[string]string{}}
	rows, err := db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		log.Printf("settings: load: %v", err)
		return ss
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			log.Printf("settings: load row: %v", err)
			continue
		}
		ss.values[k] = v
	}
	return ss
}

// PresentationHidden reports whether presentation mode is on — private
// projects hidden app-wide.
func (ss *SettingsStore) PresentationHidden() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.values[presentationKey] == "1"
}

// SetPresentationHidden turns presentation mode on or off.
func (ss *SettingsStore) SetPresentationHidden(on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if _, err := ss.db.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		presentationKey, v); err != nil {
		return fmt.Errorf("write setting: %w", err)
	}
	ss.values[presentationKey] = v
	return nil
}
