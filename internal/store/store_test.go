package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// C-5: tests touch only this database. The maintenance database is used only
// to create it when it is missing.
const (
	testDatabase        = "checknumber_test"
	maintenanceDatabase = "postgres"
	absentDatabase      = "checknumber_test_absent"
	pgInvalidCatalog    = "3D000" // SQLSTATE invalid_catalog_name: database does not exist
	day                 = 24 * time.Hour
)

const (
	numberA       = "+15552230001"
	numberB       = "+15552230002"
	resultFax     = "fax"
	resultVoice   = "voice"
	resultUnknown = "unknown"
	reasonRestart = "app restarted"
)

// A check that needs no call (R-28), as SaveDone stores it.
const (
	numberAbroad        = "+442079460958"
	resultInternational = "international"
	reasonNotCalled     = "not called: outside US and Canada"
)

var baseTime = time.Date(2026, 10, 1, 14, 30, 15, 0, time.UTC)

// newTestStore opens a Store on checknumber_test inside a fresh schema that is
// dropped when the test ends. Open sees the schema through PGOPTIONS, so the
// Store under test is built only from the environment.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	t.Setenv("PGDATABASE", testDatabase)
	admin := connectTestDatabase(t, ctx)

	schema := fmt.Sprintf("store_test_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
		admin.Close(ctx)
	})

	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	s, err := Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return s
}

// connectTestDatabase connects to checknumber_test, creating it first when the
// server reports it missing.
func connectTestDatabase(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, "")
	if err == nil {
		return conn
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgInvalidCatalog {
		t.Fatalf("connect to %s: %v", testDatabase, err)
	}
	t.Setenv("PGDATABASE", maintenanceDatabase)
	maint, err := pgx.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect to %s to create %s: %v", maintenanceDatabase, testDatabase, err)
	}
	_, err = maint.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{testDatabase}.Sanitize())
	maint.Close(ctx)
	if err != nil {
		t.Fatalf("create database %s: %v", testDatabase, err)
	}
	t.Setenv("PGDATABASE", testDatabase)
	conn, err = pgx.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect to %s after creating it: %v", testDatabase, err)
	}
	return conn
}

func history(t *testing.T, s *Store, number string) []Check {
	t.Helper()
	checks, err := s.History(context.Background(), number)
	if err != nil {
		t.Fatalf("History(%s): %v", number, err)
	}
	return checks
}

func begin(t *testing.T, s *Store, number string, at time.Time) int64 {
	t.Helper()
	id, err := s.Begin(context.Background(), number, at)
	if err != nil {
		t.Fatalf("Begin(%s): %v", number, err)
	}
	return id
}

func markStarted(t *testing.T, s *Store, id int64, at time.Time) {
	t.Helper()
	if err := s.MarkStarted(context.Background(), id, at); err != nil {
		t.Fatalf("MarkStarted(%d): %v", id, err)
	}
}

func finish(t *testing.T, s *Store, id int64, code, reason, text, recording string, at time.Time) {
	t.Helper()
	if err := s.Finish(context.Background(), id, code, reason, text, recording, at); err != nil {
		t.Fatalf("Finish(%d): %v", id, err)
	}
}

func saveDone(t *testing.T, s *Store, number string, at time.Time) int64 {
	t.Helper()
	id, err := s.SaveDone(context.Background(), number, resultInternational, reasonNotCalled, at)
	if err != nil {
		t.Fatalf("SaveDone(%s): %v", number, err)
	}
	return id
}

func TestBeginSavesNumber(t *testing.T) {
	s := newTestStore(t)
	id := begin(t, s, numberA, baseTime)

	var number, state string
	var requested time.Time
	var startedNull, finishedNull bool
	err := s.pool.QueryRow(context.Background(),
		`SELECT number, state, requested_at, started_at IS NULL, finished_at IS NULL
		FROM checks WHERE id = $1`, id).
		Scan(&number, &state, &requested, &startedNull, &finishedNull)
	if err != nil {
		t.Fatalf("read back check %d: %v", id, err)
	}
	if number != numberA || state != StateInProgress || !requested.Equal(baseTime) {
		t.Errorf("row = (%q, %q, %v), want (%q, %q, %v)",
			number, state, requested, numberA, StateInProgress, baseTime)
	}
	if !startedNull || !finishedNull {
		t.Errorf("started_at NULL = %v, finished_at NULL = %v; want both NULL", startedNull, finishedNull)
	}
}

