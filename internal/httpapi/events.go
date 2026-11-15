package httpapi

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"checknumber/internal/telnyx"
)

const (
	eventsPath = "/telnyx/events"
	// Telnyx sends the ed25519 signature and the signed unix timestamp in
	// these headers (TC-7).
	headerSignature = "telnyx-signature-ed25519"
	headerTimestamp = "telnyx-timestamp"
	// maxEventBody bounds what is read before the signature can be checked:
	// this is the one address strangers reach (AD-6, D-4). Telnyx events are a
	// few KB (TC-38).
	maxEventBody = 4 << 20
	// callIDTail is how much of a call id a log line shows (TC-39).
	callIDTail = 6

	reasonNotJSON = "body is not JSON"
)

// EventSink receives verified Telnyx events; *check.Runner satisfies it.
type EventSink interface {
	HandleEvent(telnyx.Event)
}

// NewEvents returns the handler the tunnel exposes. It serves only
// POST /telnyx/events and passes an event to sink only when its signature
// verifies against pub at now (AD-6, TC-7); everything else is 404.
func NewEvents(pub ed25519.PublicKey, sink EventSink, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != eventsPath {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventBody))
		if err != nil {
			status, reason := http.StatusBadRequest, "read body: "+refusalReason(err)
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				status, reason = http.StatusRequestEntityTooLarge, fmt.Sprintf("body above %d bytes", tooLarge.Limit)
			}
			logRefused(reason)
			http.Error(w, http.StatusText(status), status)
			return
		}
		if err := telnyx.Verify(pub, r.Header.Get(headerTimestamp), r.Header.Get(headerSignature), body, now()); err != nil {
			logRefused(refusalReason(err))
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		ev, err := telnyx.ParseEvent(body)
		if err != nil {
			// The signature is good, so this came from Telnyx: it is
			// acknowledged like any other event and only left out (D-5).
			logRefused(refusalReason(err))
			w.WriteHeader(http.StatusOK)
			return
		}
		log.Print(acceptedLine(ev))
		// The parsed event goes to the sink unchanged: call.recording.saved
		// identifies its call only by CallLegID and CallSessionID.
		sink.HandleEvent(ev)
		w.WriteHeader(http.StatusOK)
	})
}

func logRefused(reason string) {
	log.Printf("telnyx event refused: %s", reason)
}

// refusalReason is the innermost error's text. telnyx.Verify and
// telnyx.ParseEvent put what the request carried (the timestamp header, the
// body size) only in the text they wrap around it, and a log line must not
// repeat request input (C-4).
func refusalReason(err error) string {
	// encoding/json quotes the body character it stopped at.
	var syntax *json.SyntaxError
	if errors.As(err, &syntax) {
		return reasonNotJSON
	}
	for inner := errors.Unwrap(err); inner != nil; inner = errors.Unwrap(inner) {
		err = inner
	}
	return err.Error()
}

// acceptedLine is the TC-39 line for one accepted event; a field the event
// does not carry is left out.
func acceptedLine(ev telnyx.Event) string {
	var line strings.Builder
	line.WriteString("telnyx event " + ev.Type)
	for _, field := range []struct{ name, value string }{
		{"call", lastChars(ev.CallControlID, callIDTail)},
		{"result", ev.DetectionResult},
		{"cause", ev.HangupCause},
		{"sip", ev.SIPHangupCause},
	} {
		if field.value != "" {
			line.WriteString(" " + field.name + "=" + field.value)
		}
	}
	return line.String()
}

func lastChars(s string, n int) string {
	chars := []rune(s)
	if len(chars) <= n {
		return s
	}
	return string(chars[len(chars)-n:])
}
