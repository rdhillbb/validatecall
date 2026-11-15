package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"checknumber/internal/check"
	"checknumber/internal/phone"
	"checknumber/internal/result"
)

// C-5: the API is tested against fakeChecker; nothing is dialled.
const (
	apiNumber     = "+15552230101"
	apiTypedInput = "(555) 223-0101"
	apiReason     = "phone company message"
	apiText       = "The number you have dialled is not in service."
	// The contract sets no request-size ceiling; padding must not change valid input.
	largeBodyPadding = (1 << 20) + 1
)

var (
	// callZone is not UTC so a reply that forgets to convert is caught.
	callZone      = time.FixedZone("ICT", 7*60*60)
	firstCallAt   = time.Date(2026, 11, 13, 23, 41, 26, 0, callZone)
	firstCallUTC  = "2026-11-13T16:41:26Z"
	laterCallAt   = time.Date(2026, 11, 20, 9, 0, 0, 0, callZone)
	errStoreBroke = errors.New("store unavailable")
)

// fakeChecker behaves like check.Service at the API boundary: it refuses an
// invalid number before anything is dialled, dials a new number once, and
// returns the saved answer with Saved set on a later request.
type fakeChecker struct {
	mu      sync.Mutex
	next    check.Answer // the answer of the next call placed
	err     error
	saved   map[string]check.Answer
	history map[string][]check.Answer
	dialled []string
}

func newFakeChecker() *fakeChecker {
	return &fakeChecker{
		next: check.Answer{
			Code:     result.VoiceMessage,
			Label:    result.Label(result.VoiceMessage),
			Reason:   apiReason,
			Text:     apiText,
			CalledAt: firstCallAt,
		},
		saved:   map[string]check.Answer{},
		history: map[string][]check.Answer{},
	}
}

func (f *fakeChecker) Check(_ context.Context, input string) (check.Answer, error) {
	number, err := phone.Normalize(input)
	if err != nil {
		return check.Answer{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return check.Answer{}, f.err
	}
	if a, ok := f.saved[number]; ok {
		a.Saved = true
		return a, nil
	}
	a := f.next
	a.Number = number
	f.dialled = append(f.dialled, number)
	f.saved[number] = a
	return a, nil
}

func (f *fakeChecker) History(_ context.Context, input string) ([]check.Answer, error) {
	number, err := phone.Normalize(input)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.history[number], nil
}

func (f *fakeChecker) dialledNumbers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialled...)
}

func postCheck(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/check", strings.NewReader(body)))
	return rec
}

func checkBody(t *testing.T, input string) string {
	t.Helper()
	b, err := json.Marshal(checkRequest{Number: input})
	if err != nil {
		t.Fatalf("marshal check request %q: %v", input, err)
	}
	return string(b)
}

func decodeReply[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if got := rec.Header().Get(contentType); got != contentJSON {
		t.Fatalf("Content-Type = %q, want %q", got, contentJSON)
	}
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode reply %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestCheckReply(t *testing.T) {
	f := newFakeChecker()
	rec := postCheck(t, NewAPI(f), checkBody(t, apiTypedInput))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	got := decodeReply[map[string]any](t, rec)
	want := map[string]any{
		"number":       apiNumber,
		"result_code":  string(result.VoiceMessage),
		"result_label": result.Label(result.VoiceMessage),
		"called_at":    firstCallUTC,
		"saved":        false,
		"reason":       apiReason,
		"text":         apiText,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reply = %v, want %v", got, want)
	}
	if d := f.dialledNumbers(); !reflect.DeepEqual(d, []string{apiNumber}) {
		t.Fatalf("dialled = %v, want one call to %s", d, apiNumber)
	}
}

func TestCheckSavedFlag(t *testing.T) {
	f := newFakeChecker()
	h := NewAPI(f)

	first := decodeReply[checkReply](t, postCheck(t, h, checkBody(t, apiNumber)))
	// A call placed now would carry laterCallAt; the reused result must not.
	f.mu.Lock()
	f.next.CalledAt = laterCallAt
	f.mu.Unlock()
	rec := postCheck(t, h, checkBody(t, apiTypedInput))

	if rec.Code != http.StatusOK {
		t.Fatalf("second status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	second := decodeReply[checkReply](t, rec)
	if first.Saved || first.CalledAt != firstCallUTC {
		t.Fatalf("first reply = %+v, want saved false at %s", first, firstCallUTC)
	}
	if !second.Saved {
		t.Fatalf("second reply saved = false, want true: %+v", second)
	}
	if second.CalledAt != firstCallUTC {
		t.Fatalf("second called_at = %q, want the earlier call %q", second.CalledAt, firstCallUTC)
	}
	if d := f.dialledNumbers(); len(d) != 1 {
		t.Fatalf("dialled = %v, want exactly one call", d)
	}
}

func TestCheckInvalid(t *testing.T) {
	tests := []struct {
		name, input, message string
	}{
		{"too short", "123", "This is not a valid US or Canada number."},
		{"empty", "", "Enter a phone number."},
		{"other country", "+44 20 7946 0958", "Only US and Canada numbers (+1) can be checked."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeChecker()
			rec := postCheck(t, NewAPI(f), checkBody(t, tc.input))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			if got := decodeReply[map[string]any](t, rec); !reflect.DeepEqual(got, map[string]any{"error": tc.message}) {
				t.Fatalf("reply = %v, want error %q", got, tc.message)
			}
			if d := f.dialledNumbers(); len(d) != 0 {
				t.Fatalf("dialled = %v, want nothing dialled", d)
			}
		})
	}
}

func TestCheckErrors(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		err     error
		status  int
		message string
	}{
		{"bad JSON", `{"number":`, nil, http.StatusBadRequest, msgBadJSON},
		{"checker failure", checkBody(t, apiNumber), errStoreBroke, http.StatusInternalServerError, msgInternal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeChecker()
			f.err = tc.err
			rec := postCheck(t, NewAPI(f), tc.body)

			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body.String())
			}
			got := decodeReply[map[string]any](t, rec)
			if !reflect.DeepEqual(got, map[string]any{"error": tc.message}) {
				t.Fatalf("reply = %v, want error %q", got, tc.message)
			}
		})
	}
}

