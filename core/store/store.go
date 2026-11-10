// Package store persists events, rules, runs, instances and plugin state in SQLite.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dounx/cat/core/event"
	"github.com/dounx/cat/core/rules"
)

var ErrNotFound = errors.New("not found")

type Store struct{ db *sql.DB }

// Instance is a configured copy of a plugin.
type Instance struct {
	ID      string         `json:"id"`
	Plugin  string         `json:"plugin"`
	Name    string         `json:"name"`
	Enabled bool           `json:"enabled"`
	Config  map[string]any `json:"config"`
}

// StepResult records one executed pipeline step.
type StepResult struct {
	ID         string         `json:"id"`
	Instance   string         `json:"instance"`
	Action     string         `json:"action"`
	Params     map[string]any `json:"params,omitempty"`
	Output     map[string]any `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	DurationMS int64          `json:"duration_ms"`
}

// Run records one execution of a rule against an event.
type Run struct {
	ID         string       `json:"id"`
	RuleID     string       `json:"rule_id"`
	RuleName   string       `json:"rule_name"`
	EventID    string       `json:"event_id"`
	Status     string       `json:"status"` // running | ok | error
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Steps      []StepResult `json:"steps"`
	Replay     bool         `json:"replay,omitempty"`
}

const schema = `
CREATE TABLE IF NOT EXISTS events (id TEXT PRIMARY KEY, source TEXT, type TEXT, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS events_source ON events(source, id);
CREATE TABLE IF NOT EXISTS rules (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS runs (id TEXT PRIMARY KEY, rule_id TEXT, event_id TEXT, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS runs_rule ON runs(rule_id, id);
CREATE INDEX IF NOT EXISTS runs_event ON runs(event_id);
CREATE TABLE IF NOT EXISTS instances (id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS kv (scope TEXT, key TEXT, value TEXT, PRIMARY KEY(scope, key));
`

// Open opens (creating if needed) the database at path. Use ":memory:" in tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		dsn = "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func getJSON[T any](s *Store, query string, args ...any) (*T, error) {
	var data string
	err := s.db.QueryRow(query, args...).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	v := new(T)
	return v, json.Unmarshal([]byte(data), v)
}

func listJSON[T any](s *Store, query string, args ...any) ([]T, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- events ----

func (s *Store) SaveEvent(e *event.Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO events(id, source, type, data) VALUES(?,?,?,?)`, e.ID, e.Source, e.Type, string(b))
	return err
}

func (s *Store) GetEvent(id string) (*event.Event, error) {
	return getJSON[event.Event](s, `SELECT data FROM events WHERE id=?`, id)
}

// EventFilter narrows ListEvents. Before is an exclusive id cursor.
type EventFilter struct {
	Source, Type, Before string
	Limit                int
}

func (s *Store) ListEvents(f EventFilter) ([]event.Event, error) {
	var where []string
	var args []any
	if f.Source != "" {
		where = append(where, "source=?")
		args = append(args, f.Source)
	}
	if f.Type != "" {
		where = append(where, "type=?")
		args = append(args, f.Type)
	}
	if f.Before != "" {
		where = append(where, "id<?")
		args = append(args, f.Before)
	}
	q := `SELECT data FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.Limit)
	return listJSON[event.Event](s, q, args...)
}

// PruneEvents keeps only the newest keep events (and drops runs of removed ones).
func (s *Store) PruneEvents(keep int) error {
	_, err := s.db.Exec(`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`, keep)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM runs WHERE id NOT IN (SELECT id FROM runs ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

// ---- rules ----

func (s *Store) SaveRule(r *rules.Rule) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO rules(id, data) VALUES(?,?)`, r.ID, string(b))
	return err
}

func (s *Store) DeleteRule(id string) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE id=?`, id)
	return err
}

func (s *Store) ListRules() ([]rules.Rule, error) {
	return listJSON[rules.Rule](s, `SELECT data FROM rules ORDER BY id`)
}

// ---- runs ----

func (s *Store) SaveRun(r *Run) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO runs(id, rule_id, event_id, data) VALUES(?,?,?,?)`, r.ID, r.RuleID, r.EventID, string(b))
	return err
}

func (s *Store) GetRun(id string) (*Run, error) {
	return getJSON[Run](s, `SELECT data FROM runs WHERE id=?`, id)
}

func (s *Store) ListRuns(ruleID, eventID string, limit int) ([]Run, error) {
	var where []string
	var args []any
	if ruleID != "" {
		where = append(where, "rule_id=?")
		args = append(args, ruleID)
	}
	if eventID != "" {
		where = append(where, "event_id=?")
		args = append(args, eventID)
	}
	q := `SELECT data FROM runs`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	return listJSON[Run](s, q, args...)
}

// ---- instances ----

func (s *Store) SaveInstance(i *Instance) error {
	b, err := json.Marshal(i)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO instances(id, data) VALUES(?,?)`, i.ID, string(b))
	return err
}

func (s *Store) DeleteInstance(id string) error {
	if _, err := s.db.Exec(`DELETE FROM kv WHERE scope=?`, "instance:"+id); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM instances WHERE id=?`, id)
	return err
}

func (s *Store) ListInstances() ([]Instance, error) {
	return listJSON[Instance](s, `SELECT data FROM instances ORDER BY id`)
}

// ---- key/value state ----

func (s *Store) KVGet(scope, key string) (string, bool) {
	var v string
	if err := s.db.QueryRow(`SELECT value FROM kv WHERE scope=? AND key=?`, scope, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (s *Store) KVSet(scope, key, value string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO kv(scope, key, value) VALUES(?,?,?)`, scope, key, value)
	return err
}

func (s *Store) KVDelete(scope, key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE scope=? AND key=?`, scope, key)
	return err
}
