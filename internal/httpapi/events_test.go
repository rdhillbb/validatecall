package httpapi

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"checknumber/internal/telnyx"
)

// C-5: events are signed with a key generated here; no real Telnyx is used.
const (
	eventCallID    = "v3:call-one"
	eventLegID     = "leg-one"
	eventSessionID = "session-one"
	eventRecording = "https://files.example.test/rec.mp3"
	// staleBy is beyond the 5-minute window telnyx.Verify allows.
	staleBy = 10 * time.Minute

	hangupBody = `{"data":{"event_type":"call.hangup","occurred_at":"2026-11-13T15:59:58Z","payload":{` +
		`"call_control_id":"v3:call-one","call_leg_id":"leg-one","call_session_id":"session-one",` +
		`"hangup_cause":"user_busy","sip_hangup_cause":"486"}}}`
	// call.recording.saved has no call_control_id in the Telnyx spec.
	recordingBody = `{"data":{"event_type":"call.recording.saved","occurred_at":"2026-11-13T15:59:59Z","payload":{` +
		`"call_leg_id":"leg-one","call_session_id":"session-one",` +
		`"recording_urls":{"mp3":"https://files.example.test/rec.mp3"}}}}`
	detectionBody = `{"data":{"event_type":"call.machine.premium.detection.ended","occurred_at":"2026-11-13T15:59:57Z","payload":{` +
		`"call_control_id":"v3:call-one","call_leg_id":"leg-one","call_session_id":"session-one","result":"machine"}}}`

	// eventBodyLimit is TC-38's 4 MiB, written out here so a changed limit in
	// events.go fails TestEventsBodyLimit.
	eventBodyLimit = 4 << 20
	// eventCallIDTail is the last 6 characters of eventCallID (TC-39).
	eventCallIDTail = "ll-one"
	shortCallID     = "abc"
)

var (
	eventsNow = time.Date(2026, 11, 13, 16, 0, 0, 0, time.UTC)
	// noTypeBody is a Telnyx body the parser cannot read: it has no event type.
	noTypeBody   = strings.Replace(hangupBody, `"event_type":"call.hangup",`, "", 1)
	errEventRead = errors.New("connection lost")
)

type fakeSink struct {
	mu     sync.Mutex
	events []telnyx.Event
}

func (s *fakeSink) HandleEvent(ev telnyx.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *fakeSink) received() []telnyx.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]telnyx.Event(nil), s.events...)
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func unixTS(at time.Time) string {
	return strconv.FormatInt(at.Unix(), 10)
}

// sign signs "<timestamp>|<body>" the way Telnyx does.
func sign(priv ed25519.PrivateKey, ts, body string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte(ts+"|"+body)))
}

func sendEvent(h http.Handler, method, path, body, ts, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if ts != "" {
		req.Header.Set(headerTimestamp, ts)
	}
	if sig != "" {
		req.Header.Set(headerSignature, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func fixedNow() time.Time { return eventsNow }

// captureLog returns what the standard logger writes during the test, without
// the date prefix, so a test can compare whole lines.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	output, flags := log.Writer(), log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(output)
		log.SetFlags(flags)
	})
	return &logs
}

// paddedHangup is hangupBody with leading spaces up to size bytes; the
// padding leaves it a valid event.
func paddedHangup(size int) string {
	return strings.Repeat(" ", size-len(hangupBody)) + hangupBody
}

type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

