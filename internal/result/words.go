package result

import (
	"strings"
	"unicode"
)

// Reason given with Unknown when the words are a phone-company message or
// cannot be told apart from a greeting (R-35, TC-56); distinct from
// reasonCarrierMessage, which means nothing was recorded (R-22).
const reasonPhoneCompanySeeText = "phone company message, see text"

// Contractions rewritten before matching (TC-54): "can't" first, because the
// general rule would make it "ca not".
const (
	contractionCant   = "can't"
	contractionCantAs = "cannot"
	contractionNot    = "n't"
	contractionNotAs  = " not"
	wordSeparator     = " "
)

// The phrase lists of TC-54, fixed in code so they are easy to read and test.
var (
	numberGonePhrases = []string{
		"not in use",
		"no longer in use",
		"not in service",
		"no longer in service",
		"been disconnected",
		"is disconnected",
		"not a working number",
		"no longer a working number",
	}
	greetingPhrases = []string{
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
	phoneCompanyPhrases = []string{
		"temporarily",
		"cannot be completed",
		"could not be completed",
		"has been changed",
		"check the number",
		"dial again",
		"please hang up",
	}
)

// FromWords maps the words Telnyx transcribed on an answered call to a result
// (TC-54). A phone-company phrase, or both a number-gone and a greeting phrase,
// gives Unknown with a reason, so an unclear message goes to a person and never
// to a wrong Disconnected (R-35). Otherwise number gone gives Disconnected, or
// DisconnectedUnallocated when combine is true (R-23, R-34), and a greeting
// gives VoiceMessage (R-36). ok is false when no phrase matches (R-38); code and
// reason are then empty.
func FromWords(text string, combine bool) (code Code, reason string, ok bool) {
	words := normalizeWords(text)
	gone := containsPhrase(words, numberGonePhrases)
	greeting := containsPhrase(words, greetingPhrases)
	if containsPhrase(words, phoneCompanyPhrases) || (gone && greeting) {
		return Unknown, reasonPhoneCompanySeeText, true
	}
	if gone {
		if combine {
			return DisconnectedUnallocated, "", true
		}
		return Disconnected, "", true
	}
	if greeting {
		return VoiceMessage, "", true
	}
	return "", "", false
}

// normalizeWords lower-cases text, rewrites contractions, turns every other
// character that is not a letter or digit into a separator, and returns the
// words joined by single spaces with a space at each end, so a phrase matches
// only on whole words.
func normalizeWords(text string) string {
	lower := strings.ToLower(text)
	lower = strings.ReplaceAll(lower, contractionCant, contractionCantAs)
	lower = strings.ReplaceAll(lower, contractionNot, contractionNotAs)
	spaced := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, lower)
	return wordSeparator + strings.Join(strings.Fields(spaced), wordSeparator) + wordSeparator
}

// containsPhrase reports whether words (from normalizeWords) holds any phrase
// of phrases as whole words.
func containsPhrase(words string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(words, wordSeparator+p+wordSeparator) {
			return true
		}
	}
	return false
}
