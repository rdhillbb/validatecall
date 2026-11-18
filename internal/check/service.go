// Package check decides, per request, whether a number is dialled or its saved
// result is returned, and places at most one call at a time.
package check

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"checknumber/internal/phone"
	"checknumber/internal/result"
	"checknumber/internal/store"
)

// reuseWindow is TC-16: a result is reused when its call started at most
// 60 x 24 hours before the request (was 30 until 2026-11-14; the module is
// only called after a failed fax).
const reuseWindow = 60 * 24 * time.Hour

// saveLimit is TC-34: every save of a check that is past Begin has its own
// limit instead of the requester's, who may leave at any time (D-1).
const saveLimit = 10 * time.Second

// reasonCancelled is saved with Unknown when the requester left before its
// turn to dial came.
const reasonCancelled = "not called: the request was cancelled"

// The check line of R-29 (TC-48), one per request. Free text is Go-quoted so a
// quote or a line break in it can never split or fake a line; the typed input
// is never written, so a key pasted by mistake stays out of the log (C-4).
const (
	lineAnswered = "check number=%s result=%s source=%s took=%.1fs"
	lineReason   = " reason=%q"
	lineFailed   = "check failed number=%s reason=%q took=%.1fs"
	lineRefused  = "check refused reason=%q took=%.1fs"
)

// source says where the answer of a check line came from (TC-48).
type source string

const (
	sourceCall   source = "call"   // this request dialled or tried to dial
	sourceShared source = "shared" // it waited for another request's call (AD-5)
	sourceSaved  source = "saved"  // a saved result, also an International one
	sourceNone   source = "none"   // an International number saved now
)

// Dialer places one call and reports its result. Tests use a fake (C-5).
type Dialer interface {
	Call(ctx context.Context, number string) Outcome
}

// Outcome is what one call produced.
type Outcome struct {
	Code          result.Code
	Reason        string
	Text          string
	RecordingPath string
	StartedAt     time.Time
}

// Answer is the result returned for a request. Saved is true when the result
// came from an earlier call and no call was placed (R-18).
type Answer struct {
	CheckID       int64
	Number        string
	Code          result.Code
	Label         string
	Reason        string
	Text          string
	RecordingPath string
	CalledAt      time.Time
	Saved         bool
}

// flight is one request in progress for a number; later requests for the same
// number wait on done and share its answer (AD-5).
type flight struct {
	done    chan struct{}
	answer  Answer
	err     error
	waiters int // requests waiting on done; read by tests to know one is waiting
}

// Service runs checks against the store and the dialer.
type Service struct {
	store  *store.Store
	dialer Dialer
	now    func() time.Time

	// dial is held for the whole call so calls never overlap (AD-2).
	dial sync.Mutex

	mu      sync.Mutex
	flights map[string]*flight

	// logf writes the check lines; tests replace it to read them without
	// touching the global logger other packages' tests capture.
	logf func(format string, args ...any)
}

// NewService returns a Service; now is the clock (a fixed function in tests).
func NewService(st *store.Store, d Dialer, now func() time.Time) *Service {
	return &Service{store: st, dialer: d, now: now, flights: map[string]*flight{}, logf: log.Printf}
}

// Check returns the result for input. An invalid number returns the
// *phone.InvalidError and nothing is saved or dialled (R-24). A number of
// another country is saved as International and never dialled (R-28).
func (s *Service) Check(ctx context.Context, input string) (answer Answer, err error) {
	number, international, err := numberOf(input)
	if err != nil {
		s.logRefused(err, s.now())
		return Answer{}, err
	}

	// The clock is read only once this request holds or has joined the
	// number's flight: a clock that blocks (as in the tests) must never keep a
	// second request from joining. Registered first, so it runs after the
	// panic recovery below and sees the request's final answer and error.
	var start time.Time
	waited := false
	defer func() { s.logCheck(number, answer, err, waited, start) }()

	s.mu.Lock()
	if f, ok := s.flights[number]; ok {
		f.waiters++
		s.mu.Unlock()
		waited = true
		start = s.now()
		select {
		case <-f.done:
			answer := f.answer
			if international && f.err == nil {
				// No call is shared here: this request reuses the check just saved.
				answer.Saved = true
			}
			return answer, f.err
		case <-ctx.Done():
			return Answer{}, fmt.Errorf("check %s: wait for call in progress: %w", number, ctx.Err())
		}
	}
	f := &flight{done: make(chan struct{})}
	s.flights[number] = f
	s.mu.Unlock()

	defer func() {
		// A dialer panic must not leave this number's waiters stranded forever.
		if recover() != nil {
			f.err = fmt.Errorf("check %s: call panicked", number)
		}
		s.mu.Lock()
		delete(s.flights, number)
		close(f.done)
		s.mu.Unlock()
		answer, err = f.answer, f.err
	}()
	start = s.now()
	f.answer, f.err = s.check(ctx, number, international)

	return f.answer, f.err
}