func TestEventsSigned(t *testing.T) {
	pub, priv := newKey(t)
	tests := []struct {
		name string
		body string
		want telnyx.Event
	}{
		{"hangup", hangupBody, telnyx.Event{
			Type: telnyx.EventCallHangup, CallControlID: eventCallID, CallLegID: eventLegID, CallSessionID: eventSessionID,
			HangupCause: "user_busy", SIPHangupCause: "486", OccurredAt: time.Date(2026, 11, 13, 15, 59, 58, 0, time.UTC),
		}},
		{"recording saved without call control id", recordingBody, telnyx.Event{
			Type: telnyx.EventRecordingSaved, CallLegID: eventLegID, CallSessionID: eventSessionID,
			RecordingURL: eventRecording, OccurredAt: time.Date(2026, 11, 13, 15, 59, 59, 0, time.UTC),
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			ts := unixTS(eventsNow)
			rec := sendEvent(NewEvents(pub, sink, fixedNow), http.MethodPost, eventsPath, tc.body, ts, sign(priv, ts, tc.body))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			got := sink.received()
			if len(got) != 1 {
				t.Fatalf("sink got %d events, want exactly 1: %+v", len(got), got)
			}
			ev := got[0]
			if !ev.OccurredAt.Equal(tc.want.OccurredAt) {
				t.Fatalf("OccurredAt = %s, want %s", ev.OccurredAt, tc.want.OccurredAt)
			}
			ev.OccurredAt, tc.want.OccurredAt = time.Time{}, time.Time{}
			if ev != tc.want {
				t.Fatalf("event = %+v, want %+v", ev, tc.want)
			}
		})
	}
}

func TestEventsForged(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	ts := unixTS(eventsNow)
	staleTS := unixTS(eventsNow.Add(-staleBy))
	good := sign(priv, ts, hangupBody)

	// Control: the untampered request is accepted, so each 401 below comes
	// from the one thing its case changes.
	control := &fakeSink{}
	if rec := sendEvent(NewEvents(pub, control, fixedNow), http.MethodPost, eventsPath, hangupBody, ts, good); rec.Code != http.StatusOK || len(control.received()) != 1 {
		t.Fatalf("control: status %d, %d events; want 200 and 1 event", rec.Code, len(control.received()))
	}

	tests := []struct {
		name, body, ts, sig string
	}{
		{"signed by another key", hangupBody, ts, sign(otherPriv, ts, hangupBody)},
		{"body changed after signing", strings.Replace(hangupBody, "user_busy", "normal_clearing", 1), ts, good},
		{"no signature", hangupBody, ts, ""},
		{"no timestamp", hangupBody, "", good},
		{"stale timestamp", hangupBody, staleTS, sign(priv, staleTS, hangupBody)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sink := &fakeSink{}
			rec := sendEvent(NewEvents(pub, sink, fixedNow), http.MethodPost, eventsPath, tc.body, tc.ts, tc.sig)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if got := sink.received(); len(got) != 0 {
				t.Fatalf("forged event reached the sink: %+v", got)
			}
		})
	}
}

func TestEventsOnlyPath(t *testing.T) {
	pub, priv := newKey(t)
	ts := unixTS(eventsNow)
	sig := sign(priv, ts, hangupBody)

	// Control: the same signed body on POST /telnyx/events is accepted, so
	// each 404 below is decided by the route alone.
	control := &fakeSink{}
	if rec := sendEvent(NewEvents(pub, control, fixedNow), http.MethodPost, eventsPath, hangupBody, ts, sig); rec.Code != http.StatusOK {
		t.Fatalf("control: status %d, want 200", rec.Code)
	}

	tests := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/check"},
		{http.MethodGet, "/api/check"},
		{http.MethodGet, "/api/history"},
		{http.MethodGet, "/"},
		{http.MethodPost, "/"},
		{http.MethodGet, eventsPath},
		{http.MethodPut, eventsPath},
		{http.MethodPost, eventsPath + "/extra"},
		{http.MethodPost, "/telnyx"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			sink := &fakeSink{}
			rec := sendEvent(NewEvents(pub, sink, fixedNow), tc.method, tc.path, hangupBody, ts, sig)

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}
			if got := sink.received(); len(got) != 0 {
				t.Fatalf("sink got events on %s %s: %+v", tc.method, tc.path, got)
			}
		})
	}
}

