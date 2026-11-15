package check

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"checknumber/internal/phone"
	"checknumber/internal/result"
	"checknumber/internal/store"

	"github.com/jackc/pgx/v5"
)

// C-5: tests touch only this database and never a real Telnyx.
const testDatabase = "checknumber_test"

const (
	day = 24 * time.Hour
	// reuseLimit writes TC-16 out again, so a wrong reuseWindow fails here.
	reuseLimit = 60 * day
	// waitLimit bounds every wait on another goroutine, so a broken lock or
	// wait list fails the test instead of hanging it.
	waitLimit = 5 * time.Second
	// overlapWatch is how long a held call is watched for a second dial.
	overlapWatch = 100 * time.Millisecond
	pollEvery    = time.Millisecond
)

const (
	inputA     = "(555) 223-0001"
	numberA    = "+15552230001"
	inputB     = "555.223.0002"
	numberB    = "+15552230002"
	reasonDial = "Telnyx could not tell what answered"
	restarted  = "app restarted"
	// wantReasonCancelled writes the saved reason of a request cancelled in
	// the queue out again, so a changed text in service.go fails here.
	wantReasonCancelled = "not called: the request was cancelled"
)

const (
	// A number of another country (R-28) and the form it is saved under.
	inputAbroad  = "+44 20 7946 0958"
	numberAbroad = "+442079460958"
	// wantReasonInternational writes the reason of TC-43 out again, so a
	// changed text in the result package fails here.
	wantReasonInternational = "not called: outside US and Canada"
)

// updateRefused is the error of refuseUpdates, which makes the database of one
// test refuse every change to a saved check, so that only a check saved by a
// single write can be saved complete. The function and the trigger live in
// the test's own schema and are dropped with it.
const (
	updateRefused = "test: the database refuses every update of a check"
	refuseUpdates = `CREATE FUNCTION refuse_check_update() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION '` + updateRefused + `'; END $$;
		CREATE TRIGGER refuse_check_update BEFORE UPDATE ON checks
		FOR EACH ROW EXECUTE FUNCTION refuse_check_update()`
)

var baseTime = time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)

var voiceOutcome = Outcome{
	Code:          result.VoiceMessage,
	Text:          "Please leave a message after the tone.",
	RecordingPath: "/recordings/a.mp3",
}

// fakeDialer records every call. When release is set, each call blocks until
// the test sends on it.
type fakeDialer struct {
	outcome   Outcome
	release   chan struct{}
	entered   chan string
	panicCall bool

	mu       sync.Mutex
	numbers  []string
	events   []string
	inFlight int
	overlap  bool
}

func newFakeDialer(out Outcome, hold bool) *fakeDialer {
	d := &fakeDialer{outcome: out, entered: make(chan string, 4)}
	if hold {
		d.release = make(chan struct{})
	}
	return d
}

func (d *fakeDialer) Call(ctx context.Context, number string) Outcome {
	d.mu.Lock()
	d.numbers = append(d.numbers, number)
	d.events = append(d.events, "start "+number)
	d.inFlight++
	if d.inFlight > 1 {
		d.overlap = true
	}
	d.mu.Unlock()
	d.entered <- number
	if d.release != nil {
		<-d.release
	}
	d.mu.Lock()
	d.inFlight--
	d.events = append(d.events, "end "+number)
	d.mu.Unlock()
	if d.panicCall {
		panic("test dialer panic")
	}
	return d.outcome
}

func (d *fakeDialer) calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.numbers...)
}

// testClock is a fixed clock the test moves by hand.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = at
}

type fixture struct {
	store  *store.Store
	admin  *pgx.Conn
	clock  *testClock
	dialer *fakeDialer
	svc    *Service
}

// newFixture opens the real store on checknumber_test inside a fresh schema
// dropped when the test ends; the clock reads baseTime.
func newFixture(t *testing.T, d *fakeDialer) *fixture {
	t.Helper()
	ctx := context.Background()
	t.Setenv("PGDATABASE", testDatabase)
	admin, err := pgx.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect to %s: %v", testDatabase, err)
	}
	schema := fmt.Sprintf("check_test_%d", time.Now().UnixNano())
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
	if _, err := admin.Exec(ctx, "SET search_path TO "+ident); err != nil {
		t.Fatalf("set search_path %s: %v", schema, err)
	}

	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	st, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	clock := &testClock{at: baseTime}
	return &fixture{store: st, admin: admin, clock: clock, dialer: d, svc: NewService(st, d, clock.now)}
}