func TestHistory(t *testing.T) {
	f := newFakeChecker()
	f.history[apiNumber] = []check.Answer{
		{Number: apiNumber, Code: result.Fax, Label: result.Label(result.Fax), CalledAt: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)},
		{Number: apiNumber, Code: result.Unknown, Label: result.Label(result.Unknown), Reason: apiReason, CalledAt: time.Date(2026, 11, 2, 22, 30, 0, 0, callZone)},
		{Number: apiNumber, Code: result.VoiceMessage, Label: result.Label(result.VoiceMessage), Text: apiText, CalledAt: time.Date(2026, 10, 1, 8, 15, 0, 0, time.UTC)},
	}
	h := NewAPI(f)

	get := func(input string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history?number="+url.QueryEscape(input), nil))
		return rec
	}

	rec := get(apiTypedInput)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	want := historyReply{Checks: []checkReply{
		{Number: apiNumber, ResultCode: result.Fax, ResultLabel: result.Label(result.Fax), CalledAt: "2026-11-13T09:00:00Z"},
		{Number: apiNumber, ResultCode: result.Unknown, ResultLabel: result.Label(result.Unknown), Reason: apiReason, CalledAt: "2026-11-02T15:30:00Z"},
		{Number: apiNumber, ResultCode: result.VoiceMessage, ResultLabel: result.Label(result.VoiceMessage), Text: apiText, CalledAt: "2026-10-01T08:15:00Z"},
	}}
	if got := decodeReply[historyReply](t, rec); !reflect.DeepEqual(got, want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}

	none := get("555-223-0199")
	if none.Code != http.StatusOK || strings.TrimSpace(none.Body.String()) != `{"checks":[]}` {
		t.Fatalf("history of an unchecked number = %d %s, want 200 {\"checks\":[]}", none.Code, none.Body.String())
	}

	bad := get("123")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("history of an invalid number status = %d, want %d", bad.Code, http.StatusBadRequest)
	}
	if d := f.dialledNumbers(); len(d) != 0 {
		t.Fatalf("dialled = %v, want history to dial nothing", d)
	}
}

func TestCheckRejectsTrailingJSON(t *testing.T) {
	for _, suffix := range []string{`garbage`, `{"number":"5552230199"}`} {
		t.Run(suffix, func(t *testing.T) {
			f := newFakeChecker()
			rec := postCheck(t, NewAPI(f), checkBody(t, apiNumber)+suffix)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if got := decodeReply[errorReply](t, rec); got.Error != msgBadJSON {
				t.Fatalf("error = %q, want %q", got.Error, msgBadJSON)
			}
			if got := f.dialledNumbers(); len(got) != 0 {
				t.Fatalf("malformed request dialled %v", got)
			}
		})
	}
}

func TestCheckLargeJSON(t *testing.T) {
	f := newFakeChecker()
	rec := postCheck(t, NewAPI(f), strings.Repeat(" ", largeBodyPadding)+checkBody(t, apiNumber))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := decodeReply[checkReply](t, rec); got.Number != apiNumber || got.CalledAt != firstCallUTC {
		t.Fatalf("large request reply = %+v", got)
	}
	if got := f.dialledNumbers(); !reflect.DeepEqual(got, []string{apiNumber}) {
		t.Fatalf("dialled = %v, want one call", got)
	}
}

func TestZeroCallTime(t *testing.T) {
	f := newFakeChecker()
	f.next.CalledAt = time.Time{}
	h := NewAPI(f)
	rec := postCheck(t, h, checkBody(t, apiNumber))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertZero := func(s string) {
		t.Helper()
		at, err := time.Parse(time.RFC3339, s)
		if err != nil || !at.IsZero() || !strings.HasSuffix(s, "Z") {
			t.Fatalf("called_at = %q, want zero time in UTC RFC3339 (err %v)", s, err)
		}
	}
	assertZero(decodeReply[checkReply](t, rec).CalledAt)
	f.history[apiNumber] = []check.Answer{f.saved[apiNumber]}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history?number="+url.QueryEscape(apiNumber), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200", rec.Code)
	}
	got := decodeReply[historyReply](t, rec)
	if len(got.Checks) != 1 || got.Checks[0].Number != apiNumber {
		t.Fatalf("history = %+v, want the undialled entry", got)
	}
	assertZero(got.Checks[0].CalledAt)
}

func TestAPILogPrivacy(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	f := newFakeChecker()
	f.err = fmt.Errorf("check %s: %w", apiNumber, errStoreBroke)
	h := NewAPI(f)
	for _, op := range []string{"check", "history"} {
		logs.Reset()
		var rec *httptest.ResponseRecorder
		if op == "check" {
			rec = postCheck(t, h, checkBody(t, apiNumber))
		} else {
			rec = httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history?number="+url.QueryEscape(apiNumber), nil))
		}
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("%s status = %d, want 500", op, rec.Code)
		}
		if !strings.Contains(logs.String(), op) || strings.Contains(logs.String(), apiNumber) {
			t.Fatalf("%s log lacks operation context or discloses the phone number: %q", op, logs.String())
		}
		if got := decodeReply[errorReply](t, rec); got.Error != msgInternal {
			t.Fatalf("%s error = %q, want %q", op, got.Error, msgInternal)
		}
	}
}
