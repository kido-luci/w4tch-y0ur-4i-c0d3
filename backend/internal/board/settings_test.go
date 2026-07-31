package board

import "testing"

func TestSettingsStorePresentationRoundTrip(t *testing.T) {
	db := newTestDataDB(t)
	ss := NewSettingsStore(db)
	if ss.PresentationHidden() {
		t.Fatal("a fresh store must start with presentation mode off")
	}
	if err := ss.SetPresentationHidden(true); err != nil {
		t.Fatal(err)
	}
	if !ss.PresentationHidden() {
		t.Fatal("the toggle should read back on")
	}
	// A second store over the same db sees the persisted value — the restart case.
	if again := NewSettingsStore(db); !again.PresentationHidden() {
		t.Fatal("the setting must survive a reload from the db")
	}
	if err := ss.SetPresentationHidden(false); err != nil {
		t.Fatal(err)
	}
	if ss.PresentationHidden() {
		t.Fatal("the toggle should read back off")
	}
}
