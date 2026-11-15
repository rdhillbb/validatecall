// Package phone turns a typed phone number into one standard form, +1 and ten
// digits (TC-8), so the same number written two ways matches in the lookup.
// Only North American +1 numbers are accepted (R-24).
package phone

import (
	"fmt"
	"strings"
)

// Messages shown to the person when a number is refused (R-24).
const (
	msgEmpty        = "Enter a phone number."
	msgOtherCountry = "Only US and Canada numbers (+1) can be checked."
	msgInvalid      = "This is not a valid US or Canada number."
)

const (
	internationalPrefix = '+'
	countryCode         = '1'
	standardPrefix      = "+1"
	firstDigit          = '0'
	lastDigit           = '9'
	// separators people type between digit groups; any other character refuses the number.
	separators     = " -.()"
	nationalDigits = 10
	// The area code is digits 0-2 and the exchange digits 3-5 of the ten;
	// in the +1 plan both must start with 2 to 9.
	areaCodeStart = 0
	exchangeStart = 3
	lowestLead    = '2'
	// Digits of a number of another country, country code included (TC-43):
	// E.164 allows at most 15.
	minInternationalDigits = 8
	maxInternationalDigits = 15
)

// InvalidError reports why a number was refused. Message is the plain text
// for the person; it is one of the three R-24 messages.
type InvalidError struct {
	Input   string
	Message string
}

func (e *InvalidError) Error() string {
	return fmt.Sprintf("normalize phone number %q: %s", e.Input, e.Message)
}

// Normalize returns input as +1 followed by ten digits, or an *InvalidError.
// It accepts spaces, dashes, dots, brackets, and an optional leading 1 or +1.
// A refused number never yields a value (R-24).
func Normalize(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", &InvalidError{Input: input, Message: msgEmpty}
	}

	international := trimmed[0] == internationalPrefix
	if international {
		trimmed = trimmed[1:]
	}

	var digits strings.Builder
	for _, r := range trimmed {
		switch {
		case r >= firstDigit && r <= lastDigit:
			digits.WriteRune(r)
		case strings.ContainsRune(separators, r):
			continue
		default:
			return "", &InvalidError{Input: input, Message: msgInvalid}
		}
	}
	number := digits.String()
	if number == "" {
		return "", &InvalidError{Input: input, Message: msgInvalid}
	}

	hasCountryCode := number[0] == countryCode && len(number) == nationalDigits+1
	switch {
	case international && number[0] != countryCode:
		return "", &InvalidError{Input: input, Message: msgOtherCountry}
	case hasCountryCode:
		number = number[1:]
	case international || len(number) != nationalDigits:
		return "", &InvalidError{Input: input, Message: msgInvalid}
	}

	if number[areaCodeStart] < lowestLead || number[exchangeStart] < lowestLead {
		return "", &InvalidError{Input: input, Message: msgInvalid}
	}
	return standardPrefix + number, nil
}

// International reports whether input is a number of a country other than the
// US and Canada (R-28, TC-43), and returns it as + followed by its digits.
// Without a leading + the country cannot be told, so such input is never
// international; a + followed by 1 is the +1 plan, which Normalize judges.
// Normalize itself still refuses every number this function accepts (R-24).
func International(input string) (number string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" || trimmed[0] != internationalPrefix {
		return "", false
	}

	var digits strings.Builder
	for _, r := range trimmed[1:] {
		switch {
		case r >= firstDigit && r <= lastDigit:
			digits.WriteRune(r)
		case strings.ContainsRune(separators, r):
			continue
		default:
			return "", false
		}
	}
	found := digits.String()
	if len(found) < minInternationalDigits || len(found) > maxInternationalDigits {
		return "", false
	}
	// No country code starts with 0.
	if found[0] == countryCode || found[0] == firstDigit {
		return "", false
	}
	return string(internationalPrefix) + found, true
}