func TestEventsLargeSignedBody(t *testing.T) {
	pub, priv := newKey(t)
	body := strings.Repeat(" ", largeBodyPadding) + hangupBody
	ts := unixTS(eventsNow)
	sink := &fakeSink{}
	rec := sendEvent(NewEvents(pub, sink, fixedNow), http.MethodPost, eventsPath, body, ts, sign(priv, ts, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := sink.received()
	want := telnyx.Event{
		Type: telnyx.EventCallHangup, CallControlID: eventCallID, CallLegID: eventLegID, CallSessionID: eventSessionID,
		HangupCause: "user_busy", SIPHangupCause: "486", OccurredAt: time.Date(2026, 11, 13, 15, 59, 58, 0, time.UTC),
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("sink = %+v, want exactly %+v", got, want)
	}
}

func TestEventsLogPrivacy(t *testing.T) {
	pub, priv := newKey(t)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	// Without data.event_type the signed body cannot be read; it still holds
	// the private number the log must not show.
	body := strings.Replace(hangupBody, `"event_type":"call.hangup"`, `"to":"`+apiNumber+`"`, 1)
	ts := unixTS(eventsNow)
	for _, signed := range []bool{true, false} {
		logs.Reset()
		timestamp := ts
		// A signed event that cannot be read is acknowledged (D-5).
		status := http.StatusOK
		context := "telnyx event refused: event has no data.event_type"
		if !signed {
			timestamp = apiNumber
			status = http.StatusUnauthorized
			// apiNumber reads as unix seconds far from eventsNow.
			context = "telnyx event refused: timestamp outside the allowed window"
		}
		sink := &fakeSink{}
		rec := sendEvent(NewEvents(pub, sink, fixedNow), http.MethodPost, eventsPath, body, timestamp, sign(priv, timestamp, body))
		if rec.Code != status || len(sink.received()) != 0 {
			t.Fatalf("status %d, sink %+v; want %d, empty sink", rec.Code, sink.received(), status)
		}
		if !strings.Contains(logs.String(), context) || strings.Contains(logs.String(), apiNumber) {
			t.Fatalf("log lacks operation context or discloses private input: %q", logs.String())
		}
	}
}

func TestEventsBodyLimit(t *testing.T) {
	pub, priv := newKey(t)
	ts := unixTS(eventsNow)
	tests := []struct {
		name       string
		size       int
		wantStatus int
		wantEvents int
	}{
		// Control: the same signed event at the limit is accepted, so the 413
		// is decided by the one extra byte.
		{"at the limit", eventBodyLimit, http.StatusOK, 1},
		{"one byte over", eventBodyLimit + 1, http.StatusRequestEntityTooLarge, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := paddedHangup(tc.size)
			sink := &fakeSink{}
			rec := sendEvent(NewEvents(pub, sink, fixedNow), http.MethodPost, eventsPath, body, ts, sign(priv, ts, body))

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := sink.received(); len(got) != tc.wantEvents {
				t.Fatalf("sink got %d events, want %d", len(got), tc.wantEvents)
			}
		})
	}

	t.Run("not read in full", func(t *testing.T) {
		body := paddedHangup(2 * eventBodyLimit)
		reader := &countingReader{r: strings.NewReader(body)}
		req := httptest.NewRequest(http.MethodPost, eventsPath, reader)
		req.Header.Set(headerTimestamp, ts)
		req.Header.Set(headerSignature, sign(priv, ts, body))
		sink := &fakeSink{}
		rec := httptest.NewRecorder()
		NewEvents(pub, sink, fixedNow).ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
		}
		if got := sink.received(); len(got) != 0 {
			t.Fatalf("oversized event reached the sink: %d events", len(got))
		}
		if reader.read > eventBodyLimit+1 {
			t.Fatalf("handler read %d of %d bytes, want at most %d", reader.read, len(body), eventBodyLimit+1)
		}
	})
}

