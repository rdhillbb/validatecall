// Package store keeps every check of a phone number in PostgreSQL.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Check states; the schema's CHECK constraint mirrors this set.
const (
	StateInProgress = "in_progress"
	StateDone       = "done"
)

//go:embed schema.sql
var schemaSQL string

const checkColumns = `id, number, state, result_code, reason, text, recording_path,
	requested_at, started_at, finished_at`

// Check is one row of the checks table. Times are UTC; a time that is not set
// yet is the zero time.
type Check struct {
	ID            int64
	Number        string
	State         string
	ResultCode    string
	Reason        string
	Text          string
	RecordingPath string
	RequestedAt   time.Time
	StartedAt     time.Time
	FinishedAt    time.Time
}

// Store is the check history store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects using only the PG* environment variables (C-3): the empty
// connection string makes pgx read PGHOST, PGPORT, PGDATABASE, PGUSER,
// PGPASSWORD and the rest from the environment.
func Open(ctx context.Context) (*Store, error) {
	pool, err := pgxpool.New(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("store: open pool from environment: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping database from environment: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Migrate creates the schema; it is safe to run on every start.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	return nil
}

// Close releases every pooled connection.
func (s *Store) Close() {
	s.pool.Close()
}

// Begin saves the number as soon as the request arrives (AD-3).
func (s *Store) Begin(ctx context.Context, number string, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO checks (number, state, requested_at) VALUES ($1, $2, $3) RETURNING id`,
		number, StateInProgress, at.UTC()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: begin check of %s: %w", number, err)
	}
	return id, nil
}

// MarkStarted records when the call of check id started.
func (s *Store) MarkStarted(ctx context.Context, id int64, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE checks SET started_at = $2 WHERE id = $1`, id, at.UTC())
	if err != nil {
		return fmt.Errorf("store: mark check %d started: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: mark check %d started: no such check", id)
	}
	return nil
}

// Finish stores the outcome of check id and sets its state to done.
func (s *Store) Finish(ctx context.Context, id int64, resultCode, reason, text, recordingPath string, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE checks SET state = $2, result_code = $3, reason = $4, text = $5,
			recording_path = $6, finished_at = $7
		WHERE id = $1`,
		id, StateDone, resultCode, reason, text, recordingPath, at.UTC())
	if err != nil {
		return fmt.Errorf("store: finish check %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("store: finish check %d: no such check", id)
	}
	return nil
}

// SaveDone saves a check that needs no call (R-28) already finished: request,
// start and finish are the one moment at. It is a single INSERT so that a
// failure leaves no row; Begin, MarkStarted and Finish can stop between two
// writes and leave an in_progress row that start-up closes as unknown. Text
// and recording path are empty by the column defaults.
func (s *Store) SaveDone(ctx context.Context, number, resultCode, reason string, at time.Time) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO checks (number, state, result_code, reason, requested_at, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $5, $5) RETURNING id`,
		number, StateDone, resultCode, reason, at.UTC()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: save done check of %s: %w", number, err)
	}
	return id, nil
}

// LastDone returns the most recent done check of number by started_at; the
// bool is false when the number has no done check. A done check that never
// started (closed by MarkUnfinished) sorts after every started one.
func (s *Store) LastDone(ctx context.Context, number string) (Check, bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+checkColumns+` FROM checks
		WHERE number = $1 AND state = $2
		ORDER BY started_at DESC NULLS LAST, id DESC
		LIMIT 1`,
		number, StateDone)
	if err != nil {
		return Check{}, false, fmt.Errorf("store: last done check of %s: %w", number, err)
	}
	c, err := pgx.CollectExactlyOneRow(rows, scanCheck)
	if errors.Is(err, pgx.ErrNoRows) {
		return Check{}, false, nil
	}
	if err != nil {
		return Check{}, false, fmt.Errorf("store: last done check of %s: %w", number, err)
	}
	return c, true, nil
}

// History returns every check of number, newest request first.
func (s *Store) History(ctx context.Context, number string) ([]Check, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+checkColumns+` FROM checks
		WHERE number = $1
		ORDER BY requested_at DESC, id DESC`,
		number)
	if err != nil {
		return nil, fmt.Errorf("store: history of %s: %w", number, err)
	}
	checks, err := pgx.CollectRows(rows, scanCheck)
	if err != nil {
		return nil, fmt.Errorf("store: history of %s: %w", number, err)
	}
	return checks, nil
}

// MarkUnfinished closes every in_progress check with the given result and
// returns how many it closed. It runs at start-up, when no call can still be
// in flight (TC-20).
func (s *Store) MarkUnfinished(ctx context.Context, resultCode, reason string, at time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE checks SET state = $1, result_code = $2, reason = $3, finished_at = $4
		WHERE state = $5`,
		StateDone, resultCode, reason, at.UTC(), StateInProgress)
	if err != nil {
		return 0, fmt.Errorf("store: mark unfinished checks %s: %w", resultCode, err)
	}
	return tag.RowsAffected(), nil
}

func scanCheck(row pgx.CollectableRow) (Check, error) {
	var c Check
	var started, finished *time.Time
	err := row.Scan(&c.ID, &c.Number, &c.State, &c.ResultCode, &c.Reason, &c.Text,
		&c.RecordingPath, &c.RequestedAt, &started, &finished)
	if err != nil {
		return Check{}, err
	}
	c.RequestedAt = c.RequestedAt.UTC()
	if started != nil {
		c.StartedAt = started.UTC()
	}
	if finished != nil {
		c.FinishedAt = finished.UTC()
	}
	return c, nil
}
