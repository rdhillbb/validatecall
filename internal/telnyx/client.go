// Package telnyx sends the five Telnyx commands the product uses (TS-2): dial,
// start recording, hang up, download a recording, and delete a recording with
// its transcription (R-37, TC-58).
// It uses plain net/http instead of the Telnyx Go library (deviation from TC-1,
// recorded in TO-0004): only a few endpoints are needed and go.mod stays as is.
// Parameter names follow the Telnyx API reference read 2026-11-13.
package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ProductionBaseURL is the Telnyx v2 API root.
const ProductionBaseURL = "https://api.telnyx.com/v2"

// Dial settings.
const (
	// TC-2: about 5 rings at 6 seconds each; retry off so ringing cannot run longer.
	ringTimeoutSecs = 30
	retryOnTimeout  = false
	// TS-3: Premium is the only detection mode that reports fax tones and silence.
	detectionMode = "premium"
	// TC-3: set explicitly because the Telnyx docs give two different defaults
	// (3.5 s and 30 s); 20 s leaves room for a greeting or repeated fax tones.
	detectionTimeMillis = 20000
)

// Recording settings.
const (
	recordingFormat   = "mp3"
	recordingChannels = "single"
	// C-1 and TC-4: a recording is never longer than 10 seconds.
	recordingMaxSecs = 10
	// TC-9: English, cheapest Telnyx engine that supports it.
	transcriptionEngine   = "deepgram/nova-3"
	transcriptionLanguage = "en-US"
)

const (
	callsPath       = "/calls"
	actionPathFmt   = "/calls/%s/actions/%s"
	actionRecord    = "record_start"
	actionHangup    = "hangup"
	contentTypeJSON = "application/json"
	// errorBodyLimit caps how much of an error reply is read into memory.
	errorBodyLimit = 64 << 10
	// TC-35: a 10-second mp3 (C-1) is far below 5 MiB; a larger reply is not
	// the recording that was asked for.
	maxRecordingBytes = 5 << 20
	redacted          = "[redacted]"
)

// Recordings API paths and list filters (TC-58), read 2026-11-21.
const (
	recordingsPath     = "/recordings"
	transcriptionsPath = "/recording_transcriptions"
	filterCallLegID    = "filter[call_leg_id]"
	filterRecordingID  = "filter[recording_id]"
	pageNumberParam    = "page[number]"
	pageAfterParam     = "page[after]"
)

// Client sends Call Control commands to one Telnyx API base URL.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// NewClient returns a client for baseURL (ProductionBaseURL in production).
// The key comes from the caller and is only ever sent in the Authorization
// header of a command to baseURL, never to a recording link (C-4, D-2). A nil
// hc uses http.DefaultClient.
func NewClient(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), apiKey: apiKey, hc: hc}
}

// APIError is a non-2xx reply from Telnyx or from a recording link.
// Code and Detail come from the first entry of the reply's "errors" list.
type APIError struct {
	Status int
	Code   string
	Detail string
	op     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telnyx %s: status %d, code %q: %s", e.op, e.Status, e.Code, e.Detail)
}

// DialRequest holds the per-call values for Dial. From must be a number on
// the Telnyx account (C-7); WebhookURL is the tunnel's current address (TC-13).
type DialRequest struct {
	To           string
	From         string
	ConnectionID string
	WebhookURL   string
	// TimeLimitSecs is the call's maximum length (D-6, TC-37). 0 sends no
	// limit, and Telnyx then applies its own default of 4 hours.
	TimeLimitSecs int
}

type dialBody struct {
	To              string          `json:"to"`
	From            string          `json:"from"`
	ConnectionID    string          `json:"connection_id"`
	WebhookURL      string          `json:"webhook_url"`
	TimeoutSecs     int             `json:"timeout_secs"`
	RetryOnTimeout  bool            `json:"retry_on_timeout"`
	TimeLimitSecs   int             `json:"time_limit_secs,omitempty"`
	Detection       string          `json:"answering_machine_detection"`
	DetectionConfig detectionConfig `json:"answering_machine_detection_config"`
}

type detectionConfig struct {
	TotalAnalysisTimeMillis int `json:"total_analysis_time_millis"`
}

