package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

const (
	headerAuthorization = "Authorization"
	// bearerPrefix is the scheme and its separating space, matched exactly.
	bearerPrefix = "Bearer "
	msgBadKey    = "missing or wrong key"
)

// RequireKey passes a request to next only when it carries
// "Authorization: Bearer <key>" (AD-6); anything else gets 401 and next is
// not called. An empty key refuses every request, so a missing setting never
// opens the API. The key is never written into a reply or a log line (C-4).
func RequireKey(key string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key == "" || !keyMatches(want, r.Header.Get(headerAuthorization)) {
			writeJSON(w, http.StatusUnauthorized, errorReply{Error: msgBadKey})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// keyMatches compares digests rather than raw values: the constant-time compare
// returns early on a length mismatch, which would reveal the key's length.
func keyMatches(want [sha256.Size]byte, header string) bool {
	given, ok := strings.CutPrefix(header, bearerPrefix)
	if !ok {
		return false
	}
	got := sha256.Sum256([]byte(given))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
