package result

import "testing"

// Expected codes and the reason are written out here, not taken from the
// package, so a changed spelling fails the test (TC-15, TC-56).
const wantPhoneCompanySeeText = "phone company message, see text"

type wordsCase struct {
	name       string
	text       string
	combine    bool
	wantCode   Code
	wantReason string
	wantOK     bool
}

func runWordsCases(t *testing.T, cases []wordsCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, reason, ok := FromWords(c.text, c.combine)
			if code != c.wantCode || reason != c.wantReason || ok != c.wantOK {
				t.Errorf("FromWords(%q, %v) = (%q, %q, %v); want (%q, %q, %v)",
					c.text, c.combine, code, reason, ok, c.wantCode, c.wantReason, c.wantOK)
			}
		})
	}
}

// TC-54 number-gone list, each phrase on its own and inside a sentence (R-34).
func TestFromWordsNumberGone(t *testing.T) {
	phrases := []string{
		"not in use",
		"no longer in use",
		"not in service",
		"no longer in service",
		"been disconnected",
		"is disconnected",
		"not a working number",
		"no longer a working number",
	}
	var cases []wordsCase
	for _, p := range phrases {
		cases = append(cases,
			wordsCase{name: p, text: p, wantCode: "disconnected", wantOK: true},
			wordsCase{name: p + " in sentence", text: "The number you have called, " + p + "!", wantCode: "disconnected", wantOK: true},
		)
	}
	cases = append(cases,
		wordsCase{name: "D-11 message", text: "This number is not in use. Goodbye", wantCode: "disconnected", wantOK: true},
		wordsCase{name: "TC-54 example isn't", text: "This number isn't in use", wantCode: "disconnected", wantOK: true},
		wordsCase{name: "upper case", text: "THIS NUMBER IS NO LONGER IN SERVICE.", wantCode: "disconnected", wantOK: true},
		wordsCase{name: "phrase across punctuation and spaces", text: "not\tin-use", wantCode: "disconnected", wantOK: true},
	)
	runWordsCases(t, cases)
}

// R-23: with combine true a number-gone text gives the combined result.
func TestFromWordsCombine(t *testing.T) {
	runWordsCases(t, []wordsCase{
		{name: "D-11 message combined", text: "This number is not in use. Goodbye", combine: true, wantCode: "disconnected_unallocated", wantOK: true},
		{name: "isn't combined", text: "This number isn't in use", combine: true, wantCode: "disconnected_unallocated", wantOK: true},
		{name: "greeting not combined", text: "Please leave a message", combine: true, wantCode: "voice_message", wantOK: true},
		{name: "phone company not combined", text: "Your call cannot be completed as dialed", combine: true, wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		{name: "no phrase combined", text: "hello who is this", combine: true},
	})
}

// TC-54 greeting list, each phrase on its own and inside a sentence (R-36).
func TestFromWordsGreeting(t *testing.T) {
	phrases := []string{
		"leave a message",
		"leave your message",
		"leave me a message",
		"record your message",
		"after the tone",
		"at the tone",
		"after the beep",
		"not available",
		"unavailable",
		"voicemail",
		"voice mail",
		"mailbox",
	}
	var cases []wordsCase
	for _, p := range phrases {
		cases = append(cases,
			wordsCase{name: p, text: p, wantCode: "voice_message", wantOK: true},
			wordsCase{name: p + " in sentence", text: "Hi, you reached Sam; " + p + ", thanks.", wantCode: "voice_message", wantOK: true},
		)
	}
	cases = append(cases,
		wordsCase{name: "TC-54 example", text: "The number you have dialed is not available. At the tone, please record your message", wantCode: "voice_message", wantOK: true},
		wordsCase{name: "R-36 example", text: "The person you are calling is not available, please leave a message after the tone", wantCode: "voice_message", wantOK: true},
		wordsCase{name: "isn't available", text: "Sam isn't available right now", wantCode: "voice_message", wantOK: true},
	)
	runWordsCases(t, cases)
}

// TC-54 other phone-company list, each phrase on its own and with a number-gone
// or greeting phrase beside it: the phone-company phrase wins (R-35).
func TestFromWordsPhoneCompany(t *testing.T) {
	phrases := []string{
		"temporarily",
		"cannot be completed",
		"could not be completed",
		"has been changed",
		"check the number",
		"dial again",
		"please hang up",
	}
	var cases []wordsCase
	for _, p := range phrases {
		cases = append(cases,
			wordsCase{name: p, text: p, wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
			wordsCase{name: p + " with number gone", text: "This number is not in use, " + p, wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
			wordsCase{name: p + " with greeting", text: p + ". Please leave a message", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		)
	}
	cases = append(cases,
		wordsCase{name: "TC-54 example", text: "temporarily unavailable", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		wordsCase{name: "R-35 example", text: "Your call cannot be completed as dialed", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		wordsCase{name: "can't be completed", text: "Your call can't be completed as dialed", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		wordsCase{name: "couldn't be completed", text: "The call couldn't be completed", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		wordsCase{name: "upper case", text: "TEMPORARILY OUT OF SERVICE", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
	)
	runWordsCases(t, cases)
}

// Both number-gone and greeting phrases cannot be told apart: Unknown (R-35).
func TestFromWordsUnclear(t *testing.T) {
	runWordsCases(t, []wordsCase{
		{name: "not in use and leave a message", text: "This number is not in use. Please leave a message", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		{name: "disconnected and mailbox", text: "The mailbox has been disconnected", wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
		{name: "not in service and not available", text: "Not in service, not available", combine: true, wantCode: "unknown", wantReason: wantPhoneCompanySeeText, wantOK: true},
	})
}

// No phrase, an empty text, and a phrase only inside a longer word: ok false,
// empty code and reason (R-38).
func TestFromWordsNoMatch(t *testing.T) {
	runWordsCases(t, []wordsCase{
		{name: "R-38 example", text: "hello who is this"},
		{name: "empty", text: ""},
		{name: "only punctuation and spaces", text: " ... !? "},
		{name: "mailboxes full", text: "mailboxes full"},
		{name: "first phrase word inside a longer word", text: "knot in use"},
		{name: "letter before phrase", text: "évoicemail"},
		{name: "digit after phrase", text: "mailbox2"},
		{name: "voicemails", text: "voicemails"},
		{name: "phone-company phrase inside a longer word", text: "temporarilyx"},
		{name: "no phrase with combine", text: "hello who is this", combine: true},
		{name: "phrase words split by other words", text: "not really in use"},
	})
}