type dialReply struct {
	Data struct {
		CallControlID string `json:"call_control_id"`
	} `json:"data"`
}

type recordBody struct {
	Format                string `json:"format"`
	Channels              string `json:"channels"`
	MaxLength             int    `json:"max_length"`
	Transcription         bool   `json:"transcription"`
	TranscriptionEngine   string `json:"transcription_engine"`
	TranscriptionLanguage string `json:"transcription_language"`
}

type listReply struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	Meta struct {
		TotalPages int    `json:"total_pages"`
		Next       string `json:"next"`
		Cursors    struct {
			After string `json:"after"`
		} `json:"cursors"`
	} `json:"meta"`
}

type errorReply struct {
	Errors []struct {
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

// Dial places an outbound call and returns its call control id.
func (c *Client) Dial(ctx context.Context, req DialRequest) (string, error) {
	body := dialBody{
		To:              req.To,
		From:            req.From,
		ConnectionID:    req.ConnectionID,
		WebhookURL:      req.WebhookURL,
		TimeoutSecs:     ringTimeoutSecs,
		RetryOnTimeout:  retryOnTimeout,
		TimeLimitSecs:   req.TimeLimitSecs,
		Detection:       detectionMode,
		DetectionConfig: detectionConfig{TotalAnalysisTimeMillis: detectionTimeMillis},
	}
	op := fmt.Sprintf("dial %s", req.To)
	var reply dialReply
	if err := c.send(ctx, op, http.MethodPost, callsPath, body, &reply); err != nil {
		return "", err
	}
	if reply.Data.CallControlID == "" {
		return "", c.errorf("telnyx %s: reply has no data.call_control_id", op)
	}
	return reply.Data.CallControlID, nil
}

// StartRecording records at most 10 seconds of the call, with an English
// transcription (R-13, C-1, TS-4).
func (c *Client) StartRecording(ctx context.Context, callControlID string) error {
	body := recordBody{
		Format:                recordingFormat,
		Channels:              recordingChannels,
		MaxLength:             recordingMaxSecs,
		Transcription:         true,
		TranscriptionEngine:   transcriptionEngine,
		TranscriptionLanguage: transcriptionLanguage,
	}
	return c.action(ctx, callControlID, actionRecord, body)
}

// Hangup ends the call.
func (c *Client) Hangup(ctx context.Context, callControlID string) error {
	return c.action(ctx, callControlID, actionHangup, struct{}{})
}

// RecordingRef names what of one call is deleted at Telnyx. The
// transcription-saved event carries RecordingID and TranscriptionID; the
// recording-saved event documents neither, so CallLegID lets the client find
// the recording by listing (TC-58).
type RecordingRef struct {
	RecordingID, TranscriptionID, CallLegID string
}

var errNoRecordingID = errors.New("no recording, transcription or call leg id is known")

// DeleteRecording deletes the call's recordings and their transcriptions at
// Telnyx (R-37, C-1). Recording ids are ref.RecordingID, else those listed by
// ref.CallLegID; transcription ids are ref.TranscriptionID, else those listed
// by each recording id. Every delete is tried and the errors are joined. A 404
// means already deleted, and an empty list is no error.
func (c *Client) DeleteRecording(ctx context.Context, ref RecordingRef) error {
	if ref == (RecordingRef{}) {
		return fmt.Errorf("telnyx delete recording: %w", errNoRecordingID)
	}
	var errs []error
	var recordingIDs []string
	switch {
	case ref.RecordingID != "":
		recordingIDs = []string{ref.RecordingID}
	case ref.CallLegID != "":
		var err error
		recordingIDs, err = c.listIDs(ctx, "list recordings of call leg "+ref.CallLegID, recordingsPath, filterCallLegID, ref.CallLegID)
		errs = append(errs, err)
	}
	var transcriptionIDs []string
	if ref.TranscriptionID != "" {
		transcriptionIDs = []string{ref.TranscriptionID}
	} else {
		for _, id := range recordingIDs {
			ids, err := c.listIDs(ctx, "list transcriptions of recording "+id, transcriptionsPath, filterRecordingID, id)
			transcriptionIDs = append(transcriptionIDs, ids...)
			errs = append(errs, err)
		}
	}
	// Transcriptions were listed by their recording's id, so the recordings
	// go last.
	for _, id := range transcriptionIDs {
		errs = append(errs, c.deleteByID(ctx, "delete transcription "+id, transcriptionsPath, id))
	}
	for _, id := range recordingIDs {
		errs = append(errs, c.deleteByID(ctx, "delete recording "+id, recordingsPath, id))
	}
	return errors.Join(errs...)
}

// listIDs returns any IDs found before an error so cleanup can still proceed.
func (c *Client) listIDs(ctx context.Context, op, path, filter, id string) ([]string, error) {
	query := url.Values{filter: {id}}
	var ids []string
	var errs []error
	seenCursors := make(map[string]bool)
	for page := 1; ; page++ {
		var reply listReply
		if err := c.send(ctx, op, http.MethodGet, path+"?"+query.Encode(), nil, &reply); err != nil {
			return ids, errors.Join(append(errs, err)...)
		}
		if reply.Data == nil {
			errs = append(errs, c.errorf("telnyx %s: missing list data on page %d", op, page))
		}
		for _, item := range reply.Data {
			if item.ID == "" {
				errs = append(errs, c.errorf("telnyx %s: missing id in list on page %d", op, page))
				continue
			}
			ids = append(ids, item.ID)
		}
		// Transcriptions can use cursors. Keep the original path and filter
		// when constructing the next request.
		switch after := reply.Meta.Cursors.After; {
		case after != "":
			if seenCursors[after] {
				err := c.errorf("telnyx %s: repeated list cursor on page %d", op, page)
				return ids, errors.Join(append(errs, err)...)
			}
			seenCursors[after] = true
			query.Del(pageNumberParam)
			query.Set(pageAfterParam, after)
		case reply.Meta.Next != "":
			err := c.errorf("telnyx %s: missing next list cursor on page %d", op, page)
			return ids, errors.Join(append(errs, err)...)
		case page < reply.Meta.TotalPages:
			query.Del(pageAfterParam)
			query.Set(pageNumberParam, strconv.Itoa(page+1))
		default:
			return ids, errors.Join(errs...)
		}
	}
}

// deleteByID deletes path/id; a 404 means it is already gone.
func (c *Client) deleteByID(ctx context.Context, op, path, id string) error {
	// PathEscape leaves standalone dot segments unchanged.
	if id == "." || id == ".." {
		return c.errorf("telnyx %s: invalid resource id", op)
	}
	err := c.send(ctx, op, http.MethodDelete, path+"/"+url.PathEscape(id), nil, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Download copies the recording at link into dst unchanged. The link is
// pre-signed: its query is the credential, and the store refuses a request
// that also carries the Telnyx key, so no Authorization header is sent (D-2).
// Every error shows the link without that query (TC-35). dst is written only
// after the whole recording has passed the size checks.
func (c *Client) Download(ctx context.Context, link string, dst io.Writer) (err error) {
	shown, query := splitLink(link)
	// Any failure source can echo credentials, including a redirected request,
	// a response reader or the destination writer (TC-35).
	defer func() {
		if err == nil {
			return
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			apiErr.Code = hideQuery(apiErr.Code, query)
			apiErr.Detail = hideQuery(apiErr.Detail, query)
		}
		if text := hideQuery(err.Error(), query); text != err.Error() {
			err = &redactedError{cause: err, text: text}
		}
	}()
	op := "download recording " + shown
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return c.errorf("telnyx %s: build request: %w", op, withoutLink(err))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.errorf("telnyx %s: %w", op, withoutLink(err))
	}
	defer resp.Body.Close()
	if apiErr := c.checkStatus(op, resp); apiErr != nil {
		return apiErr
	}
	// One byte past the limit is enough to know the reply is too large.
	recording, err := io.ReadAll(io.LimitReader(resp.Body, maxRecordingBytes+1))
	if err != nil {
		return c.errorf("telnyx %s: read body: %w", op, err)
	}
	if len(recording) == 0 {
		return c.errorf("telnyx %s: the recording is empty", op)
	}
	if len(recording) > maxRecordingBytes {
		return c.errorf("telnyx %s: the recording is larger than %d bytes", op, maxRecordingBytes)
	}
	if _, err := dst.Write(recording); err != nil {
		return c.errorf("telnyx %s: write recording: %w", op, err)
	}
	return nil
}

// splitLink returns the link as an error may show it (scheme://host/path) and
// its query, which is never shown (TC-35).
func splitLink(link string) (shown, query string) {
	u, err := url.Parse(link)
	if err != nil {
		// Not a URL: the text before the first "?" is all that can be shown.
		shown, query, _ = strings.Cut(link, "?")
		return shown, query
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawPath: u.RawPath}).String(), u.RawQuery
}

// withoutLink drops the *url.Error that net/http and net/url wrap around a
// failure: its text repeats the whole link, query included. The cause stays
// reachable for errors.Is.
func withoutLink(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func hideQuery(text, query string) string {
	if query == "" {
		return text
	}
	text = strings.ReplaceAll(text, query, redacted)
	text = strings.ReplaceAll(text, url.QueryEscape(query), redacted)
	// Stores may echo one parameter or decode its percent escapes rather than
	// repeating the complete query. Each nonempty value is sensitive too.
	for _, parameter := range strings.Split(query, "&") {
		_, value, found := strings.Cut(parameter, "=")
		if !found {
			value = parameter
		}
		if value == "" {
			continue
		}
		text = strings.ReplaceAll(text, value, redacted)
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != "" {
			text = strings.ReplaceAll(text, url.QueryEscape(decoded), redacted)
			text = strings.ReplaceAll(text, decoded, redacted)
		}
	}
	return text
}

func (c *Client) action(ctx context.Context, callControlID, action string, body any) error {
	op := fmt.Sprintf("%s call %s", action, callControlID)
	if callControlID == "" {
		return c.errorf("telnyx %s: empty call control id", op)
	}
	path := fmt.Sprintf(actionPathFmt, url.PathEscape(callControlID), action)
	return c.send(ctx, op, http.MethodPost, path, body, nil)
}

// send sends a method request to path, with body as JSON when body is not nil,
// and, when out is not nil, decodes the reply into it.
func (c *Client) send(ctx context.Context, op, method, path string, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return c.errorf("telnyx %s: encode body: %w", op, err)
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, payload)
	if err != nil {
		return c.errorf("telnyx %s: build request: %w", op, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", contentTypeJSON)
	}
	req.Header.Set("Accept", contentTypeJSON)
	c.authorize(req)
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.errorf("telnyx %s: %w", op, err)
	}
	defer resp.Body.Close()
	if apiErr := c.checkStatus(op, resp); apiErr != nil {
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return c.errorf("telnyx %s: decode reply: %w", op, err)
	}
	return nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
}

// checkStatus turns a non-2xx reply into an *APIError and returns nil for a
// 2xx reply. The key is removed from the reply text so it can never appear in
// an error message (C-4).
func (c *Client) checkStatus(op string, resp *http.Response) *APIError {
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	apiErr := &APIError{Status: resp.StatusCode, op: c.redact(op)}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	var reply errorReply
	switch {
	case readErr != nil:
		apiErr.Detail = fmt.Sprintf("read error reply: %v", readErr)
	case json.Unmarshal(raw, &reply) == nil && len(reply.Errors) > 0:
		first := reply.Errors[0]
		apiErr.Code = first.Code
		apiErr.Detail = first.Detail
		if apiErr.Detail == "" {
			apiErr.Detail = first.Title
		}
	default:
		apiErr.Detail = strings.TrimSpace(string(raw))
	}
	apiErr.Code = c.redact(apiErr.Code)
	apiErr.Detail = c.redact(apiErr.Detail)
	return apiErr
}

func (c *Client) redact(s string) string {
	if c.apiKey == "" {
		return s
	}
	return strings.ReplaceAll(s, c.apiKey, redacted)
}

// Transport and writer errors may echo credentials too. Preserve their identity
// for errors.Is/errors.As while keeping the public error text safe (C-4).
type redactedError struct {
	cause error
	text  string
}

func (e *redactedError) Error() string { return e.text }
func (e *redactedError) Unwrap() error { return e.cause }

func (c *Client) errorf(format string, args ...any) error {
	err := fmt.Errorf(format, args...)
	text := c.redact(err.Error())
	if text == err.Error() {
		return err
	}
	return &redactedError{cause: err, text: text}
}
