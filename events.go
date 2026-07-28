package main

// The board's history — an append-only log of the card mutations that a chart
// or an activity feed needs (status, estimate, cycle, parent, priority).
//
// It exists because current state cannot answer "when". data.db knows a card
// is done; it does not know it crossed into done last Tuesday, and without
// that there is no burndown, no cycle report and no "what moved this week".
//
// Append-only is the whole design: rows are never updated or deleted, so a
// replay is deterministic and the table doubles as an audit trail. Unlike the
// other stores there is no in-memory serving copy — history is written far
// more often than it is read, and the reads are range queries SQLite indexes
// better than a slice scan would.

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

// TodoEvent is one recorded change. From/To carry the values as strings
// (a state id, a cycle id, a formatted point total) so one table covers every
// kind without a column per field.
type TodoEvent struct {
	ID     int64     `json:"id"`
	TodoID string    `json:"todoId"`
	Ts     time.Time `json:"ts"`
	Kind   string    `json:"kind"` // created | status | estimate | cycle | parent | priority
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
}

// EventStore appends to and reads back data.db's todo_events.
type EventStore struct {
	db *sql.DB
}

func NewEventStore(db *sql.DB) *EventStore { return &EventStore{db: db} }

// maxEventsPerTodo backstops a runaway writer — a client looping on a status
// flip, say — rather than expiring history. It is set far above anything a
// human produces: a card moved five times a day would take two years to reach
// it, so in practice this never fires.
const maxEventsPerTodo = 500

// Append records one change. Best-effort by contract: the caller has already
// committed the change itself, so a lost history row is logged and swallowed
// rather than failing a move the user already saw succeed.
func (es *EventStore) Append(todoID, kind, from, to string) {
	if _, err := es.db.Exec(
		`INSERT INTO todo_events(todo_id,ts,kind,from_val,to_val) VALUES(?,?,?,?,?)`,
		todoID, timeToNano(time.Now()), kind, from, to); err != nil {
		log.Printf("events: append %s/%s: %v", todoID, kind, err)
		return
	}
	es.trim(todoID)
}

// trim keeps one card's newest maxEventsPerTodo rows and drops the rest.
//
// Deliberately NOT an age-based retention policy, which is the obvious design
// and the wrong one: ComputeBurndown replays a cycle's window backwards from
// the live board, so expiring anything inside that window silently changes a
// chart into fiction. A count cap per card cannot do that, because a cycle
// short enough to chart cannot also hold 500 events for one card.
//
// `created` is exempt at any age. The replay reads its absence as "the card
// existed before this window", so dropping it would make a card look like it
// had been in the cycle since day one.
//
// The statement is a no-op below the cap — the subquery already selects every
// row — so there is nothing to gate it on.
func (es *EventStore) trim(todoID string) {
	if _, err := es.db.Exec(
		`DELETE FROM todo_events
		 WHERE todo_id = ?
		   AND kind <> 'created'
		   AND id NOT IN (
		     SELECT id FROM todo_events WHERE todo_id = ? ORDER BY ts DESC, id DESC LIMIT ?
		   )`,
		todoID, todoID, maxEventsPerTodo); err != nil {
		log.Printf("events: trim %s: %v", todoID, err)
	}
}

func (es *EventStore) scan(rows *sql.Rows) []TodoEvent {
	defer rows.Close()
	var out []TodoEvent
	for rows.Next() {
		var e TodoEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.TodoID, &ts, &e.Kind, &e.From, &e.To); err != nil {
			log.Printf("events: scan: %v", err)
			continue
		}
		e.Ts = nanoToTime(ts)
		out = append(out, e)
	}
	return out
}

// ForTodo returns one card's history, oldest first — the card panel's
// activity feed.
func (es *EventStore) ForTodo(todoID string) ([]TodoEvent, error) {
	rows, err := es.db.Query(
		`SELECT id,todo_id,ts,kind,from_val,to_val FROM todo_events WHERE todo_id=? ORDER BY ts, id`, todoID)
	if err != nil {
		return nil, fmt.Errorf("events: for todo: %w", err)
	}
	return es.scan(rows), nil
}

// Since returns every event at or after ts, oldest first — the board-wide
// activity feed, and the raw material a burndown replays.
func (es *EventStore) Since(ts time.Time) ([]TodoEvent, error) {
	rows, err := es.db.Query(
		`SELECT id,todo_id,ts,kind,from_val,to_val FROM todo_events WHERE ts>=? ORDER BY ts, id`,
		timeToNano(ts))
	if err != nil {
		return nil, fmt.Errorf("events: since: %w", err)
	}
	return es.scan(rows), nil
}

// Recent returns the last n events, newest first.
func (es *EventStore) Recent(n int) ([]TodoEvent, error) {
	if n <= 0 || n > 500 {
		n = 100
	}
	rows, err := es.db.Query(
		`SELECT id,todo_id,ts,kind,from_val,to_val FROM todo_events ORDER BY ts DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("events: recent: %w", err)
	}
	return es.scan(rows), nil
}

// PurgeTodo drops a deleted card's history. Called only when a card is
// removed outright — the one exception to append-only, because history for a
// card nothing can name again is unreachable noise.
func (es *EventStore) PurgeTodo(todoID string) {
	if _, err := es.db.Exec(`DELETE FROM todo_events WHERE todo_id=?`, todoID); err != nil {
		log.Printf("events: purge %s: %v", todoID, err)
	}
}
