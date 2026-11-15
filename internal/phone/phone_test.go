package phone

import (
	"errors"
	"testing"
)

// The expected messages are written out here, not taken from the package
// constants, so a changed message text fails the test (R-24 wording).
const (
	wantEmpty        = "Enter a phone number."
	wantOtherCountry = "Only US and Canada numbers (+1) can be checked."
	wantInvalid      = "This is not a valid US or Canada number."
)

// assertRefused checks that input is refused with exactly wantMessage and that
// no number is returned with the error.
func assertRefused(t *testing.T, input, wantMessage string) {
	t.Helper()
	got, err := Normalize(input)
	if got != "" {
		t.Errorf("Normalize(%q) returned %q with a refusal; want empty", input, got)
	}
	var invalid *InvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("Normalize(%q) error = %v (%T); want *InvalidError", input, err, err)
	}
	if invalid.Message != wantMessage {
		t.Errorf("Normalize(%q) message = %q; want %q", input, invalid.Message, wantMessage)
	}
	if invalid.Input != input {
		t.Errorf("Normalize(%q) error Input = %q; want the input", input, invalid.Input)
	}
}

func TestNormalizeAcceptedForms(t *testing.T) {
	const want = "+12125550142"
	inputs := []string{
		"(212) 555-0142",
		"212.555.0142",
		"1 212 555 0142",
		"+12125550142",
		"2125550142",
		"+1 (212) 555-0142",
		"  212-555-0142  ",
	}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			got, err := Normalize(input)
			if err != nil {
				t.Fatalf("Normalize(%q) error = %v; want nil", input, err)
			}
			if got != want {
				t.Errorf("Normalize(%q) = %q; want %q", input, got, want)
			}
		})
	}
}

func TestNormalizeEmpty(t *testing.T) {
	for _, input := range []string{"", "   ", "\t\n"} {
		t.Run(input, func(t *testing.T) {
			assertRefused(t, input, wantEmpty)
		})
	}
}

func TestNormalizeOtherCountry(t *testing.T) {
	for _, input := range []string{"+44 20 7946 0958", "+33 1 23 45 67 89", "+52 55 1234 5678"} {
		t.Run(input, func(t *testing.T) {
			assertRefused(t, input, wantOtherCountry)
		})
	}
}

func TestInternational(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"United Kingdom", "+44 20 7946 0958", "+442079460958"},
		{"France", "+33 1 23 45 67 89", "+33123456789"},
		{"Mexico", "+52 55 1234 5678", "+525512345678"},
		{"no separators", "+442079460958", "+442079460958"},
		{"every separator", "  +44 (20) 7946-09.58  ", "+442079460958"},
		{"eight digits, the fewest", "+44 207 946", "+44207946"},
		{"fifteen digits, the most", "+44 20 7946 0958 123", "+442079460958123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := International(tc.input)
			if !ok || got != tc.want {
				t.Errorf("International(%q) = (%q, %v); want (%q, true)", tc.input, got, ok, tc.want)
			}
		})
	}
}

func TestInternationalRejects(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"plus one number", "+1 212 555 0142"},
		{"plus one number without separators", "+12125550142"},
		{"plus one with fifteen digits", "+1 212 555 0142 1234"},
		{"other country without plus", "44 20 7946 0958"},
		{"US number without plus", "(212) 555-0142"},
		{"seven digits", "+44 20 794"},
		{"sixteen digits", "+44 20 7946 0958 1234"},
		{"a letter", "+44 20 7946 095A"},
		{"letters only", "+phone"},
		{"other symbol", "+44#20#7946#0958"},
		{"plus inside", "+44 20+7946 0958"},
		{"two plus signs", "++44 20 7946 0958"},
		{"starts with zero", "+044 20 7946 0958"},
		{"plus only", "+"},
		{"plus and separators only", "+ (-.)"},
		{"empty", ""},
		{"spaces only", "   "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := International(tc.input)
			if ok || got != "" {
				t.Errorf("International(%q) = (%q, %v); want (\"\", false)", tc.input, got, ok)
			}
		})
	}
}

func TestNormalizeInvalid(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"too short", "123"},
		{"nine digits", "212 555 014"},
		{"eleven digits without leading 1", "2212 555 0142"},
		{"twelve digits", "1 212 555 01423"},
		{"plus one too short", "+1 212 555 014"},
		{"plus one too long", "+1 212 555 01423"},
		{"letters", "212-555-CALL"},
		{"letters only", "phone"},
		{"other symbol", "212#555#0142"},
		{"plus only", "+"},
		{"plus inside", "212+5550142"},
		{"separators only", "(-.)"},
		{"area code starts with 0", "(012) 555-0142"},
		{"area code starts with 1", "(112) 555-0142"},
		{"area code starts with 1 after leading 1", "1 112 555 0142"},
		{"area code starts with 0 after plus one", "+1 012 555 0142"},
		{"exchange starts with 0", "(212) 055-0142"},
		{"exchange starts with 1", "(212) 155-0142"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertRefused(t, tc.input, wantInvalid)
		})
	}
}

func TestInvalidErrorText(t *testing.T) {
	_, err := Normalize("123")
	if err == nil {
		t.Fatal(`Normalize("123") error = nil; want a refusal`)
	}
	const want = `normalize phone number "123": ` + wantInvalid
	if err.Error() != want {
		t.Errorf("Error() = %q; want %q", err.Error(), want)
	}
}
