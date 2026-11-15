package httpapi

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Test key values only; no real key is used (C-4).
const (
	authTestKey   = "test-key-0011-alpha"
	authOtherKey  = "test-key-0011-bravo"
	authBody      = `{"number":"+15552230101"}`
	stubReplyBody = "reached next"
)

var wantRefusal = `{"error":"` + msgBadKey + `"}` + "\n"

// stubNext records every request it receives and answers 200.
type stubNext struct {
	got []*http.Request
}

func (s *stubNext) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.got = append(s.got, r)
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, stubReplyBody)
}

// keyRequest is a valid check request; only its Authorization header varies.
func keyRequest(authorization string, set bool) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/check", strings.NewReader(authBody))
	r.Header.Set(contentType, contentJSON)
	if set {
		r.Header.Set(headerAuthorization, authorization)
	}
	return r
}

func assertRefused(t *testing.T, name string, rec *httptest.ResponseRecorder, next *stubNext) {
	t.Helper()
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s: status = %d, want 401", name, rec.Code)
	}
	if got := rec.Header().Get(contentType); got != contentJSON {
		t.Fatalf("%s: Content-Type = %q, want %q", name, got, contentJSON)
	}
	if got := rec.Body.String(); got != wantRefusal {
		t.Fatalf("%s: body = %q, want %q", name, got, wantRefusal)
	}
	if len(next.got) != 0 {
		t.Fatalf("%s: next called %d times, want 0", name, len(next.got))
	}
}

func TestRequireKeyPasses(t *testing.T) {
	next := &stubNext{}
	h := RequireKey(authTestKey, next)
	req := keyRequest(bearerPrefix+authTestKey, true)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != stubReplyBody {
		t.Fatalf("reply = %d %q, want 200 %q from next", rec.Code, rec.Body.String(), stubReplyBody)
	}
	if len(next.got) != 1 {
		t.Fatalf("next called %d times, want 1", len(next.got))
	}
	got := next.got[0]
	if got != req {
		t.Fatal("next received a different request value, want the request unchanged")
	}
	if got.Method != http.MethodPost || got.URL.Path != "/api/check" {
		t.Fatalf("next got %s %s, want POST /api/check", got.Method, got.URL.Path)
	}
	if h := got.Header.Get(headerAuthorization); h != bearerPrefix+authTestKey {
		t.Fatalf("Authorization header at next = %q, want it unchanged", h)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body at next: %v", err)
	}
	if string(body) != authBody {
		t.Fatalf("body at next = %q, want %q", body, authBody)
	}
}

func TestRequireKeyRefuses(t *testing.T) {
	cases := []struct {
		name   string
		header string
		set    bool
	}{
		{name: "missing header", set: false},
		{name: "empty header", header: "", set: true},
		{name: "basic scheme with right key", header: "Basic " + authTestKey, set: true},
		{name: "token scheme with right key", header: "Token " + authTestKey, set: true},
		{name: "bare right key", header: authTestKey, set: true},
		{name: "wrong key", header: bearerPrefix + authOtherKey, set: true},
		{name: "empty bearer value", header: bearerPrefix, set: true},
		{name: "bearer without space", header: "Bearer" + authTestKey, set: true},
		{name: "prefix of right key", header: bearerPrefix + authTestKey[:len(authTestKey)-1], set: true},
		{name: "right key plus extra", header: bearerPrefix + authTestKey + "x", set: true},
		{name: "right key with trailing space", header: bearerPrefix + authTestKey + " ", set: true},
		{name: "double space before key", header: "Bearer  " + authTestKey, set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := &stubNext{}
			rec := httptest.NewRecorder()
			RequireKey(authTestKey, next).ServeHTTP(rec, keyRequest(tc.header, tc.set))
			assertRefused(t, tc.name, rec, next)
		})
	}
}

func TestRequireKeyEmptyRefusesAll(t *testing.T) {
	cases := []struct {
		name   string
		header string
		set    bool
	}{
		{name: "missing header", set: false},
		{name: "empty bearer value", header: bearerPrefix, set: true},
		{name: "bearer only", header: "Bearer", set: true},
		{name: "some key", header: bearerPrefix + authTestKey, set: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := &stubNext{}
			rec := httptest.NewRecorder()
			RequireKey("", next).ServeHTTP(rec, keyRequest(tc.header, tc.set))
			assertRefused(t, tc.name, rec, next)
		})
	}
}

func TestRefusalHidesKey(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	// Each request carries the configured key in some form, so a reply or log
	// line that echoed the header or the key would contain it.
	headers := []string{
		"Basic " + authTestKey,
		bearerPrefix + authTestKey + "x",
		authTestKey,
	}
	for _, header := range headers {
		logs.Reset()
		next := &stubNext{}
		rec := httptest.NewRecorder()
		RequireKey(authTestKey, next).ServeHTTP(rec, keyRequest(header, true))
		assertRefused(t, header, rec, next)
		if strings.Contains(rec.Body.String(), authTestKey) {
			t.Fatalf("reply body %q contains the key", rec.Body.String())
		}
		for name, values := range rec.Header() {
			for _, v := range values {
				if strings.Contains(v, authTestKey) {
					t.Fatalf("reply header %s = %q contains the key", name, v)
				}
			}
		}
		if strings.Contains(logs.String(), authTestKey) {
			t.Fatalf("log contains the key: %q", logs.String())
		}
	}
}