func TestFinishAndHistory(t *testing.T) {
	s := newTestStore(t)

	first := begin(t, s, numberA, baseTime)
	markStarted(t, s, first, baseTime.Add(time.Second))
	finish(t, s, first, resultFax, "fax tone", "", "", baseTime.Add(20*time.Second))

	other := begin(t, s, numberB, baseTime.Add(time.Minute))
	finish(t, s, other, resultFax, "fax tone", "", "", baseTime.Add(2*time.Minute))

	later := baseTime.Add(31 * day)
	second := begin(t, s, numberA, later)
	markStarted(t, s, second, later.Add(time.Second))
	finish(t, s, second, resultVoice, "voice answered", "please leave a message", "recordings/2.wav", later.Add(15*time.Second))

	// Arrival timestamps, not insertion order, determine history order.
	waitingAt := baseTime.Add(-day)
	waiting := begin(t, s, numberA, waitingAt)

	want := []Check{
		{
			ID: second, Number: numberA, State: StateDone, ResultCode: resultVoice,
			Reason: "voice answered", Text: "please leave a message", RecordingPath: "recordings/2.wav",
			RequestedAt: later, StartedAt: later.Add(time.Second), FinishedAt: later.Add(15 * time.Second),
		},
		{
			ID: first, Number: numberA, State: StateDone, ResultCode: resultFax, Reason: "fax tone",
			RequestedAt: baseTime, StartedAt: baseTime.Add(time.Second), FinishedAt: baseTime.Add(20 * time.Second),
		},
		{ID: waiting, Number: numberA, State: StateInProgress, RequestedAt: waitingAt},
	}
	if got := history(t, s, numberA); !reflect.DeepEqual(got, want) {
		t.Errorf("History(%s) =\n%+v\nwant\n%+v", numberA, got, want)
	}
}

func TestLastDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Inserted first but started later, so started_at order differs from id order.
	newest := begin(t, s, numberA, baseTime)
	markStarted(t, s, newest, baseTime.Add(10*day))
	finish(t, s, newest, resultVoice, "", "", "", baseTime.Add(10*day+time.Minute))

	older := begin(t, s, numberA, baseTime.Add(time.Hour))
	markStarted(t, s, older, baseTime.Add(2*day))
	finish(t, s, older, resultFax, "", "", "", baseTime.Add(2*day+time.Minute))

	running := begin(t, s, numberA, baseTime.Add(20*day))
	markStarted(t, s, running, baseTime.Add(20*day))

	got, ok, err := s.LastDone(ctx, numberA)
	if err != nil {
		t.Fatalf("LastDone(%s): %v", numberA, err)
	}
	if !ok || got.ID != newest || got.ResultCode != resultVoice || got.State != StateDone {
		t.Errorf("LastDone(%s) = (id %d, %q, %q, %v), want (id %d, %q, %q, true)",
			numberA, got.ID, got.ResultCode, got.State, ok, newest, resultVoice, StateDone)
	}

	begin(t, s, numberB, baseTime)
	if got, ok, err := s.LastDone(ctx, numberB); err != nil || ok {
		t.Errorf("LastDone(%s) with only an in_progress check = (id %d, %v, %v), want not found", numberB, got.ID, ok, err)
	}
}

func TestLastDoneNeverStarted(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	started := begin(t, s, numberA, baseTime)
	markStarted(t, s, started, baseTime.Add(time.Second))
	finish(t, s, started, resultFax, "fax tone", "", "", baseTime.Add(time.Minute))
	begin(t, s, numberA, baseTime.Add(day))
	neverStarted := begin(t, s, numberB, baseTime.Add(day))
	closedAt := baseTime.Add(2 * day)
	if n, err := s.MarkUnfinished(ctx, resultUnknown, reasonRestart, closedAt); err != nil || n != 2 {
		t.Fatalf("MarkUnfinished = (%d, %v), want (2, nil)", n, err)
	}

	got, ok, err := s.LastDone(ctx, numberA)
	if err != nil || !ok || got.ID != started {
		t.Errorf("LastDone(%s) = (%+v, %v, %v), want started check %d", numberA, got, ok, err, started)
	}
	got, ok, err = s.LastDone(ctx, numberB)
	want := Check{
		ID: neverStarted, Number: numberB, State: StateDone,
		ResultCode: resultUnknown, Reason: reasonRestart,
		RequestedAt: baseTime.Add(day), FinishedAt: closedAt,
	}
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("LastDone(%s) = (%+v, %v, %v), want (%+v, true, nil)", numberB, got, ok, err, want)
	}
}

