package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"checknumber/internal/check"
	"checknumber/internal/phone"
	"checknumber/internal/result"
)

// C-5: the page is tested against fakeChecker; nothing is dialled.
const (
	testNumber    = "+15552230101"
	testInput     = "(555) 223-0101"
	recordingName = "call-1.mp3"
	recordingBody = "ID3 fake mp3 bytes"
	secretBody    = "outside the recordings folder"
	voiceText     = `You have reached a number that has been disconnected. Please check the number <or> dial "again".`
	unknownReason = "Telnyx could not tell what answered"
	wordsReason   = "phone company message, see text"
	goneText      = "This number is not in use. Goodbye"
	companyText   = "Your call cannot be completed as dialed"
	savedDays     = 8
	fileMode      = 0o600
	dirMode       = 0o700
)

var (
	// callZone is not Eastern so a page that forgets to convert is caught.
	callZone = time.FixedZone("ICT", 7*60*60)
	// 2026-11-13 13:12 UTC is 08:12 EST; 2026-09-02 11:55 UTC is 07:55 EDT,
	// so a fixed -5 offset instead of the Eastern zone is caught too.
	novemberCall  = time.Date(2026, 11, 13, 20, 12, 0, 0, callZone)
	septemberCall = time.Date(2026, 9, 2, 18, 55, 0, 0, callZone)
)

// fakeChecker refuses an invalid number like check.Service does and otherwise
// returns the answer and history it holds, recording what it was asked.
type fakeChecker struct {
	mu           sync.Mutex
	answer       check.Answer
	history      []check.Answer
	err          error
	checked      []string
	historyAsked []string
}

func (f *fakeChecker) Check(_ context.Context, input string) (check.Answer, error) {
	if _, err := phone.Normalize(input); err != nil {
		return check.Answer{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checked = append(f.checked, input)
	if f.err != nil {
		return check.Answer{}, f.err
	}
	return f.answer, nil
}

func (f *fakeChecker) History(_ context.Context, input string) ([]check.Answer, error) {
	if _, err := phone.Normalize(input); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyAsked = append(f.historyAsked, input)
	return f.history, nil
}

func (f *fakeChecker) checks() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.checked...)
}

func answer(code result.Code, calledAt time.Time) check.Answer {
	return check.Answer{Number: testNumber, Code: code, Label: result.Label(code), CalledAt: calledAt}
}

func postCheck(t *testing.T, h http.Handler, input string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{numberField: {input}}
	req := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) string {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body:\n%s", rec.Code, status, rec.Body.String())
	}
	return rec.Body.String()
}

func wantContains(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(body, p) {
			t.Errorf("page lacks %q; body:\n%s", p, body)
		}
	}
}

func wantAbsent(t *testing.T, body string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(body, p) {
			t.Errorf("page has %q, want it absent; body:\n%s", p, body)
		}
	}
}

// wantOrder checks that parts appear in body in the given order.
func wantOrder(t *testing.T, body string, parts ...string) {
	t.Helper()
	at := -1
	for _, p := range parts {
		i := strings.Index(body, p)
		if i < 0 {
			t.Fatalf("page lacks %q; body:\n%s", p, body)
		}
		if i <= at {
			t.Fatalf("%q appears before the element preceding it in %q", p, parts)
		}
		at = i
	}
}

func TestCheckShowsResult(t *testing.T) {
	f := &fakeChecker{
		answer:  answer(result.Fax, novemberCall),
		history: []check.Answer{answer(result.Fax, novemberCall)},
	}
	body := wantStatus(t, postCheck(t, NewPage(f, t.TempDir()), testInput), http.StatusOK)

	if got := f.checks(); !reflect.DeepEqual(got, []string{testInput}) {
		t.Fatalf("checked = %v, want one check of %q", got, testInput)
	}
	// The approved mockup's layout: heading, form, result, history.
	wantOrder(t, body,
		"<h1>Check a number</h1>",
		`<p class="sub">Test page. Enter one fax number and press Check.</p>`,
		`<form class="panel" id="f" method="post" action="/check"`,
		`<label for="num">Phone number</label>`,
		`<input id="num" name="number" type="tel" value="(555) 223-0101">`,
		`<button class="primary" type="submit">Check</button>`,
		`<p class="hint">A number checked in the last 60 days is not dialled again; you get the last result. A new call takes up to about 90 seconds.</p>`,
		`<section class="panel" id="result" aria-live="polite">`,
		`<span class="chip c-ok">Fax</span><span class="hint">A fax machine answered</span>`,
		`<dt>Number</dt><dd class="mono">&#43;15552230101</dd>`,
		`<dt>Called</dt><dd>2026-11-13 08:12 (Eastern)</dd>`,
		`<h2>Earlier checks of this number</h2>`,
	)
	// "Calling" is ready for the script to show while the form waits (UX-1).
	wantContains(t, body, `<template id="calling">`, `<strong>Calling <span data-number></span></strong>`,
		"Ringing. This can take up to about 90 seconds.")
	wantAbsent(t, body, "Mockup for review", `id="error"`, `id="saved"`, "<audio", `id="copy"`, "<blockquote", "<dt>Reason</dt>")
}

