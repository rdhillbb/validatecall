// Package httpapi serves the check API (R-12, TC-17) and the Telnyx event
// endpoint. They are separate handlers because the tunnel reaches only the
// events handler; the check API answers on the local listener (AD-6).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"checknumber/internal/check"
	"checknumber/internal/phone"
	"checknumber/internal/result"
)

const (
	routeCheck   = "POST /api/check"
	routeHistory = "GET /api/history"
	// historyParam is the query parameter of GET /api/history.
	historyParam = "number"

	msgBadJSON  = `request body must be JSON {"number": "..."}`
	msgInternal = "internal error"
	contentType = "Content-Type"
	contentJSON = "application/json"
)

// Checker runs checks and reads the history; *check.Service satisfies it.
type Checker interface {
	Check(ctx context.Context, input string) (check.Answer, error)
	History(ctx context.Context, input string) ([]check.Answer, error)
}

type checkRequest struct {
	Number string `json:"number"`
}

// checkReply is the TC-23 reply shape, shared by check and history.
type checkReply struct {
	Number      string      `json:"number"`
	ResultCode  result.Code `json:"result_code"`
	ResultLabel string      `json:"result_label"`
	// CalledAt preserves the zero time when no call was placed, in UTC RFC 3339.
	CalledAt string `json:"called_at"`
	Saved    bool   `json:"saved"`
	Reason   string `json:"reason"`
	Text     string `json:"text"`
}

type historyReply struct {
	Checks []checkReply `json:"checks"`
}

type errorReply struct {
	Error string `json:"error"`
}

// NewAPI returns the check API handler: POST /api/check waits for the result
// and returns it in the reply (AD-4); GET /api/history?number= returns every
// check of the number newest first (R-16).
func NewAPI(c Checker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(routeCheck, func(w http.ResponseWriter, r *http.Request) {
		var req checkRequest
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorReply{Error: msgBadJSON})
			return
		}
		// Refuse a second value or trailing garbage before any paid call.
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeJSON(w, http.StatusBadRequest, errorReply{Error: msgBadJSON})
			return
		}
		answer, err := c.Check(r.Context(), req.Number)
		if err != nil {
			writeError(w, "check", err)
			return
		}
		writeJSON(w, http.StatusOK, toReply(answer))
	})
	mux.HandleFunc(routeHistory, func(w http.ResponseWriter, r *http.Request) {
		answers, err := c.History(r.Context(), r.URL.Query().Get(historyParam))
		if err != nil {
			writeError(w, "history", err)
			return
		}
		reply := historyReply{Checks: make([]checkReply, 0, len(answers))}
		for _, a := range answers {
			reply.Checks = append(reply.Checks, toReply(a))
		}
		writeJSON(w, http.StatusOK, reply)
	})
	return mux
}

func toReply(a check.Answer) checkReply {
	reply := checkReply{
		Number:      a.Number,
		ResultCode:  a.Code,
		ResultLabel: a.Label,
		Saved:       a.Saved,
		Reason:      a.Reason,
		Text:        a.Text,
	}
	reply.CalledAt = a.CalledAt.UTC().Format(time.RFC3339)
	return reply
}

// writeError replies 400 with the person-facing message for a refused number
// (R-24) and 500 for anything else. Error strings may contain phone numbers.
func writeError(w http.ResponseWriter, op string, err error) {
	var invalid *phone.InvalidError
	if errors.As(err, &invalid) {
		writeJSON(w, http.StatusBadRequest, errorReply{Error: invalid.Message})
		return
	}
	log.Printf("httpapi: %s failed (%T)", op, err)
	writeJSON(w, http.StatusInternalServerError, errorReply{Error: msgInternal})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set(contentType, contentJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("httpapi: write %d reply failed (%T)", status, err)
	}
}