// seed stores a finished check whose call started at startedAt.
func (f *fixture) seed(t *testing.T, number string, code result.Code, reason string, startedAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := f.store.Begin(ctx, number, startedAt)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if err := f.store.MarkStarted(ctx, id, startedAt); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	if err := f.store.Finish(ctx, id, string(code), reason, "", "", startedAt.Add(time.Minute)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return id
}

func (f *fixture) rowCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.admin.QueryRow(context.Background(), "SELECT count(*) FROM checks").Scan(&n); err != nil {
		t.Fatalf("count checks: %v", err)
	}
	return n
}

func (f *fixture) history(t *testing.T, input string) []Answer {
	t.Helper()
	got, err := f.svc.History(context.Background(), input)
	if err != nil {
		t.Fatalf("History(%q): %v", input, err)
	}
	return got
}

// waitFor polls cond until it holds or waitLimit passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(pollEvery)
	}
}

func receive(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

type checkResult struct {
	answer Answer
	err    error
}

func (f *fixture) checkAsync(input string) <-chan checkResult {
	return f.checkAsyncCtx(context.Background(), input)
}

// checkAsyncCtx runs Check for a requester the test can make leave (D-1).
func (f *fixture) checkAsyncCtx(ctx context.Context, input string) <-chan checkResult {
	ch := make(chan checkResult, 1)
	go func() {
		a, err := f.svc.Check(ctx, input)
		ch <- checkResult{a, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan checkResult, input string) Answer {
	t.Helper()
	r := settle(t, ch, input)
	if r.err != nil {
		t.Fatalf("Check(%q): %v", input, r.err)
	}
	return r.answer
}

// settle waits for a request to return, whatever it returned.
func settle(t *testing.T, ch <-chan checkResult, input string) checkResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitLimit):
		t.Fatalf("timed out waiting for Check(%q)", input)
		return checkResult{}
	}
}

// waitForWaiter returns once one request waits on number's call in progress.
func (f *fixture) waitForWaiter(t *testing.T, number string) {
	t.Helper()
	waitFor(t, "second request to wait", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		fl, ok := f.svc.flights[number]
		return ok && fl.waiters == 1
	})
}

// onlyRow reads the single check of number from the database.
func (f *fixture) onlyRow(t *testing.T, number string) store.Check {
	t.Helper()
	checks, err := f.store.History(context.Background(), number)
	if err != nil {
		t.Fatalf("store.History(%q): %v", number, err)
	}
	if len(checks) != 1 {
		t.Fatalf("store.History(%q) has %d checks, want 1", number, len(checks))
	}
	return checks[0]
}

func TestRefusedNotSaved(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	ctx := context.Background()

	// A valid request first proves the store and dialer are live.
	if _, err := f.svc.Check(ctx, inputA); err != nil {
		t.Fatalf("Check(%q): %v", inputA, err)
	}
	if rows, calls := f.rowCount(t), len(f.dialer.calls()); rows != 1 || calls != 1 {
		t.Fatalf("after valid check: rows %d, dials %d; want 1, 1", rows, calls)
	}

	// TC-43: without a leading + the country cannot be told, and seven digits
	// are too few for a number of another country; both stay refused.
	for _, input := range []string{"", "123", "44 20 7946 0958", "+44 20 794"} {
		var invalid *phone.InvalidError
		if _, err := f.svc.Check(ctx, input); !errors.As(err, &invalid) {
			t.Errorf("Check(%q) error = %v, want *phone.InvalidError", input, err)
		}
		if _, err := f.svc.History(ctx, input); !errors.As(err, &invalid) {
			t.Errorf("History(%q) error = %v, want *phone.InvalidError", input, err)
		}
	}
	if rows, calls := f.rowCount(t), len(f.dialer.calls()); rows != 1 || calls != 1 {
		t.Errorf("after invalid checks: rows %d, dials %d; want 1, 1 (nothing new)", rows, calls)
	}
}

