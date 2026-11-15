package telnyx

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Event types the call flow acts on. Field names and values follow the Telnyx
// Call Control OpenAPI spec (call-control.json) read 2026-11-13.
const (
	EventCallAnswered                 = "call.answered"
	EventCallHangup                   = "call.hangup"
	EventMachinePremiumDetectionEnded = "call.machine.premium.detection.ended"
	EventRecordingSaved               = "call.recording.saved"
	EventRecordingError               = "call.recording.error"
	EventTranscriptionSaved           = "call.recording.transcription.saved"
)

// maxEventAge is how far the signed timestamp may be from now in either
// direction: it blocks replays and tolerates clock drift (AD-6, TC-7; Telnyx
// recommends 5 minutes).
const maxEventAge = 5 * time.Minute

// signedSeparator joins the timestamp and the raw body in the signed message:
// Telnyx signs "<timestamp>|<body>".
const signedSeparator = "|"

var (
	errMissingHeader  = errors.New("missing signature or timestamp header")
	errBadPublicKey   = errors.New("public key has the wrong length")
	errBadTimestamp   = errors.New("timestamp is not unix seconds")
	errStaleTimestamp = errors.New("timestamp outside the allowed window")
	errBadEncoding    = errors.New("signature is not valid base64")
	errBadSignature   = errors.New("signature does not match")
	errNoEventType    = errors.New("event has no data.event_type")
)

// Verify checks that body was signed by the holder of pub's private key
// together with timestamp, and that timestamp is within maxEventAge of now.
// An unsigned or wrongly signed event must never be accepted (AD-6).
func Verify(pub ed25519.PublicKey, timestamp, signature string, body []byte, now time.Time) error {
	if timestamp == "" || signature == "" {
		return fmt.Errorf("telnyx verify event: %w", errMissingHeader)
	}
	// ed25519.Verify panics on a key of the wrong length.
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("telnyx verify event: %w (%d bytes)", errBadPublicKey, len(pub))
	}
	secs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("telnyx verify event: %w: %q", errBadTimestamp, timestamp)
	}
	age := now.Sub(time.Unix(secs, 0))
	if age > maxEventAge || age < -maxEventAge {
		return fmt.Errorf("telnyx verify event: %w: timestamp %s is %s from now", errStaleTimestamp, timestamp, age)
	}
	sig, err := base64.StdEncoding.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("telnyx verify event: %w: %v", errBadEncoding, err)
	}
	msg := make([]byte, 0, len(timestamp)+len(signedSeparator)+len(body))
	msg = append(msg, timestamp...)
	msg = append(msg, signedSeparator...)
	msg = append(msg, body...)
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("telnyx verify event: %w (timestamp %s, %d byte body)", errBadSignature, timestamp, len(body))
	}
	return nil
}

// Event is one Telnyx call event, reduced to the fields the call flow uses.
type Event struct {
	Type          string
	CallControlID string
	// CallLegID and CallSessionID identify the call in every payload,
	// including call.recording.saved, which has no call_control_id in the
	// Telnyx spec. The runner correlates such events by them.
	CallLegID         string
	CallSessionID     string
	HangupCause       string
	SIPHangupCause    string
	DetectionResult   string
	RecordingURL      string
	TranscriptionText string
	OccurredAt        time.Time
}

type eventEnvelope struct {
	Data struct {
		EventType  string       `json:"event_type"`
		OccurredAt string       `json:"occurred_at"`
		Payload    eventPayload `json:"payload"`
	} `json:"data"`
}

type eventPayload struct {
	CallControlID     string       `json:"call_control_id"`
	CallLegID         string       `json:"call_leg_id"`
	CallSessionID     string       `json:"call_session_id"`
	HangupCause       string       `json:"hangup_cause"`
	SIPHangupCause    numberOrText `json:"sip_hangup_cause"`
	Result            string       `json:"result"`
	TranscriptionText string       `json:"transcription_text"`
	RecordingURLs     struct {
		// The dial asks for mp3 recordings (client.go recordingFormat).
		MP3 string `json:"mp3"`
	} `json:"recording_urls"`
}

// numberOrText is a JSON string, or a JSON number kept as its text: the spec
// gives sip_hangup_cause as "486", and D-5 covers an event that sends 486.
type numberOrText string

// UnmarshalJSON never fails: a value that is neither a string nor a number
// leaves the field empty, like any other field that cannot be read (D-5).
func (v *numberOrText) UnmarshalJSON(raw []byte) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		*v = numberOrText(text)
		return nil
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		*v = numberOrText(number)
	}
	return nil
}

// ParseEvent reads a Telnyx webhook body. Only the fields of the event's own
// type are filled; any other type gets just Type and CallControlID, so stray
// payload fields of events the flow ignores cannot reach it (TS-2).
// Only a body that is not JSON or has no data.event_type is an error: the
// event is signed, so a field that cannot be read is left empty instead of
// losing the whole event (D-5).
func ParseEvent(body []byte) (Event, error) {
	var env eventEnvelope
	// A value whose JSON type does not fit its field (a payload that is not
	// an object, a number where text belongs) is not a failure: encoding/json
	// skips it, fills everything else, and only then reports it as
	// *UnmarshalTypeError.
	var typeErr *json.UnmarshalTypeError
	if err := json.Unmarshal(body, &env); err != nil && !errors.As(err, &typeErr) {
		return Event{}, fmt.Errorf("telnyx parse event: %w", err)
	}
	d, payload := env.Data, env.Data.Payload
	if d.EventType == "" {
		return Event{}, fmt.Errorf("telnyx parse event: %w", errNoEventType)
	}
	ev := Event{Type: d.EventType, CallControlID: payload.CallControlID}
	switch d.EventType {
	case EventCallAnswered, EventRecordingError:
		// Known types with nothing beyond the call's identity.
	case EventCallHangup:
		ev.HangupCause = payload.HangupCause
		ev.SIPHangupCause = string(payload.SIPHangupCause)
	case EventMachinePremiumDetectionEnded:
		ev.DetectionResult = payload.Result
	case EventRecordingSaved:
		ev.RecordingURL = payload.RecordingURLs.MP3
	case EventTranscriptionSaved:
		ev.TranscriptionText = payload.TranscriptionText
	default:
		return ev, nil
	}
	ev.CallLegID = payload.CallLegID
	ev.CallSessionID = payload.CallSessionID
	// An occurred_at that is missing or cannot be parsed leaves OccurredAt zero (D-5).
	if at, err := time.Parse(time.RFC3339Nano, d.OccurredAt); err == nil {
		ev.OccurredAt = at
	}
	return ev, nil
}