func TestTimesAreUTC(t *testing.T) {
	s := newTestStore(t)
	phnomPenh := time.FixedZone("ICT", 7*60*60)
	requested := time.Date(2026, 11, 13, 22, 5, 9, 0, phnomPenh)
	started := requested.Add(3 * time.Second)
	finished := requested.Add(40 * time.Second)

	id := begin(t, s, numberA, requested)
	markStarted(t, s, id, started)
	finish(t, s, id, resultFax, "", "", "", finished)

	got := history(t, s, numberA)
	if len(got) != 1 {
		t.Fatalf("History(%s) returned %d checks, want 1", numberA, len(got))
	}
	for name, pair := range map[string][2]time.Time{
		"requested_at": {got[0].RequestedAt, requested},
		"started_at":   {got[0].StartedAt, started},
		"finished_at":  {got[0].FinishedAt, finished},
	} {
		gotTime, wantTime := pair[0], pair[1]
		if gotTime.Location() != time.UTC {
			t.Errorf("%s location = %v, want UTC", name, gotTime.Location())
		}
		if !gotTime.Truncate(time.Second).Equal(wantTime) || gotTime.Format(time.DateTime) != wantTime.UTC().Format(time.DateTime) {
			t.Errorf("%s = %v, want %v", name, gotTime, wantTime.UTC())
		}
	}

	var storedUTC string
	err := s.pool.QueryRow(context.Background(),
		`SELECT to_char(requested_at AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') FROM checks WHERE id = $1`, id).
		Scan(&storedUTC)
	if err != nil {
		t.Fatalf("read stored requested_at of %d: %v", id, err)
	}
	if want := requested.UTC().Format(time.DateTime); storedUTC != want {
		t.Errorf("stored requested_at in UTC = %s, want %s", storedUTC, want)
	}
}

func TestMarkUnfinished(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	doneID := begin(t, s, numberA, baseTime)
	markStarted(t, s, doneID, baseTime.Add(time.Second))
	finish(t, s, doneID, resultFax, "fax tone", "", "", baseTime.Add(time.Minute))
	doneBefore := history(t, s, numberA)

	startedID := begin(t, s, numberB, baseTime.Add(time.Hour))
	markStarted(t, s, startedID, baseTime.Add(time.Hour+time.Second))
	waitingID := begin(t, s, numberB, baseTime.Add(2*time.Hour))

	at := baseTime.Add(3 * time.Hour)
	n, err := s.MarkUnfinished(ctx, resultUnknown, reasonRestart, at)
	if err != nil {
		t.Fatalf("MarkUnfinished: %v", err)
	}
	if n != 2 {
		t.Errorf("MarkUnfinished closed %d checks, want 2", n)
	}

	want := []Check{
		{ID: waitingID, Number: numberB, State: StateDone, ResultCode: resultUnknown, Reason: reasonRestart,
			RequestedAt: baseTime.Add(2 * time.Hour), FinishedAt: at},
		{ID: startedID, Number: numberB, State: StateDone, ResultCode: resultUnknown, Reason: reasonRestart,
			RequestedAt: baseTime.Add(time.Hour), StartedAt: baseTime.Add(time.Hour + time.Second), FinishedAt: at},
	}
	if got := history(t, s, numberB); !reflect.DeepEqual(got, want) {
		t.Errorf("History(%s) after MarkUnfinished =\n%+v\nwant\n%+v", numberB, got, want)
	}
	if got := history(t, s, numberA); !reflect.DeepEqual(got, doneBefore) {
		t.Errorf("done check changed by MarkUnfinished:\n%+v\nwant\n%+v", got, doneBefore)
	}

	if n, err := s.MarkUnfinished(ctx, resultUnknown, reasonRestart, at); err != nil || n != 0 {
		t.Errorf("second MarkUnfinished = (%d, %v), want (0, nil)", n, err)
	}
}