func TestCheckShowsFormOnly(t *testing.T) {
	f := &fakeChecker{history: []check.Answer{answer(result.Fax, novemberCall)}}
	body := wantStatus(t, get(t, NewPage(f, t.TempDir()), "/"), http.StatusOK)

	wantContains(t, body, `<section class="panel" id="result" aria-live="polite" hidden>`, `value=""`)
	wantAbsent(t, body, `class="chip`, "Earlier checks of this number")
	if len(f.checks()) != 0 || len(f.historyAsked) != 0 {
		t.Fatalf("GET / asked the checker: checks %v, history %v", f.checks(), f.historyAsked)
	}
}

func TestCrossSiteCheckRefused(t *testing.T) {
	f := &fakeChecker{answer: answer(result.Fax, novemberCall)}
	form := url.Values{numberField: {testInput}}
	req := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	NewPage(f, t.TempDir()).ServeHTTP(rec, req)

	wantStatus(t, rec, http.StatusForbidden)
	if got := f.checks(); len(got) != 0 {
		t.Fatalf("checked = %v, want no check from another site's page", got)
	}
}

func TestVoiceMessageBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recordingName), []byte(recordingBody), fileMode); err != nil {
		t.Fatalf("write recording: %v", err)
	}
	a := answer(result.VoiceMessage, novemberCall)
	a.RecordingPath = recordingName
	a.Text = voiceText
	h := NewPage(&fakeChecker{answer: a}, dir)
	body := wantStatus(t, postCheck(t, h, testInput), http.StatusOK)

	wantOrder(t, body,
		`<span class="chip c-info">Voice message</span><span class="hint">New call just made</span>`,
		`<dt>Called</dt><dd>2026-11-13 08:12 (Eastern)</dd>`,
		`<div class="player"><audio controls preload="metadata" src="/recordings/call-1.mp3"></audio></div>`,
		`<strong>What the message said</strong><button class="copy" type="button" id="copy">Copy text</button>`,
		// The full text, escaped, so it is shown and copied exactly.
		`<blockquote id="txt">You have reached a number that has been disconnected. Please check the number &lt;or&gt; dial &#34;again&#34;.</blockquote>`,
	)
	wantContains(t, body, "navigator.clipboard.writeText(txt.textContent)")

	// The player's source is served from the recordings folder.
	rec := get(t, h, "/recordings/"+recordingName)
	if got := wantStatus(t, rec, http.StatusOK); got != recordingBody {
		t.Fatalf("recording body = %q, want %q", got, recordingBody)
	}
}

func TestSavedNote(t *testing.T) {
	a := answer(result.VoiceMessage, time.Now().Add(-(savedDays*day + time.Hour)))
	a.Saved = true
	body := wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: a}, t.TempDir()), testInput), http.StatusOK)

	wantOrder(t, body,
		`<span class="chip c-info">Voice message</span></div>`,
		`<div class="saved" id="saved">Saved result from a call 8 days ago. Not dialled again. A new call happens after 60 days.</div>`,
		`<dt>Number</dt>`,
	)
	wantAbsent(t, body, hintNewCall)

	// A new call has no saved note.
	a.Saved = false
	a.CalledAt = novemberCall
	body = wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: a}, t.TempDir()), testInput), http.StatusOK)
	wantAbsent(t, body, "Saved result from a call", `id="saved"`)
}

func TestSavedNoteBoundaries(t *testing.T) {
	// Fixed UTC instants force both sides of the whole-24-hour boundary.
	now := time.Date(2026, time.November, 13, 16, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		age  time.Duration
		note string
	}{
		{"same instant", 0, "0 days ago"},
		{"before one day", 24*time.Hour - time.Nanosecond, "0 days ago"},
		{"exactly one day", 24 * time.Hour, "1 days ago"},
		{"before eight days", 8*24*time.Hour - time.Nanosecond, "7 days ago"},
		{"exactly eight days", 8 * 24 * time.Hour, "8 days ago"},
		{"reuse limit", 60 * 24 * time.Hour, "60 days ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := answer(result.VoiceMessage, now.Add(-tc.age))
			a.Saved = true
			rec := httptest.NewRecorder()
			render(rec, http.StatusOK, pageData{Result: toResultView(a, now)})
			body := wantStatus(t, rec, http.StatusOK)
			wantContains(t, body, `<div class="saved" id="saved">Saved result from a call `+tc.note+`. Not dialled again. A new call happens after 60 days.</div>`)
			wantAbsent(t, body, hintNewCall)
		})
	}
}

