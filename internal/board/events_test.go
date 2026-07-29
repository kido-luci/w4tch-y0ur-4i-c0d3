package board

import (
	"encoding/json"
	"testing"
	"time"
)

// The cap is a backstop against a runaway writer, not a retention policy — so
// what matters is that it bounds growth WITHOUT taking anything the burndown
// replay depends on.
func TestEventsTrimBoundsGrowthAndKeepsCreated(t *testing.T) {
	db := newTestDataDB(t)
	es := NewEventStore(db)

	es.Append("card-a", "created", "", "backlog")
	for i := 0; i < maxEventsPerTodo+40; i++ {
		es.Append("card-a", "status", "backlog", "doing")
	}
	got, err := es.ForTodo("card-a")
	if err != nil {
		t.Fatal(err)
	}
	// The cap plus the exempt `created` row.
	if len(got) > maxEventsPerTodo+1 {
		t.Fatalf("history should be capped, got %d rows", len(got))
	}
	if len(got) < maxEventsPerTodo {
		t.Fatalf("the cap trimmed too far, got %d rows", len(got))
	}
	// created must survive at any age — the replay reads its absence as "this
	// card already existed before the window".
	if got[0].Kind != "created" {
		t.Fatalf("the created event must be kept and stay first, got %q", got[0].Kind)
	}

	// Another card's history is untouched by the first one's trimming.
	es.Append("card-b", "created", "", "backlog")
	es.Append("card-b", "status", "backlog", "done")
	if other, err := es.ForTodo("card-b"); err != nil || len(other) != 2 {
		t.Fatalf("want card-b's 2 rows intact, got %d (%v)", len(other), err)
	}
}

// Under the cap nothing is dropped at all.
func TestEventsTrimIsANoOpBelowTheCap(t *testing.T) {
	db := newTestDataDB(t)
	es := NewEventStore(db)
	for _, k := range []string{"created", "status", "estimate", "cycle", "priority"} {
		es.Append("card-a", k, "", "x")
	}
	got, err := es.ForTodo("card-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("want all 5 rows kept, got %d", len(got))
	}
}

// An empty log must serialise as `[]`, not `null`. A nil slice marshals as null,
// and a client doing `events.map(...)` on it throws — which is how the cycles
// tab broke on a board whose log had no rows yet, while every seeded test
// instance worked fine.
func TestEventsEmptyListMarshalsAsArray(t *testing.T) {
	db := newTestDataDB(t)
	es := NewEventStore(db)

	for name, got := range map[string][]TodoEvent{
		"ForTodo": mustEvents(t, func() ([]TodoEvent, error) { return es.ForTodo("nobody") }),
		"Since":   mustEvents(t, func() ([]TodoEvent, error) { return es.Since(time.Now()) }),
		"Recent":  mustEvents(t, func() ([]TodoEvent, error) { return es.Recent(10) }),
	} {
		if got == nil {
			t.Errorf("%s returned a nil slice; it marshals as null", name)
			continue
		}
		b, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "[]" {
			t.Errorf("%s marshalled as %s, want []", name, b)
		}
	}
}

func mustEvents(t *testing.T, fn func() ([]TodoEvent, error)) []TodoEvent {
	t.Helper()
	got, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	return got
}
