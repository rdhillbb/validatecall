package telnyx

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"
)

// Sample values shaped like the Telnyx Call Control spec examples (C-5: no real key or call).
const (
	sampleCallID     = "v3:MdI91X4lWFEs7IgbBEOT9M4AigoY08M0WWZFISt1Yw2axZ_IiE4pqg"
	sampleOccurredAt = "2026-11-13T12:00:01.521992Z"
	sampleMP3URL     = "https://example.com/recording.mp3"
	sampleLegID      = "428c31b6-7af4-4bcb-b7f5-5013ef9657c1"
	sampleSessionID  = "428c31b6-abf3-3bc1-b7f4-5013ef9657c1"
	// Independent of maxEventAge so changing implementation policy breaks these tests.
	contractEventWindow = 5 * time.Minute
)

// testNow is the fixed clock every Verify test uses.
var testNow = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

func newKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	return pub, priv
}

func unixStamp(at time.Time) string { return strconv.FormatInt(at.Unix(), 10) }

// signEvent signs "<timestamp>|<body>" the way Telnyx does and returns base64.
func signEvent(priv ed25519.PrivateKey, timestamp string, body []byte) string {
	msg := append([]byte(timestamp+"|"), body...)
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
}

// eventBody wraps a payload object in the Telnyx webhook envelope.
func eventBody(eventType, payload string) []byte {
	return []byte(fmt.Sprintf(`{"data":{"record_type":"event","event_type":%q,`+
		`"id":"0ccc7b54-4df3-4bca-a65a-3da1ecc777f0","occurred_at":%q,"payload":%s},"meta":{"attempt":1}}`,
		eventType, sampleOccurredAt, payload))
}

func TestVerifyGood(t *testing.T) {
	pub, priv := newKeyPair(t)
	body := eventBody(EventCallAnswered, fmt.Sprintf(`{"call_control_id":%q,"state":"answered"}`, sampleCallID))
	cases := map[string]time.Time{
		"signed now":                   testNow,
		"signed exactly 5 minutes ago": testNow.Add(-contractEventWindow),
		"clock 5 minutes ahead":        testNow.Add(contractEventWindow),
	}
	for name, signedAt := range cases {
		t.Run(name, func(t *testing.T) {
			ts := unixStamp(signedAt)
			if err := Verify(pub, ts, signEvent(priv, ts, body), body, testNow); err != nil {
				t.Fatalf("Verify of a correctly signed event: %v", err)
			}
		})
	}
}