// internationalAnswer is the answer for numberAbroad saved as check id by a
// request made at at.
func internationalAnswer(id int64, at time.Time) Answer {
	return Answer{
		CheckID: id, Number: numberAbroad, Code: result.International, Label: "International",
		Reason: wantReasonInternational, CalledAt: at,
	}
}

func TestInternationalSavedNotDialled(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))

	got, err := f.svc.Check(context.Background(), inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q): %v", inputAbroad, err)
	}
	row := f.onlyRow(t, numberAbroad)
	if want := internationalAnswer(row.ID, baseTime); got != want {
		t.Errorf("Check(%q) = %+v, want %+v", inputAbroad, got, want)
	}
	// TC-43: the request time is the saved time.
	wantRow := store.Check{
		ID: row.ID, Number: numberAbroad, State: store.StateDone,
		ResultCode: string(result.International), Reason: wantReasonInternational,
		RequestedAt: baseTime, StartedAt: baseTime, FinishedAt: baseTime,
	}
	if row != wantRow {
		t.Errorf("saved check = %+v, want %+v", row, wantRow)
	}
	if rows, calls := f.rowCount(t), f.dialer.calls(); rows != 1 || len(calls) != 0 {
		t.Errorf("rows %d, dialled %v; want 1 row and no call", rows, calls)
	}
}

// R-28: the answer comes at once, also while a call holds the dial lock.
func TestInternationalDoesNotWaitForCall(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	first := f.checkAsync(inputA)
	receive(t, f.dialer.entered, "first dial")
	got := await(t, f.checkAsync(inputAbroad), inputAbroad)
	if want := internationalAnswer(f.onlyRow(t, numberAbroad).ID, baseTime); got != want {
		t.Errorf("Check(%q) during a call = %+v, want %+v", inputAbroad, got, want)
	}

	close(f.dialer.release)
	await(t, first, inputA)
	if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Errorf("dialled %v, want only the call to %s", calls, numberA)
	}
}

func TestInternationalReused(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	ctx := context.Background()

	first, err := f.svc.Check(ctx, inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q): %v", inputAbroad, err)
	}
	if first.Saved || first.Code != result.International {
		t.Fatalf("first Check(%q) = %+v, want a new International check", inputAbroad, first)
	}

	saved := internationalAnswer(first.CheckID, baseTime)
	saved.Saved = true
	for _, age := range []time.Duration{10 * day, reuseLimit} {
		f.clock.set(baseTime.Add(age))
		// The number written another way finds the same saved result.
		got, err := f.svc.Check(ctx, numberAbroad)
		if err != nil {
			t.Fatalf("Check(%q) after %v: %v", numberAbroad, age, err)
		}
		if got != saved {
			t.Errorf("Check(%q) after %v = %+v, want %+v", numberAbroad, age, got, saved)
		}
		if rows := f.rowCount(t); rows != 1 {
			t.Errorf("after %v: %d rows, want 1 (no row added)", age, rows)
		}
	}

	later := baseTime.Add(reuseLimit + time.Minute)
	f.clock.set(later)
	got, err := f.svc.Check(ctx, inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q) after the window: %v", inputAbroad, err)
	}
	if want := internationalAnswer(got.CheckID, later); got != want || got.CheckID == first.CheckID {
		t.Errorf("Check(%q) after the window = %+v, want %+v with a new check id", inputAbroad, got, want)
	}
	if rows := f.rowCount(t); rows != 2 {
		t.Errorf("after the window: %d rows, want 2 (one row added)", rows)
	}
	if calls := f.dialer.calls(); len(calls) != 0 {
		t.Errorf("dialled %v, want no calls", calls)
	}
}