func TestOpenUsesEnvironment(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	var database, schema string
	if err := s.pool.QueryRow(ctx, `SELECT current_database(), current_schema()`).Scan(&database, &schema); err != nil {
		t.Fatalf("read current database: %v", err)
	}
	if database != testDatabase {
		t.Errorf("Open connected to database %q, want %q from PGDATABASE", database, testDatabase)
	}
	if got, want := "-c search_path="+schema, os.Getenv("PGOPTIONS"); got != want {
		t.Errorf("current schema = %q, want the one PGOPTIONS names (%q)", schema, want)
	}

	// Discriminator: if Open used anything but the environment, changing
	// PGDATABASE would not make it fail.
	t.Setenv("PGDATABASE", absentDatabase)
	bad, err := Open(ctx)
	if err == nil {
		bad.Close()
		t.Fatalf("Open with PGDATABASE=%s succeeded, want a missing-database error", absentDatabase)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgInvalidCatalog {
		t.Errorf("Open with PGDATABASE=%s error = %v, want SQLSTATE %s", absentDatabase, err, pgInvalidCatalog)
	}
}

func TestSaveDone(t *testing.T) {
	s := newTestStore(t)
	at := time.Date(2026, 11, 14, 13, 5, 9, 0, time.FixedZone("ICT", 7*60*60))

	id := saveDone(t, s, numberAbroad, at)

	want := Check{
		ID: id, Number: numberAbroad, State: StateDone,
		ResultCode: resultInternational, Reason: reasonNotCalled,
		RequestedAt: at.UTC(), StartedAt: at.UTC(), FinishedAt: at.UTC(),
	}
	if got := history(t, s, numberAbroad); !reflect.DeepEqual(got, []Check{want}) {
		t.Errorf("History(%s) after SaveDone =\n%+v\nwant\n%+v", numberAbroad, got, []Check{want})
	}
	got, ok, err := s.LastDone(context.Background(), numberAbroad)
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("LastDone(%s) = (%+v, %v, %v), want (%+v, true, nil)", numberAbroad, got, ok, err, want)
	}

	// The three writes of the dial path with the same values give the same row.
	stepwise := begin(t, s, numberA, at)
	markStarted(t, s, stepwise, at)
	finish(t, s, stepwise, resultInternational, reasonNotCalled, "", "", at)
	want.ID, want.Number = stepwise, numberA
	if got := history(t, s, numberA); !reflect.DeepEqual(got, []Check{want}) {
		t.Errorf("row of Begin, MarkStarted and Finish =\n%+v\nwant the form SaveDone wrote\n%+v", got, []Check{want})
	}
}

func TestSaveDoneFailureLeavesNoRow(t *testing.T) {
	s := newTestStore(t)
	// The same write succeeds on a live context, so the cancelled context is
	// the only cause of the failure below.
	saveDone(t, s, numberA, baseTime)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.SaveDone(ctx, numberAbroad, resultInternational, reasonNotCalled, baseTime)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SaveDone(%s) with a cancelled context: error = %v, want one that wraps %v", numberAbroad, err, context.Canceled)
	}
	if msg := err.Error(); !strings.HasPrefix(msg, "store: ") || !strings.Contains(msg, numberAbroad) {
		t.Errorf("SaveDone error = %q, want a store error that names %s", msg, numberAbroad)
	}
	if got := history(t, s, numberAbroad); len(got) != 0 {
		t.Errorf("History(%s) after the failed SaveDone = %+v, want no row", numberAbroad, got)
	}
}

func TestSaveDoneNeverUnfinished(t *testing.T) {
	s := newTestStore(t)
	id := saveDone(t, s, numberAbroad, baseTime)

	n, err := s.MarkUnfinished(context.Background(), resultUnknown, reasonRestart, baseTime.Add(time.Hour))
	if err != nil || n != 0 {
		t.Errorf("MarkUnfinished after SaveDone = (%d, %v), want (0, nil)", n, err)
	}
	want := []Check{{
		ID: id, Number: numberAbroad, State: StateDone,
		ResultCode: resultInternational, Reason: reasonNotCalled,
		RequestedAt: baseTime, StartedAt: baseTime, FinishedAt: baseTime,
	}}
	if got := history(t, s, numberAbroad); !reflect.DeepEqual(got, want) {
		t.Errorf("History(%s) after MarkUnfinished =\n%+v\nwant\n%+v", numberAbroad, got, want)
	}
}