// logCheck writes the line of a request that reached the check. waited is
// true when the request waited on another request's flight.
func (s *Service) logCheck(number string, a Answer, err error, waited bool, start time.Time) {
	took := s.now().Sub(start).Seconds()
	if err != nil {
		// Only the error's type: its text may hold more than the line needs.
		s.logf(lineFailed, number, fmt.Sprintf("%T", err), took)
		return
	}
	src := sourceCall
	switch {
	case a.Saved:
		src = sourceSaved
	case waited:
		src = sourceShared
	case a.Code == result.International:
		src = sourceNone
	}
	if a.Reason == "" {
		s.logf(lineAnswered, number, a.Code, src, took)
		return
	}
	s.logf(lineAnswered+lineReason, number, a.Code, src, took, a.Reason)
}

// logRefused writes the line of a refused input (R-24). The error text of
// phone.Normalize quotes the input, so only the message shown to the person is
// written.
func (s *Service) logRefused(err error, start time.Time) {
	message := fmt.Sprintf("%T", err)
	var invalid *phone.InvalidError
	if errors.As(err, &invalid) {
		message = invalid.Message
	}
	s.logf(lineRefused, message, s.now().Sub(start).Seconds())
}

// numberOf returns input in the form it is saved under: a +1 number, or the
// number of another country (international true), which is saved and never
// dialled (R-28). Any other input returns the *phone.InvalidError of Normalize.
func numberOf(input string) (number string, international bool, err error) {
	number, err = phone.Normalize(input)
	if err == nil {
		return number, false, nil
	}
	if abroad, ok := phone.International(input); ok {
		return abroad, true, nil
	}
	return "", false, err
}

func (s *Service) check(ctx context.Context, number string, international bool) (Answer, error) {
	if international {
		// A live waiter must not lose this no-call result when the first
		// requester leaves during the lookup (R-28, AD-5).
		lookupCtx, cancel := saveContext(ctx)
		defer cancel()
		ctx = lookupCtx
	}
	reuse, ok, err := s.reusable(ctx, number)
	if err != nil {
		return Answer{}, err
	}
	if ok {
		return toAnswer(reuse, true), nil
	}
	if international {
		return s.saveInternational(ctx, number)
	}

	id, err := s.store.Begin(ctx, number, s.now())
	if err != nil {
		return Answer{}, fmt.Errorf("check %s: %w", number, err)
	}

	s.dial.Lock()
	defer s.dial.Unlock()

	if cause := ctx.Err(); cause != nil {
		// Nobody waits for this call any more, so it is not placed; the row is
		// closed here because nothing else would close it before a restart.
		saveCtx, cancel := saveContext(ctx)
		defer cancel()
		saveErr := s.store.Finish(saveCtx, id, string(result.Unknown), reasonCancelled, "", "", s.now())
		return Answer{}, fmt.Errorf("check %s: not called: %w", number, errors.Join(cause, saveErr))
	}

	startedAt := s.now().UTC()
	startCtx, cancelStart := saveContext(ctx)
	err = s.store.MarkStarted(startCtx, id, startedAt)
	cancelStart()
	if err != nil {
		return Answer{}, fmt.Errorf("check %s: %w", number, err)
	}
	out := s.dialer.Call(ctx, number)
	finishCtx, cancelFinish := saveContext(ctx)
	defer cancelFinish()
	if err := s.store.Finish(finishCtx, id, string(out.Code), out.Reason, out.Text, out.RecordingPath, s.now()); err != nil {
		return Answer{}, fmt.Errorf("check %s: %w", number, err)
	}
	return Answer{
		CheckID:       id,
		Number:        number,
		Code:          out.Code,
		Label:         result.Label(out.Code),
		Reason:        out.Reason,
		Text:          out.Text,
		RecordingPath: out.RecordingPath,
		CalledAt:      startedAt,
	}, nil
}