func TestInternationalConcurrentReused(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	entered := make(chan string, 1)
	release := make(chan struct{})
	var once sync.Once
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	f.svc.now = func() time.Time {
		// Hold the first request before Begin, so the second must join its flight.
		once.Do(func() {
			entered <- numberAbroad
			<-release
		})
		return f.clock.now()
	}

	first := f.checkAsync(inputAbroad)
	receive(t, entered, "international request before save")
	second := f.checkAsync(numberAbroad)
	f.waitForWaiter(t, numberAbroad)
	unblock.Do(func() { close(release) })

	a1, a2 := await(t, first, inputAbroad), await(t, second, numberAbroad)
	want := internationalAnswer(a1.CheckID, baseTime)
	if a1 != want {
		t.Errorf("first answer = %+v, want %+v", a1, want)
	}
	want.Saved = true
	if a2 != want {
		t.Errorf("second answer = %+v, want reused answer %+v", a2, want)
	}
	row := f.onlyRow(t, numberAbroad)
	if row.ID != a1.CheckID || row.State != store.StateDone || row.ResultCode != string(result.International) {
		t.Errorf("saved check = %+v, want the shared done International check %d", row, a1.CheckID)
	}
	if rows, calls := f.rowCount(t), f.dialer.calls(); rows != 1 || len(calls) != 0 {
		t.Errorf("rows %d, dialled %v; want 1 row and no call", rows, calls)
	}
}

// internationalRow is the row of numberAbroad saved as check id by a request
// made at at (TC-43: the request time is every time of the row).
func internationalRow(id int64, at time.Time) store.Check {
	return store.Check{
		ID: id, Number: numberAbroad, State: store.StateDone,
		ResultCode: string(result.International), Reason: wantReasonInternational,
		RequestedAt: at, StartedAt: at, FinishedAt: at,
	}
}

// holdFirstClockRead makes the service's first clock read wait until release
// is called; entered receives once that read has begun. The first request for
// a number with no saved check reads the clock after its lookup and before
// its save.
func (f *fixture) holdFirstClockRead(t *testing.T) (entered <-chan string, release func()) {
	t.Helper()
	reading := make(chan string, 1)
	gate := make(chan struct{})
	var first, open sync.Once
	release = func() { open.Do(func() { close(gate) }) }
	t.Cleanup(release)
	f.svc.now = func() time.Time {
		first.Do(func() {
			reading <- "clock read"
			<-gate
		})
		return f.clock.now()
	}
	return reading, release
}

// A check saved by several writes can stop between them and stay unfinished;
// start-up then closes it as Unknown. Here the database refuses every write
// after the first, so only a check saved by one write is saved at all.
func TestInternationalSavedInOneWrite(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	ctx := context.Background()
	// Saved before the refusal exists; the refused change of it proves below
	// that the database does refuse.
	seeded := f.seed(t, numberA, result.Fax, "", baseTime.Add(-day))
	if _, err := f.admin.Exec(ctx, refuseUpdates); err != nil {
		t.Fatalf("make the database refuse updates of checks: %v", err)
	}
	if err := f.store.MarkStarted(ctx, seeded, baseTime); err == nil || !strings.Contains(err.Error(), updateRefused) {
		t.Fatalf("MarkStarted(%d) error = %v, want %q", seeded, err, updateRefused)
	}

	got, err := f.svc.Check(ctx, inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q) while every update is refused: %v", inputAbroad, err)
	}
	row := f.onlyRow(t, numberAbroad)
	if want := internationalAnswer(row.ID, baseTime); got != want {
		t.Errorf("Check(%q) = %+v, want %+v", inputAbroad, got, want)
	}
	if want := internationalRow(row.ID, baseTime); row != want {
		t.Errorf("saved check = %+v, want %+v", row, want)
	}
	// TC-20, the start-up step: no check is left for it to close as Unknown.
	closed, err := f.store.MarkUnfinished(ctx, string(result.Unknown), restarted, baseTime.Add(time.Minute))
	if err != nil || closed != 0 {
		t.Errorf("MarkUnfinished after the check = (%d, %v), want (0, nil): no unfinished check", closed, err)
	}
	if after := f.onlyRow(t, numberAbroad); after != row {
		t.Errorf("check after the start-up step = %+v, want it unchanged %+v", after, row)
	}
	if rows, calls := f.rowCount(t), f.dialer.calls(); rows != 2 || len(calls) != 0 {
		t.Errorf("rows %d, dialled %v; want 2 rows (the seeded one and this one) and no call", rows, calls)
	}
}

