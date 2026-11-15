// Package result holds the fixed result codes of a number check, their labels
// (TC-15: the wording of the R-lines) and the pure mappings from Telnyx signals
// to a result. Codes are what storage and REST carry; labels are what people see.
package result

// Code is the fixed storage and REST value of one result (TC-15).
type Code string

// The eleven result codes, in the contract order that All returns.
const (
	Fax                     Code = "fax"
	VoiceMessage            Code = "voice_message"
	PersonAnswered          Code = "person_answered"
	Disconnected            Code = "disconnected"
	NoAnswer                Code = "no_answer"
	PickedUp                Code = "picked_up"
	Busy                    Code = "busy"
	Unallocated             Code = "unallocated"
	DisconnectedUnallocated Code = "disconnected_unallocated"
	International           Code = "international"
	Unknown                 Code = "unknown"
)

// Telnyx Premium answering machine detection values (TS-3) that map to a result.
const (
	detectionFax           = "fax_detected"
	detectionHumanHome     = "human_residence"
	detectionHumanBusiness = "human_business"
	detectionMachine       = "machine"
	detectionSilence       = "silence"
)

// Telnyx hang-up causes for a call that was never answered (TC-6, TC-12).
const (
	causeUserBusy       = "user_busy"
	causeTimeout        = "timeout"
	causeNoAnswer       = "no_answer"
	causeNotFound       = "not_found"
	causeCallRejected   = "call_rejected"
	causeNormalClearing = "normal_clearing"
	causeUnspecified    = "unspecified"
)

// Reasons given with Unknown (R-20, R-22).
const (
	reasonDetectionUnclear = "Telnyx could not tell what answered"
	reasonCarrierMessage   = "phone company message, not recorded"
	reasonNetworkEnded     = "network ended the call: "
)

// ReasonInternational is the reason saved with International (TC-43): the
// number is of another country and is never dialled (R-28).
const ReasonInternational = "not called: outside US and Canada"

// The network's own code for a hang-up, added to an Unknown reason (TC-40).
// Telnyx sends sipUnspecified when the network gave it no code.
const (
	sipUnspecified   = "unspecified"
	networkCodeOpen  = " (network code "
	networkCodeClose = ")"
)

// ordered is the single list of codes; All copies it so callers cannot change it.
var ordered = []Code{
	Fax,
	VoiceMessage,
	PersonAnswered,
	Disconnected,
	NoAnswer,
	PickedUp,
	Busy,
	Unallocated,
	DisconnectedUnallocated,
	International,
	Unknown,
}

var labels = map[Code]string{
	Fax:                     "Fax",
	VoiceMessage:            "Voice message",
	PersonAnswered:          "Person answered",
	Disconnected:            "Disconnected",
	NoAnswer:                "No answer",
	PickedUp:                "Picked up",
	Busy:                    "Busy",
	Unallocated:             "Unallocated",
	DisconnectedUnallocated: "Disconnected / Unallocated",
	International:           "International",
	Unknown:                 "Unknown",
}

var detectionCodes = map[string]Code{
	detectionFax:           Fax,
	detectionHumanHome:     PersonAnswered,
	detectionHumanBusiness: PersonAnswered,
	detectionMachine:       VoiceMessage,
	detectionSilence:       PickedUp,
}

var hangupCodes = map[string]Code{
	causeUserBusy:     Busy,
	causeTimeout:      NoAnswer,
	causeNoAnswer:     NoAnswer,
	causeNotFound:     Unallocated,
	causeCallRejected: Disconnected,
}

// All returns the eleven codes in contract order. The slice is a fresh copy.
func All() []Code {
	return append([]Code(nil), ordered...)
}

// Label returns the text shown for code, or "" for a value that is not one of
// the eleven codes.
func Label(code Code) string {
	return labels[code]
}

// Reusable reports whether a stored result may be reused instead of dialling
// again. Only Unknown is redialled on the next request (R-21).
func Reusable(code Code) bool {
	return code != Unknown
}

// FromDetection maps a Telnyx Premium detection value (TS-3) to a result.
// The reason is empty for a mapped value; any other value gives Unknown with
// the reason that Telnyx could not tell what answered (R-20, TC-14).
func FromDetection(premium string) (Code, string) {
	if code, ok := detectionCodes[premium]; ok {
		return code, ""
	}
	return Unknown, reasonDetectionUnclear
}

// FromHangup maps the Telnyx hang-up cause of a call that was never answered
// to a result. normal_clearing and unspecified before answer are taken as a
// phone-company message that could not be recorded (R-22, TC-12). When combine
// is true (R-23), Unallocated and Disconnected both become
// DisconnectedUnallocated. The reason is empty unless the code is Unknown;
// an Unknown reason ends with the network code sip when Telnyx sent one
// (D-8, TC-40: 404 and 480 were seen on calls with different outcomes).
func FromHangup(cause, sip string, combine bool) (Code, string) {
	code, ok := hangupCodes[cause]
	if !ok {
		switch cause {
		case causeNormalClearing, causeUnspecified:
			return Unknown, reasonCarrierMessage + networkCode(sip)
		default:
			return Unknown, reasonNetworkEnded + cause + networkCode(sip)
		}
	}
	if combine && (code == Unallocated || code == Disconnected) {
		return DisconnectedUnallocated, ""
	}
	return code, ""
}

// networkCode is the reason suffix that names sip, or "" when Telnyx sent no
// network code.
func networkCode(sip string) string {
	if sip == "" || sip == sipUnspecified {
		return ""
	}
	return networkCodeOpen + sip + networkCodeClose
}