func TestVerifyRejectsForged(t *testing.T) {
	pub, priv := newKeyPair(t)
	_, otherPriv := newKeyPair(t)
	body := eventBody(EventCallHangup, fmt.Sprintf(`{"call_control_id":%q,"hangup_cause":"normal_clearing"}`, sampleCallID))
	ts := unixStamp(testNow)
	good := signEvent(priv, ts, body)
	changed := eventBody(EventCallHangup, fmt.Sprintf(`{"call_control_id":%q,"hangup_cause":"user_busy"}`, sampleCallID))
	earlierTS := unixStamp(testNow.Add(-time.Minute))

	// The unchanged event must pass, so each refusal below comes from its one change.
	if err := Verify(pub, ts, good, body, testNow); err != nil {
		t.Fatalf("baseline event refused: %v", err)
	}

	cases := []struct {
		name      string
		pub       ed25519.PublicKey
		timestamp string
		signature string
		body      []byte
		want      error
	}{
		{"changed body", pub, ts, good, changed, errBadSignature},
		{"another key's signature", pub, ts, signEvent(otherPriv, ts, body), body, errBadSignature},
		{"signature for another timestamp", pub, earlierTS, good, body, errBadSignature},
		{"missing signature header", pub, ts, "", body, errMissingHeader},
		{"missing timestamp header", pub, "", good, body, errMissingHeader},
		{"signature not base64", pub, ts, "not*base64!", body, errBadEncoding},
		{"signature cut to half length", pub, ts, good[:len(good)/2], body, errBadSignature},
		{"timestamp not a number", pub, "12:00", good, body, errBadTimestamp},
		{"no public key", nil, ts, good, body, errBadPublicKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.pub, tc.timestamp, tc.signature, tc.body, testNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsStale(t *testing.T) {
	pub, priv := newKeyPair(t)
	body := eventBody(EventCallAnswered, fmt.Sprintf(`{"call_control_id":%q}`, sampleCallID))
	cases := map[string]time.Time{
		"one second past 5 minutes old":   testNow.Add(-contractEventWindow - time.Second),
		"one hour old":                    testNow.Add(-time.Hour),
		"one second past 5 minutes ahead": testNow.Add(contractEventWindow + time.Second),
	}
	for name, signedAt := range cases {
		t.Run(name, func(t *testing.T) {
			ts := unixStamp(signedAt)
			err := Verify(pub, ts, signEvent(priv, ts, body), body, testNow)
			if !errors.Is(err, errStaleTimestamp) {
				t.Fatalf("Verify error = %v, want %v", err, errStaleTimestamp)
			}
		})
	}
}

func TestParseEventTypes(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, sampleOccurredAt)
	if err != nil {
		t.Fatalf("parse sample time: %v", err)
	}
	cases := []struct {
		eventType string
		payload   string
		want      Event
	}{
		{
			EventCallAnswered,
			fmt.Sprintf(`{"call_control_id":%q,"connection_id":"7267xxxxxxxxxxxxxx","state":"answered"}`, sampleCallID),
			Event{Type: EventCallAnswered, CallControlID: sampleCallID, OccurredAt: at},
		},
		{
			EventCallHangup,
			fmt.Sprintf(`{"call_control_id":%q,"state":"hangup","hangup_cause":"user_busy","hangup_source":"callee","sip_hangup_cause":"486"}`, sampleCallID),
			Event{Type: EventCallHangup, CallControlID: sampleCallID, HangupCause: "user_busy", SIPHangupCause: "486", OccurredAt: at},
		},
		{
			EventMachinePremiumDetectionEnded,
			fmt.Sprintf(`{"call_control_id":%q,"result":"fax_detected"}`, sampleCallID),
			Event{Type: EventMachinePremiumDetectionEnded, CallControlID: sampleCallID, DetectionResult: "fax_detected", OccurredAt: at},
		},
		{
			EventRecordingSaved,
			fmt.Sprintf(`{"call_control_id":%q,"channels":"single","recording_urls":{"mp3":%q,"wav":null},"public_recording_urls":{"mp3":null,"wav":null}}`, sampleCallID, sampleMP3URL),
			Event{Type: EventRecordingSaved, CallControlID: sampleCallID, RecordingURL: sampleMP3URL, OccurredAt: at},
		},
		{
			EventRecordingError,
			fmt.Sprintf(`{"call_control_id":%q,"reason":"Internal server error"}`, sampleCallID),
			Event{Type: EventRecordingError, CallControlID: sampleCallID, OccurredAt: at},
		},
		{
			EventTranscriptionSaved,
			fmt.Sprintf(`{"call_control_id":%q,"status":"completed","transcription_text":"Hi, leave a message."}`, sampleCallID),
			Event{Type: EventTranscriptionSaved, CallControlID: sampleCallID, TranscriptionText: "Hi, leave a message.", OccurredAt: at},
		},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			got, err := ParseEvent(eventBody(tc.eventType, tc.payload))
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseEvent =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

// Every known type keeps the call's leg and session ids. call.recording.saved
// has no call_control_id in the Telnyx spec, so they are its only identity.
func TestParseEventKeepsCallIdentity(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, sampleOccurredAt)
	if err != nil {
		t.Fatalf("parse sample time: %v", err)
	}
	withID := fmt.Sprintf(`"call_control_id":%q,"call_leg_id":%q,"call_session_id":%q`, sampleCallID, sampleLegID, sampleSessionID)
	noControlID := fmt.Sprintf(`"call_leg_id":%q,"call_session_id":%q`, sampleLegID, sampleSessionID)
	cases := []struct {
		eventType string
		payload   string
		want      Event
	}{
		{EventCallAnswered, `{` + withID + `,"state":"answered"}`,
			Event{Type: EventCallAnswered, CallControlID: sampleCallID, CallLegID: sampleLegID, CallSessionID: sampleSessionID, OccurredAt: at}},
		{EventCallHangup, `{` + withID + `,"hangup_cause":"user_busy"}`,
			Event{Type: EventCallHangup, CallControlID: sampleCallID, CallLegID: sampleLegID, CallSessionID: sampleSessionID, HangupCause: "user_busy", OccurredAt: at}},
		{EventMachinePremiumDetectionEnded, `{` + withID + `,"result":"machine"}`,
			Event{Type: EventMachinePremiumDetectionEnded, CallControlID: sampleCallID, CallLegID: sampleLegID, CallSessionID: sampleSessionID, DetectionResult: "machine", OccurredAt: at}},
		{EventRecordingSaved, fmt.Sprintf(`{`+noControlID+`,"channels":"single","recording_urls":{"mp3":%q}}`, sampleMP3URL),
			Event{Type: EventRecordingSaved, CallLegID: sampleLegID, CallSessionID: sampleSessionID, RecordingURL: sampleMP3URL, OccurredAt: at}},
		{EventRecordingError, `{` + withID + `,"reason":"Internal server error"}`,
			Event{Type: EventRecordingError, CallControlID: sampleCallID, CallLegID: sampleLegID, CallSessionID: sampleSessionID, OccurredAt: at}},
		{EventTranscriptionSaved, `{` + withID + `,"status":"completed","transcription_text":"Hi."}`,
			Event{Type: EventTranscriptionSaved, CallControlID: sampleCallID, CallLegID: sampleLegID, CallSessionID: sampleSessionID, TranscriptionText: "Hi.", OccurredAt: at}},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			got, err := ParseEvent(eventBody(tc.eventType, tc.payload))
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseEvent =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}

func TestParseUnknownType(t *testing.T) {
	// The payload carries fields of the known types; none may leak into the event.
	payload := fmt.Sprintf(`{"call_control_id":%q,"hangup_cause":"user_busy","sip_hangup_cause":"486",`+
		`"result":"machine","recording_urls":{"mp3":%q},"transcription_text":"hello"}`, sampleCallID, sampleMP3URL)
	got, err := ParseEvent(eventBody("call.initiated", payload))
	if err != nil {
		t.Fatalf("ParseEvent of an unknown type: %v", err)
	}
	want := Event{Type: "call.initiated", CallControlID: sampleCallID}
	if got != want {
		t.Fatalf("ParseEvent = %+v, want %+v", got, want)
	}
}

func TestParseUnknownTypeObjectResult(t *testing.T) {
	// Telnyx CallAIGatherEnded uses an object result, unlike premium detection.
	body := eventBody("call.ai_gather.ended", fmt.Sprintf(`{"call_control_id":%q,"result":{"age":29,"city":"Paris"}}`, sampleCallID))
	got, err := ParseEvent(body)
	if err != nil {
		t.Fatalf("ParseEvent of an unknown type with its own payload schema: %v", err)
	}
	want := Event{Type: "call.ai_gather.ended", CallControlID: sampleCallID}
	if got != want {
		t.Fatalf("ParseEvent = %+v, want %+v", got, want)
	}
}

// D-5: a signed event with a field that cannot be read keeps its type and
// every field that can be read.
func TestParseEventTolerant(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, sampleOccurredAt)
	if err != nil {
		t.Fatalf("parse sample time: %v", err)
	}
	// envelope takes occurred_at and the payload member as raw JSON.
	envelope := func(eventType, occurredAt, payloadMember string) []byte {
		return []byte(fmt.Sprintf(`{"data":{"event_type":%q,"occurred_at":%s%s}}`, eventType, occurredAt, payloadMember))
	}
	sampleAt := strconv.Quote(sampleOccurredAt)
	hangup := func(sipCause string) string {
		return fmt.Sprintf(`,"payload":{"call_control_id":%q,"call_leg_id":%q,"hangup_cause":"user_busy","sip_hangup_cause":%s}`, sampleCallID, sampleLegID, sipCause)
	}
	cases := []struct {
		name string
		body []byte
		want Event
	}{
		{"occurred_at is not a time", envelope(EventCallHangup, `"yesterday"`, hangup(`"486"`)),
			Event{Type: EventCallHangup, CallControlID: sampleCallID, CallLegID: sampleLegID, HangupCause: "user_busy", SIPHangupCause: "486"}},
		{"occurred_at is a number", envelope(EventCallHangup, `1794571201`, hangup(`"486"`)),
			Event{Type: EventCallHangup, CallControlID: sampleCallID, CallLegID: sampleLegID, HangupCause: "user_busy", SIPHangupCause: "486"}},
		{"sip_hangup_cause is a number", envelope(EventCallHangup, sampleAt, hangup(`486`)),
			Event{Type: EventCallHangup, CallControlID: sampleCallID, CallLegID: sampleLegID, HangupCause: "user_busy", SIPHangupCause: "486", OccurredAt: at}},
		{"sip_hangup_cause is an object", envelope(EventCallHangup, sampleAt, hangup(`{"code":486}`)),
			Event{Type: EventCallHangup, CallControlID: sampleCallID, CallLegID: sampleLegID, HangupCause: "user_busy", OccurredAt: at}},
		{"detection result is an object", envelope(EventMachinePremiumDetectionEnded, sampleAt, fmt.Sprintf(`,"payload":{"call_control_id":%q,"result":{"kind":"machine"}}`, sampleCallID)),
			Event{Type: EventMachinePremiumDetectionEnded, CallControlID: sampleCallID, OccurredAt: at}},
		{"no payload", envelope(EventCallHangup, sampleAt, ""),
			Event{Type: EventCallHangup, OccurredAt: at}},
		{"payload is null", envelope(EventCallHangup, sampleAt, `,"payload":null`),
			Event{Type: EventCallHangup, OccurredAt: at}},
		{"payload is text", envelope(EventCallHangup, sampleAt, `,"payload":"x"`),
			Event{Type: EventCallHangup, OccurredAt: at}},
		{"payload is a list", envelope(EventRecordingSaved, sampleAt, `,"payload":[1,2]`),
			Event{Type: EventRecordingSaved, OccurredAt: at}},
		{"unknown type without payload", envelope("call.initiated", sampleAt, ""),
			Event{Type: "call.initiated"}},
		{"unknown type, payload is text", envelope("call.initiated", sampleAt, `,"payload":"x"`),
			Event{Type: "call.initiated"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEvent(tc.body)
			if err != nil {
				t.Fatalf("ParseEvent(%s): %v", tc.body, err)
			}
			if got != tc.want {
				t.Fatalf("ParseEvent(%s) =\n %+v\nwant\n %+v", tc.body, got, tc.want)
			}
		})
	}
}

func TestParseEventRejects(t *testing.T) {
	var notJSON *json.SyntaxError
	cases := []struct {
		name string
		body string
		// noType is true when the body is JSON and the refusal is the missing type.
		noType bool
	}{
		{"not JSON", `{"data":`, false},
		{"empty body", ``, false},
		{"no event type", fmt.Sprintf(`{"data":{"payload":{"call_control_id":%q}}}`, sampleCallID), true},
		{"event type is empty", `{"data":{"event_type":"","payload":{}}}`, true},
		{"event type is a number", fmt.Sprintf(`{"data":{"event_type":7,"payload":{"call_control_id":%q}}}`, sampleCallID), true},
		{"data is text", `{"data":"call.hangup"}`, true},
		{"event type outside data", `{"event_type":"call.hangup","data":{}}`, true},
		{"JSON that is not an object", `["call.hangup"]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := ParseEvent([]byte(tc.body))
			if ev != (Event{}) {
				t.Errorf("ParseEvent returned an event with its error: %+v", ev)
			}
			if tc.noType {
				if !errors.Is(err, errNoEventType) {
					t.Fatalf("ParseEvent error = %v, want %v", err, errNoEventType)
				}
				return
			}
			if !errors.As(err, &notJSON) {
				t.Fatalf("ParseEvent error = %v (%T), want a JSON syntax error", err, err)
			}
		})
	}
}

// TC-58: only the transcription-saved payload documents the two ids.
func TestParseRecordingIDs(t *testing.T) {
	at, err := time.Parse(time.RFC3339Nano, sampleOccurredAt)
	if err != nil {
		t.Fatalf("parse sample time: %v", err)
	}
	const (
		recordingID     = "35a6f1b2-0c1d-4e8f-9a7b-1c2d3e4f5a6b"
		transcriptionID = "7b1c2d3e-4f5a-4b6c-8d9e-0f1a2b3c4d5e"
	)
	ids := fmt.Sprintf(`"recording_id":%q,"recording_transcription_id":%q`, recordingID, transcriptionID)
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      Event
	}{
		{"transcription saved with both ids", EventTranscriptionSaved,
			fmt.Sprintf(`{"call_control_id":%q,"status":"completed","transcription_text":"Hi.",%s}`, sampleCallID, ids),
			Event{Type: EventTranscriptionSaved, CallControlID: sampleCallID, TranscriptionText: "Hi.",
				RecordingID: recordingID, TranscriptionID: transcriptionID, OccurredAt: at}},
		{"transcription saved without ids", EventTranscriptionSaved,
			fmt.Sprintf(`{"call_control_id":%q,"status":"completed","transcription_text":"Hi."}`, sampleCallID),
			Event{Type: EventTranscriptionSaved, CallControlID: sampleCallID, TranscriptionText: "Hi.", OccurredAt: at}},
		{"recording saved", EventRecordingSaved,
			fmt.Sprintf(`{"call_control_id":%q,"recording_urls":{"mp3":%q},%s}`, sampleCallID, sampleMP3URL, ids),
			Event{Type: EventRecordingSaved, CallControlID: sampleCallID, RecordingURL: sampleMP3URL, OccurredAt: at}},
		{"unknown type", "call.initiated",
			fmt.Sprintf(`{"call_control_id":%q,%s}`, sampleCallID, ids),
			Event{Type: "call.initiated", CallControlID: sampleCallID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEvent(eventBody(tc.eventType, tc.payload))
			if err != nil {
				t.Fatalf("ParseEvent: %v", err)
			}
			if got != tc.want {
				t.Fatalf("ParseEvent =\n %+v\nwant\n %+v", got, tc.want)
			}
		})
	}
}