// saveInternational saves number as a new International check (R-28). It
// takes neither the dial lock nor the dialer, so it never waits for a call in
// progress.
//
// The check is saved by one write (store.SaveDone), not by Begin, MarkStarted
// and Finish: a stop or a database failure between those three would leave the
// row unfinished, and start-up would close it as Unknown instead of
// International. The write has its own limit, so a requester who leaves cannot
// interrupt it and the requests waiting on this one still get the saved check.
func (s *Service) saveInternational(ctx context.Context, number string) (Answer, error) {
	// TC-43: the request time is saved as the start time, the time the reuse
	// window reads.
	at := s.now().UTC()
	saveCtx, cancel := saveContext(ctx)
	defer cancel()
	id, err := s.store.SaveDone(saveCtx, number, string(result.International), result.ReasonInternational, at)
	if err != nil {
		return Answer{}, fmt.Errorf("check %s: %w", number, err)
	}
	return Answer{
		CheckID:  id,
		Number:   number,
		Code:     result.International,
		Label:    result.Label(result.International),
		Reason:   result.ReasonInternational,
		CalledAt: at,
	}, nil
}

// saveContext keeps ctx's values but not its cancellation, and adds saveLimit.
func saveContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), saveLimit)
}

// reusable returns the last done check when its result may be returned
// without dialling (R-18, R-21, TC-16).
func (s *Service) reusable(ctx context.Context, number string) (store.Check, bool, error) {
	last, ok, err := s.store.LastDone(ctx, number)
	if err != nil {
		return store.Check{}, false, fmt.Errorf("check %s: %w", number, err)
	}
	if !ok || !result.Reusable(result.Code(last.ResultCode)) || s.now().Sub(last.StartedAt) > reuseWindow {
		return store.Check{}, false, nil
	}

	// LastDone sorts a check closed before it started (TC-20, "app restarted")
	// after every started one, so a newer unknown from a restart would be
	// skipped; R-21 requires it to be dialled again.
	checks, err := s.store.History(ctx, number)
	if err != nil {
		return store.Check{}, false, fmt.Errorf("check %s: %w", number, err)
	}
	for _, c := range checks {
		// Match History's id tie-break when request timestamps have equal precision.
		newer := c.RequestedAt.After(last.RequestedAt) ||
			(c.RequestedAt.Equal(last.RequestedAt) && c.ID > last.ID)
		if newer && c.State == store.StateDone && !result.Reusable(result.Code(c.ResultCode)) {
			return store.Check{}, false, nil
		}
	}
	return last, true, nil
}

// History returns every check of the number in input, newest first (R-16).
func (s *Service) History(ctx context.Context, input string) ([]Answer, error) {
	number, _, err := numberOf(input)
	if err != nil {
		return nil, err
	}
	checks, err := s.store.History(ctx, number)
	if err != nil {
		return nil, fmt.Errorf("check history %s: %w", number, err)
	}
	answers := make([]Answer, 0, len(checks))
	for _, c := range checks {
		answers = append(answers, toAnswer(c, false))
	}
	return answers, nil
}

func toAnswer(c store.Check, saved bool) Answer {
	code := result.Code(c.ResultCode)
	return Answer{
		CheckID:       c.ID,
		Number:        c.Number,
		Code:          code,
		Label:         result.Label(code),
		Reason:        c.Reason,
		Text:          c.Text,
		RecordingPath: c.RecordingPath,
		CalledAt:      c.StartedAt,
		Saved:         saved,
	}
}