// The save has its own limit: when the first requester leaves before the
// write, the check is still saved and a request waiting on it still gets it.
func TestInternationalSavedWhenRequesterLeaves(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	entered, release := f.holdFirstClockRead(t)

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	first := f.checkAsyncCtx(ctx, inputAbroad)
	receive(t, entered, "first request to reach its save")
	second := f.checkAsync(numberAbroad)
	f.waitForWaiter(t, numberAbroad)
	leave()
	release()

	got := await(t, second, numberAbroad)
	settle(t, first, inputAbroad)
	row := f.onlyRow(t, numberAbroad)
	want := internationalAnswer(row.ID, baseTime)
	want.Saved = true
	if got != want {
		t.Errorf("waiting Check(%q) = %+v, want the saved check %+v", numberAbroad, got, want)
	}
	if wantRow := internationalRow(row.ID, baseTime); row != wantRow {
		t.Errorf("saved check = %+v, want %+v", row, wantRow)
	}
	if rows, calls := f.rowCount(t), f.dialer.calls(); rows != 1 || len(calls) != 0 {
		t.Errorf("rows %d, dialled %v; want 1 row and no call", rows, calls)
	}
}

func TestInternationalWaiterSurvivesLookupCancellation(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("saved=%t", existing), func(t *testing.T) {
			f := newFixture(t, newFakeDialer(voiceOutcome, false))
			ctx := context.Background()
			var saved Answer
			if existing {
				var err error
				saved, err = f.svc.Check(ctx, inputAbroad)
				if err != nil {
					t.Fatalf("seed International result: %v", err)
				}
			}

			// Force cancellation during the SELECT, before any result or save,
			// by holding the test schema's table until a live waiter has joined.
			tx, err := f.admin.Begin(ctx)
			if err != nil {
				t.Fatalf("begin lookup barrier: %v", err)
			}
			defer func() {
				if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Errorf("release lookup barrier: %v", err)
				}
			}()
			if _, err := tx.Exec(ctx, "LOCK TABLE checks IN ACCESS EXCLUSIVE MODE"); err != nil {
				t.Fatalf("hold lookup barrier: %v", err)
			}
			requestCtx, leave := context.WithCancel(ctx)
			defer leave()
			first := f.checkAsyncCtx(requestCtx, inputAbroad)
			waitFor(t, "lookup blocked by table lock", func() bool {
				var blocked bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_locks WHERE relation = 'checks'::regclass
					AND mode = 'AccessShareLock' AND NOT granted
					AND pid <> pg_backend_pid())`).Scan(&blocked); err != nil {
					t.Fatalf("observe lookup barrier: %v", err)
				}
				return blocked
			})
			second := f.checkAsync(numberAbroad)
			f.waitForWaiter(t, numberAbroad)
			leave()
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("release lookup barrier: %v", err)
			}

			got := await(t, second, numberAbroad)
			await(t, first, inputAbroad)
			row := f.onlyRow(t, numberAbroad)
			want := internationalAnswer(row.ID, baseTime)
			want.Saved = true
			if got != want {
				t.Errorf("live waiter = %+v, want %+v", got, want)
			}
			if existing && got.CheckID != saved.CheckID {
				t.Errorf("reused id %d, want existing id %d", got.CheckID, saved.CheckID)
			}
			if wantRow := internationalRow(row.ID, baseTime); row != wantRow {
				t.Errorf("saved row = %+v, want %+v", row, wantRow)
			}
			if rows, calls := f.rowCount(t), f.dialer.calls(); rows != 1 || len(calls) != 0 {
				t.Errorf("rows %d, dialled %v; want 1 row and no call", rows, calls)
			}
		})
	}
}

func TestInternationalHistory(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))
	ctx := context.Background()
	// The check of another number must not be listed.
	f.seed(t, numberA, result.Fax, "", baseTime.Add(-day))

	first, err := f.svc.Check(ctx, inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q): %v", inputAbroad, err)
	}
	later := baseTime.Add(reuseLimit + time.Minute)
	f.clock.set(later)
	second, err := f.svc.Check(ctx, inputAbroad)
	if err != nil {
		t.Fatalf("Check(%q) after the window: %v", inputAbroad, err)
	}
	if first.CheckID == second.CheckID {
		t.Fatalf("both checks have id %d, want two checks", first.CheckID)
	}

	want := []Answer{
		internationalAnswer(second.CheckID, later),
		internationalAnswer(first.CheckID, baseTime),
	}
	for _, input := range []string{inputAbroad, numberAbroad} {
		if got := f.history(t, input); !reflect.DeepEqual(got, want) {
			t.Errorf("History(%q) = %+v, want %+v", input, got, want)
		}
	}
}

// wantReused checks number, seeded as a fax whose call started age ago, and
// expects that saved result back with nothing dialled and no row added.
func wantReused(t *testing.T, f *fixture, number string, age time.Duration) {
	t.Helper()
	startedAt := baseTime.Add(-age)
	id := f.seed(t, number, result.Fax, "", startedAt)

	got, err := f.svc.Check(context.Background(), number)
	if err != nil {
		t.Fatalf("Check(%q): %v", number, err)
	}
	want := Answer{
		CheckID: id, Number: number, Code: result.Fax, Label: "Fax",
		CalledAt: startedAt, Saved: true,
	}
	if got != want {
		t.Errorf("Check(%q) = %+v, want %+v", number, got, want)
	}
	if h := f.history(t, number); len(h) != 1 {
		t.Errorf("History(%q) has %d checks, want 1 (no copy saved)", number, len(h))
	}
	if calls := f.dialer.calls(); len(calls) != 0 {
		t.Errorf("dialled %v, want no calls", calls)
	}
}

func TestReuseWithinWindow(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, false))

	for _, age := range []time.Duration{10 * day, 45 * day} {
		t.Run(age.String(), func(t *testing.T) {
			wantReused(t, f, fmt.Sprintf("+1555223%04d", int(age/day)), age)
		})
	}
}

func TestReuseBoundary(t *testing.T) {
	t.Run("exactly at the limit", func(t *testing.T) {
		wantReused(t, newFixture(t, newFakeDialer(voiceOutcome, false)), numberA, reuseLimit)
	})

	t.Run("one minute older", func(t *testing.T) {
		f := newFixture(t, newFakeDialer(voiceOutcome, false))
		oldStart := baseTime.Add(-reuseLimit - time.Minute)
		oldID := f.seed(t, numberA, result.Fax, "", oldStart)

		got, err := f.svc.Check(context.Background(), inputA)
		if err != nil {
			t.Fatalf("Check(%q): %v", inputA, err)
		}
		if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
			t.Fatalf("dialled %v, want [%s]", calls, numberA)
		}
		want := Answer{
			CheckID: got.CheckID, Number: numberA, Code: result.VoiceMessage, Label: "Voice message",
			Text: voiceOutcome.Text, RecordingPath: voiceOutcome.RecordingPath, CalledAt: baseTime,
		}
		if got != want || got.CheckID == oldID {
			t.Errorf("Check(%q) = %+v, want %+v with a new check id", inputA, got, want)
		}

		h := f.history(t, inputA)
		wantHistory := []Answer{
			want,
			{CheckID: oldID, Number: numberA, Code: result.Fax, Label: "Fax", CalledAt: oldStart},
		}
		if !reflect.DeepEqual(h, wantHistory) {
			t.Errorf("History(%q) = %+v, want %+v", inputA, h, wantHistory)
		}
	})
}

func TestUnknownAlwaysDialled(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *fixture)
	}{
		{"unknown call 1 day ago", func(t *testing.T, f *fixture) {
			f.seed(t, numberA, result.Unknown, reasonDial, baseTime.Add(-day))
		}},
		{"unknown call 45 days ago", func(t *testing.T, f *fixture) {
			f.seed(t, numberA, result.Unknown, reasonDial, baseTime.Add(-45*day))
		}},
		{"restart unknown wins a tied request timestamp", func(t *testing.T, f *fixture) {
			ctx := context.Background()
			requestedAt := baseTime.Add(-2 * day)
			f.seed(t, numberA, result.Fax, "", requestedAt)
			// PostgreSQL timestamps can tie; History orders the newer id first.
			if _, err := f.store.Begin(ctx, numberA, requestedAt); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := f.store.MarkUnfinished(ctx, string(result.Unknown), restarted, baseTime.Add(-day)); err != nil {
				t.Fatalf("MarkUnfinished: %v", err)
			}
		}},
		{"closed unknown at restart after a fax 10 days ago", func(t *testing.T, f *fixture) {
			ctx := context.Background()
			f.seed(t, numberA, result.Fax, "", baseTime.Add(-10*day))
			if _, err := f.store.Begin(ctx, numberA, baseTime.Add(-2*day)); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			if _, err := f.store.MarkUnfinished(ctx, string(result.Unknown), restarted, baseTime.Add(-day)); err != nil {
				t.Fatalf("MarkUnfinished: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, newFakeDialer(voiceOutcome, false))
			tc.setup(t, f)
			before := len(f.history(t, inputA))

			got, err := f.svc.Check(context.Background(), inputA)
			if err != nil {
				t.Fatalf("Check(%q): %v", inputA, err)
			}
			if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
				t.Errorf("dialled %v, want [%s]", calls, numberA)
			}
			if got.Saved || got.Code != result.VoiceMessage || !got.CalledAt.Equal(baseTime) {
				t.Errorf("Check(%q) = %+v, want a new voice message call at %v", inputA, got, baseTime)
			}
			if h := f.history(t, inputA); len(h) != before+1 || h[0].CheckID != got.CheckID {
				t.Errorf("History(%q) = %+v, want %d checks with the new one first", inputA, h, before+1)
			}
		})
	}
}

func TestRepeatRequestWaits(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	first := f.checkAsync(inputA)
	receive(t, f.dialer.entered, "first dial")
	second := f.checkAsync(numberA)
	waitFor(t, "second request to wait", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		fl, ok := f.svc.flights[numberA]
		return ok && fl.waiters == 1
	})
	close(f.dialer.release)

	a1, a2 := await(t, first, inputA), await(t, second, numberA)
	if a1 != a2 {
		t.Errorf("answers differ: first %+v, second %+v", a1, a2)
	}
	if a1.Saved || a1.Code != result.VoiceMessage {
		t.Errorf("answer = %+v, want the new voice message call", a1)
	}
	if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Errorf("dialled %v, want one call to %s", calls, numberA)
	}
	if h := f.history(t, numberA); len(h) != 1 {
		t.Errorf("History has %d checks, want 1", len(h))
	}
}

func TestOneCallAtATime(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	first := f.checkAsync(inputA)
	receive(t, f.dialer.entered, "first dial")
	second := f.checkAsync(inputB)
	// B's row exists once its request is past Begin and at the dial lock.
	waitFor(t, "second request to be saved", func() bool {
		h, err := f.svc.History(context.Background(), inputB)
		return err == nil && len(h) == 1
	})
	select {
	case n := <-f.dialer.entered:
		t.Fatalf("dialled %s while the call to %s was in progress", n, numberA)
	case <-time.After(overlapWatch):
	}
	if h := f.history(t, inputB); !h[0].CalledAt.IsZero() {
		t.Errorf("second check started at %v while the first call was in progress", h[0].CalledAt)
	}

	f.dialer.release <- struct{}{}
	await(t, first, inputA)
	receive(t, f.dialer.entered, "second dial")
	f.dialer.release <- struct{}{}
	await(t, second, inputB)

	f.dialer.mu.Lock()
	defer f.dialer.mu.Unlock()
	wantEvents := []string{"start " + numberA, "end " + numberA, "start " + numberB, "end " + numberB}
	if f.dialer.overlap || !reflect.DeepEqual(f.dialer.events, wantEvents) {
		t.Errorf("dial events %v (overlap %v), want %v", f.dialer.events, f.dialer.overlap, wantEvents)
	}
}

func TestDialerPanicReleasesWaiters(t *testing.T) {
	d := newFakeDialer(voiceOutcome, true)
	d.panicCall = true
	f := newFixture(t, d)
	first := f.checkAsync(inputA)
	receive(t, d.entered, "first dial")
	second := f.checkAsync(numberA)
	waitFor(t, "second request to wait", func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		fl, ok := f.svc.flights[numberA]
		return ok && fl.waiters == 1
	})
	close(d.release)

	wantError := "check " + numberA + ": call panicked"
	for i, ch := range []<-chan checkResult{first, second} {
		select {
		case got := <-ch:
			if got.answer != (Answer{}) || got.err == nil || got.err.Error() != wantError {
				t.Errorf("request %d = %+v, %v; want empty Answer and %q", i, got.answer, got.err, wantError)
			}
		case <-time.After(waitLimit):
			t.Fatalf("request %d stranded after dialer panic", i)
		}
	}
	if calls := d.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Fatalf("dialled %v, want one call to %s", calls, numberA)
	}
	f.svc.mu.Lock()
	_, stuck := f.svc.flights[numberA]
	f.svc.mu.Unlock()
	if stuck {
		t.Fatal("number remains in wait list after dialer panic")
	}

	// The failed dependency must not prevent a later request from dialling.
	d.panicCall = false
	got, err := f.svc.Check(context.Background(), numberA)
	if err != nil || got.Code != result.VoiceMessage || got.Saved || len(d.calls()) != 2 {
		t.Errorf("retry = %+v, %v; calls %v; want a new voice call", got, err, d.calls())
	}
}

// D-1: the requester leaves during the call; the paid call's result is saved.
func TestResultSavedWhenRequesterLeaves(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	first := f.checkAsyncCtx(ctx, inputA)
	receive(t, f.dialer.entered, "dial")
	leave()
	close(f.dialer.release)
	settle(t, first, inputA)

	row := f.onlyRow(t, numberA)
	if row.State != store.StateDone || row.ResultCode != string(result.VoiceMessage) ||
		row.Text != voiceOutcome.Text || row.RecordingPath != voiceOutcome.RecordingPath ||
		!row.StartedAt.Equal(baseTime) {
		t.Errorf("saved check = %+v, want done with the voice message call started at %v", row, baseTime)
	}

	again, err := f.svc.Check(context.Background(), inputA)
	if err != nil {
		t.Fatalf("next Check(%q): %v", inputA, err)
	}
	if !again.Saved || again.CheckID != row.ID || again.Code != result.VoiceMessage {
		t.Errorf("next Check(%q) = %+v, want the saved voice message of check %d", inputA, again, row.ID)
	}
	if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Errorf("dialled %v, want one call to %s", calls, numberA)
	}
}

// D-1, AD-5: the waiter's own context is alive, so the first requester
// leaving must not cost it the result.
func TestWaiterGetsResultWhenFirstLeaves(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	first := f.checkAsyncCtx(ctx, inputA)
	receive(t, f.dialer.entered, "first dial")
	second := f.checkAsync(numberA)
	f.waitForWaiter(t, numberA)
	leave()
	close(f.dialer.release)

	got := await(t, second, numberA)
	settle(t, first, inputA)
	h := f.history(t, numberA)
	if len(h) != 1 || got != h[0] || got.Code != result.VoiceMessage || got.Text != voiceOutcome.Text {
		t.Errorf("waiter got %+v, want the one saved voice message check; History = %+v", got, h)
	}
	if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Errorf("dialled %v, want one call to %s", calls, numberA)
	}
}

func TestCancelledInQueueNotLeftInProgress(t *testing.T) {
	f := newFixture(t, newFakeDialer(voiceOutcome, true))

	first := f.checkAsync(inputA)
	receive(t, f.dialer.entered, "first dial")
	ctx, leave := context.WithCancel(context.Background())
	defer leave()
	queued := f.checkAsyncCtx(ctx, inputB)
	// B's row exists once its request is past Begin and at the dial lock.
	waitFor(t, "queued request to be saved", func() bool {
		h, err := f.svc.History(context.Background(), inputB)
		return err == nil && len(h) == 1
	})
	leave()
	close(f.dialer.release)
	await(t, first, inputA)

	got := settle(t, queued, inputB)
	if got.answer != (Answer{}) || !errors.Is(got.err, context.Canceled) {
		t.Errorf("cancelled Check(%q) = %+v, %v; want empty Answer and context.Canceled", inputB, got.answer, got.err)
	}
	if calls := f.dialer.calls(); !reflect.DeepEqual(calls, []string{numberA}) {
		t.Errorf("dialled %v, want only the call to %s", calls, numberA)
	}
	row := f.onlyRow(t, numberB)
	if row.State != store.StateDone || row.ResultCode != string(result.Unknown) ||
		row.Reason != wantReasonCancelled || !row.StartedAt.IsZero() {
		t.Errorf("cancelled check = %+v, want done, unknown, reason %q, never started", row, wantReasonCancelled)
	}
}
