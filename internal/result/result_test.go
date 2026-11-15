package result

import (
	"reflect"
	"testing"
)

// Expected codes, labels and reasons are written out here, not taken from the
// package, so a changed spelling fails the test (TC-15, R-20, R-22, TC-43).
const (
	wantDetectionUnclear = "Telnyx could not tell what answered"
	wantCarrierMessage   = "phone company message, not recorded"
	wantInternational    = "not called: outside US and Canada"
)

func TestCodesAndLabels(t *testing.T) {
	want := []struct {
		code  Code
		label string
	}{
		{"fax", "Fax"},
		{"voice_message", "Voice message"},
		{"person_answered", "Person answered"},
		{"disconnected", "Disconnected"},
		{"no_answer", "No answer"},
		{"picked_up", "Picked up"},
		{"busy", "Busy"},
		{"unallocated", "Unallocated"},
		{"disconnected_unallocated", "Disconnected / Unallocated"},
		{"international", "International"},
		{"unknown", "Unknown"},
	}

	all := All()
	if len(all) != len(want) {
		t.Fatalf("All() returned %d codes %v; want %d", len(all), all, len(want))
	}
	for i, w := range want {
		if all[i] != w.code {
			t.Errorf("All()[%d] = %q; want %q", i, all[i], w.code)
		}
		if got := Label(w.code); got != w.label {
			t.Errorf("Label(%q) = %q; want %q", w.code, got, w.label)
		}
	}

	all[0] = "changed"
	if again := All(); again[0] != "fax" {
		t.Errorf("All() after changing a returned slice starts with %q; want \"fax\"", again[0])
	}
	if got := Label("not_a_code"); got != "" {
		t.Errorf("Label(\"not_a_code\") = %q; want \"\"", got)
	}
}

func TestReasonInternational(t *testing.T) {
	if ReasonInternational != wantInternational {
		t.Errorf("ReasonInternational = %q; want %q", ReasonInternational, wantInternational)
	}
}

func TestFromDetection(t *testing.T) {
	cases := []struct {
		premium    string
		wantCode   Code
		wantReason string
	}{
		{"fax_detected", "fax", ""},
		{"human_residence", "person_answered", ""},
		{"human_business", "person_answered", ""},
		{"machine", "voice_message", ""},
		{"silence", "picked_up", ""},
		{"not_sure", "unknown", wantDetectionUnclear},
		{"human", "unknown", wantDetectionUnclear},
		{"", "unknown", wantDetectionUnclear},
		{"FAX_DETECTED", "unknown", wantDetectionUnclear},
	}
	for _, c := range cases {
		t.Run(c.premium, func(t *testing.T) {
			code, reason := FromDetection(c.premium)
			if code != c.wantCode || reason != c.wantReason {
				t.Errorf("FromDetection(%q) = (%q, %q); want (%q, %q)",
					c.premium, code, reason, c.wantCode, c.wantReason)
			}
		})
	}
}

func TestFromHangup(t *testing.T) {
	cases := []struct {
		cause      string
		wantCode   Code
		wantReason string
	}{
		{"user_busy", "busy", ""},
		{"timeout", "no_answer", ""},
		{"no_answer", "no_answer", ""},
		{"not_found", "unallocated", ""},
		{"call_rejected", "disconnected", ""},
		{"normal_clearing", "unknown", wantCarrierMessage},
		{"unspecified", "unknown", wantCarrierMessage},
		{"originator_cancel", "unknown", "network ended the call: originator_cancel"},
		{"destination_out_of_order", "unknown", "network ended the call: destination_out_of_order"},
		{"", "unknown", "network ended the call: "},
	}
	for _, c := range cases {
		t.Run(c.cause, func(t *testing.T) {
			code, reason := FromHangup(c.cause, "", false)
			if code != c.wantCode || reason != c.wantReason {
				t.Errorf("FromHangup(%q, \"\", false) = (%q, %q); want (%q, %q)",
					c.cause, code, reason, c.wantCode, c.wantReason)
			}
		})
	}
}

func TestFromHangupCombine(t *testing.T) {
	cases := []struct {
		cause      string
		combine    bool
		wantCode   Code
		wantReason string
	}{
		{"not_found", true, "disconnected_unallocated", ""},
		{"call_rejected", true, "disconnected_unallocated", ""},
		{"not_found", false, "unallocated", ""},
		{"call_rejected", false, "disconnected", ""},
		// combine changes only the two R-23 results.
		{"user_busy", true, "busy", ""},
		{"timeout", true, "no_answer", ""},
		{"no_answer", true, "no_answer", ""},
		{"normal_clearing", true, "unknown", wantCarrierMessage},
		{"unspecified", true, "unknown", wantCarrierMessage},
		{"originator_cancel", true, "unknown", "network ended the call: originator_cancel"},
	}
	for _, c := range cases {
		code, reason := FromHangup(c.cause, "", c.combine)
		if code != c.wantCode || reason != c.wantReason {
			t.Errorf("FromHangup(%q, \"\", %v) = (%q, %q); want (%q, %q)",
				c.cause, c.combine, code, reason, c.wantCode, c.wantReason)
		}
	}
}

// TC-40: only an Unknown reason carries the network code, and only when Telnyx
// sent one; "unspecified" is Telnyx's word for no code.
func TestFromHangupNetworkCode(t *testing.T) {
	cases := []struct {
		name       string
		cause      string
		sip        string
		combine    bool
		wantCode   Code
		wantReason string
	}{
		{"unspecified with 480", "unspecified", "480", false, "unknown", wantCarrierMessage + " (network code 480)"},
		{"normal clearing with 404", "normal_clearing", "404", false, "unknown", wantCarrierMessage + " (network code 404)"},
		{"unmapped cause with 487", "originator_cancel", "487", false, "unknown", "network ended the call: originator_cancel (network code 487)"},
		{"unknown combined keeps the code", "unspecified", "480", true, "unknown", wantCarrierMessage + " (network code 480)"},
		{"no code sent", "unspecified", "", false, "unknown", wantCarrierMessage},
		{"code unspecified", "unspecified", "unspecified", false, "unknown", wantCarrierMessage},
		{"unmapped cause, code unspecified", "originator_cancel", "unspecified", false, "unknown", "network ended the call: originator_cancel"},
		{"busy with 486", "user_busy", "486", false, "busy", ""},
		{"no answer with 480", "no_answer", "480", false, "no_answer", ""},
		{"unallocated with 404", "not_found", "404", false, "unallocated", ""},
		{"rejected with 403", "call_rejected", "403", false, "disconnected", ""},
		{"combined with 404", "not_found", "404", true, "disconnected_unallocated", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, reason := FromHangup(c.cause, c.sip, c.combine)
			if code != c.wantCode || reason != c.wantReason {
				t.Errorf("FromHangup(%q, %q, %v) = (%q, %q); want (%q, %q)",
					c.cause, c.sip, c.combine, code, reason, c.wantCode, c.wantReason)
			}
		})
	}
}

func TestReusable(t *testing.T) {
	want := map[Code]bool{
		"fax":                      true,
		"voice_message":            true,
		"person_answered":          true,
		"disconnected":             true,
		"no_answer":                true,
		"picked_up":                true,
		"busy":                     true,
		"unallocated":              true,
		"disconnected_unallocated": true,
		"international":            true,
		"unknown":                  false,
	}
	got := map[Code]bool{}
	for _, code := range All() {
		got[code] = Reusable(code)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Reusable over All() = %v; want %v", got, want)
	}
}