func TestUnknownAndPerson(t *testing.T) {
	unknown := answer(result.Unknown, novemberCall)
	unknown.Reason = unknownReason
	body := wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: unknown}, t.TempDir()), testInput), http.StatusOK)
	wantOrder(t, body,
		`<span class="chip c-bad">Unknown</span><span class="hint">Follow up by hand</span>`,
		`<dt>Called</dt><dd>2026-11-13 08:12 (Eastern)</dd><dt>Reason</dt><dd>Telnyx could not tell what answered</dd>`,
		`<p class="hint">The next check of this number will dial again.</p>`,
	)

	// C-1: even if a recording path and text reached the page, a person
	// answering shows neither.
	person := answer(result.PersonAnswered, novemberCall)
	person.RecordingPath = recordingName
	person.Text = voiceText
	body = wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: person}, t.TempDir()), testInput), http.StatusOK)
	wantContains(t, body, `<span class="chip c-warn">Person answered</span><span class="hint">The app hung up. Nothing was kept.</span>`)
	wantAbsent(t, body, "<audio", recordingName, `id="copy"`, `<blockquote`, "disconnected", "<dt>Reason</dt>",
		"The next check of this number will dial again.", "What the message said")
}

func TestDisconnectedWordsBlock(t *testing.T) {
	for _, code := range []result.Code{result.Disconnected, result.DisconnectedUnallocated} {
		t.Run(string(code), func(t *testing.T) {
			a := answer(code, novemberCall)
			a.RecordingPath = recordingName
			a.Text = goneText
			body := wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: a}, t.TempDir()), testInput), http.StatusOK)

			wantOrder(t, body,
				`<span class="chip c-info">`+result.Label(code)+`</span><span class="hint">New call just made</span>`,
				`<dt>Called</dt><dd>2026-11-13 08:12 (Eastern)</dd></dl>`,
				`<div class="player"><audio controls preload="metadata" src="/recordings/call-1.mp3"></audio></div>`,
				`<strong>What the message said</strong><button class="copy" type="button" id="copy">Copy text</button>`,
				`<blockquote id="txt">This number is not in use. Goodbye</blockquote>`,
			)
			wantAbsent(t, body, "<dt>Reason</dt>")
		})
	}
}

func TestUnknownWordsBlock(t *testing.T) {
	a := answer(result.Unknown, novemberCall)
	a.Reason = wordsReason
	a.RecordingPath = recordingName
	a.Text = companyText
	body := wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: a}, t.TempDir()), testInput), http.StatusOK)
	wantOrder(t, body,
		`<span class="chip c-bad">Unknown</span><span class="hint">Follow up by hand</span>`,
		`<dt>Reason</dt><dd>phone company message, see text</dd></dl>`,
		`<div class="player"><audio controls preload="metadata" src="/recordings/call-1.mp3"></audio></div>`,
		`<strong>What the message said</strong><button class="copy" type="button" id="copy">Copy text</button>`,
		`<blockquote id="txt">Your call cannot be completed as dialed</blockquote>`,
		`<p class="hint">The next check of this number will dial again.</p>`,
	)

	// An Unknown without words shows as today: its reason, no player and no text.
	plain := answer(result.Unknown, novemberCall)
	plain.Reason = unknownReason
	body = wantStatus(t, postCheck(t, NewPage(&fakeChecker{answer: plain}, t.TempDir()), testInput), http.StatusOK)
	wantContains(t, body, `<dt>Reason</dt><dd>Telnyx could not tell what answered</dd></dl>`,
		`<p class="hint">The next check of this number will dial again.</p>`)
	wantAbsent(t, body, "<audio", `id="copy"`, "<blockquote", "What the message said")
}

func TestInvalidMessage(t *testing.T) {
	tests := []struct {
		name, input, message string
	}{
		{"too short", "123", "This is not a valid US or Canada number."},
		{"empty", "", "Enter a phone number."},
		// html/template writes "+" as "&#43;"; the browser shows "+".
		{"other country", "+44 20 7946 0958", "Only US and Canada numbers (&#43;1) can be checked."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeChecker{answer: answer(result.Fax, novemberCall), history: []check.Answer{answer(result.Fax, novemberCall)}}
			body := wantStatus(t, postCheck(t, NewPage(f, t.TempDir()), tc.input), http.StatusBadRequest)

			wantContains(t, body,
				`<p class="error" role="alert" id="error">`+tc.message+`</p>`,
				`<section class="panel" id="result" aria-live="polite" hidden>`,
			)
			wantAbsent(t, body, `class="chip`, "<dt>Number</dt>", "Earlier checks of this number")
			if got := f.checks(); len(got) != 0 {
				t.Fatalf("checked = %v, want nothing dialled", got)
			}
		})
	}
}