func TestEventsUnreadableSignedAcknowledged(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	logs := captureLog(t)
	ts := unixTS(eventsNow)
	tests := []struct {
		name, body, wantLog string
	}{
		{"no event type", noTypeBody, "telnyx event refused: event has no data.event_type\n"},
		{"not JSON", "<html>bad gateway</html>", "telnyx event refused: body is not JSON\n"},
		{"empty body", "", "telnyx event refused: body is not JSON\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			sink := &fakeSink{}
			h := NewEvents(pub, sink, fixedNow)
			rec := sendEvent(h, http.MethodPost, eventsPath, tc.body, ts, sign(priv, ts, tc.body))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := logs.String(); got != tc.wantLog {
				t.Fatalf("log = %q, want %q", got, tc.wantLog)
			}
			// Only the signature earns the 200: the same body signed by
			// another key is still refused.
			if forged := sendEvent(h, http.MethodPost, eventsPath, tc.body, ts, sign(otherPriv, ts, tc.body)); forged.Code != http.StatusUnauthorized {
				t.Fatalf("forged status = %d, want %d", forged.Code, http.StatusUnauthorized)
			}
			if got := sink.received(); len(got) != 0 {
				t.Fatalf("unreadable event reached the sink: %+v", got)
			}
		})
	}
}

func TestEventsLogLines(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	logs := captureLog(t)
	ts := unixTS(eventsNow)
	shortIDBody := strings.Replace(hangupBody, eventCallID, shortCallID, 1)
	tests := []struct {
		name       string
		body       string
		signer     ed25519.PrivateKey
		wantStatus int
		wantLog    string
	}{
		{"hangup", hangupBody, priv, http.StatusOK,
			"telnyx event call.hangup call=" + eventCallIDTail + " cause=user_busy sip=486"},
		{"hangup with a short call id", shortIDBody, priv, http.StatusOK,
			"telnyx event call.hangup call=" + shortCallID + " cause=user_busy sip=486"},
		{"detection ended", detectionBody, priv, http.StatusOK,
			"telnyx event call.machine.premium.detection.ended call=" + eventCallIDTail + " result=machine"},
		{"recording saved has no call id", recordingBody, priv, http.StatusOK,
			"telnyx event call.recording.saved"},
		{"signed by another key", hangupBody, otherPriv, http.StatusUnauthorized,
			"telnyx event refused: signature does not match"},
		{"no signature", hangupBody, nil, http.StatusUnauthorized,
			"telnyx event refused: missing signature or timestamp header"},
		{"signed but unreadable", noTypeBody, priv, http.StatusOK,
			"telnyx event refused: event has no data.event_type"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			sig := ""
			if tc.signer != nil {
				sig = sign(tc.signer, ts, tc.body)
			}
			rec := sendEvent(NewEvents(pub, &fakeSink{}, fixedNow), http.MethodPost, eventsPath, tc.body, ts, sig)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			got := logs.String()
			private := map[string]string{
				"key":       base64.StdEncoding.EncodeToString(pub),
				"signature": sig,
				"body":      tc.body,
			}
			for what, value := range private {
				if value != "" && strings.Contains(got, value) {
					t.Errorf("log contains the %s: %q", what, got)
				}
			}
			if got != tc.wantLog+"\n" {
				t.Fatalf("log = %q, want the one line %q", got, tc.wantLog)
			}
		})
	}

	t.Run("body cannot be read", func(t *testing.T) {
		logs.Reset()
		req := httptest.NewRequest(http.MethodPost, eventsPath, iotest.ErrReader(errEventRead))
		sink := &fakeSink{}
		rec := httptest.NewRecorder()
		NewEvents(pub, sink, fixedNow).ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest || len(sink.received()) != 0 {
			t.Fatalf("status %d, %d events; want %d and none", rec.Code, len(sink.received()), http.StatusBadRequest)
		}
		if got, want := logs.String(), "telnyx event refused: read body: "+errEventRead.Error()+"\n"; got != want {
			t.Fatalf("log = %q, want %q", got, want)
		}
	})
}