func TestCheckFailureMessage(t *testing.T) {
	f := &fakeChecker{err: errors.New("store unavailable for +15552230101")}
	body := wantStatus(t, postCheck(t, NewPage(f, t.TempDir()), testInput), http.StatusInternalServerError)

	wantContains(t, body, `<p class="error" role="alert" id="error">`+msgCheckFailed+`</p>`)
	wantAbsent(t, body, `class="chip`, "store unavailable")
}

func TestHistoryAndRecordingRoute(t *testing.T) {
	newest := answer(result.VoiceMessage, novemberCall)
	older := answer(result.NoAnswer, septemberCall)
	// A check closed before it was dialled (TC-20) has no call time.
	neverCalled := answer(result.Unknown, time.Time{})
	f := &fakeChecker{history: []check.Answer{newest, older, neverCalled}}
	h := NewPage(f, t.TempDir())

	body := wantStatus(t, get(t, h, "/?number="+url.QueryEscape(testInput)), http.StatusOK)
	if !reflect.DeepEqual(f.historyAsked, []string{testInput}) {
		t.Fatalf("history asked for %v, want %q", f.historyAsked, testInput)
	}
	wantOrder(t, body,
		`<input id="num" name="number" type="tel" value="(555) 223-0101">`,
		`<h2>Earlier checks of this number</h2>`,
		`<th>Date and time of call (Eastern)</th><th>Result</th>`,
		`<tr><td>2026-11-13 08:12</td><td>Voice message</td></tr>`,
		`<tr><td>2026-09-02 07:55</td><td>No answer</td></tr>`,
		`<tr><td>Not called</td><td>Unknown</td></tr>`,
	)
	wantAbsent(t, body, `class="chip`)

	// After a check the history of the dialled number follows the result.
	f.answer = newest
	body = wantStatus(t, postCheck(t, h, testInput), http.StatusOK)
	if got := f.historyAsked[len(f.historyAsked)-1]; got != testNumber {
		t.Fatalf("history after check asked for %q, want %q", got, testNumber)
	}
	wantOrder(t, body, `id="result"`, `<span class="chip c-info">Voice message</span>`,
		`<h2>Earlier checks of this number</h2>`,
		`<tr><td>2026-11-13 08:12</td><td>Voice message</td></tr>`,
		`<tr><td>2026-09-02 07:55</td><td>No answer</td></tr>`)
}

func TestHistoryUnfinishedChecks(t *testing.T) {
	// Service.History includes checks before they have a final result, including
	// a queued check without a call time. Preserve its supplied newest-first order.
	pending := check.Answer{Number: testNumber, CalledAt: novemberCall.Add(time.Hour)}
	queued := check.Answer{Number: testNumber}
	completed := answer(result.Fax, novemberCall)
	f := &fakeChecker{history: []check.Answer{queued, pending, completed}}
	body := wantStatus(t, get(t, NewPage(f, t.TempDir()), "/?number="+url.QueryEscape(testInput)), http.StatusOK)
	wantOrder(t, body,
		`<tr><td>Not called</td><td>Calling</td></tr>`,
		`<tr><td>2026-11-13 09:12</td><td>Calling</td></tr>`,
		`<tr><td>2026-11-13 08:12</td><td>Fax</td></tr>`,
	)
	wantAbsent(t, body, `<td></td>`, `<td>Unknown</td>`)
	if len(f.checks()) != 0 {
		t.Fatalf("history GET dialled a number: %v", f.checks())
	}
}

func TestRecordingRoute(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "recordings")
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, dirMode); err != nil {
		t.Fatalf("make recordings folder: %v", err)
	}
	files := map[string]string{
		filepath.Join(dir, recordingName): recordingBody,
		filepath.Join(sub, recordingName): recordingBody,
		filepath.Join(base, "x"):          secretBody,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "x"), filepath.Join(dir, "link.mp3")); err != nil {
		t.Fatalf("make symlink: %v", err)
	}
	h := NewPage(&fakeChecker{}, dir)

	// The plain name is served, so the 404s below are the name rule, not a broken route.
	if got := wantStatus(t, get(t, h, "/recordings/"+recordingName), http.StatusOK); got != recordingBody {
		t.Fatalf("recording body = %q, want %q", got, recordingBody)
	}
	for _, target := range []string{
		"/recordings/../x",
		"/recordings/..",
		"/recordings/sub/" + recordingName,
		"/recordings/sub%2F" + recordingName,
		"/recordings/sub",
		"/recordings/",
		"/recordings/missing.mp3",
		"/recordings/link.mp3",
		"/recordings/..%5Cx",
	} {
		t.Run(target, func(t *testing.T) {
			body := wantStatus(t, get(t, h, target), http.StatusNotFound)
			wantAbsent(t, body, secretBody, recordingBody)
		})
	}
}
