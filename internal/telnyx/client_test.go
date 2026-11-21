package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeKey is not a Telnyx key; tests never use a real key or call Telnyx (C-5).
const fakeKey = "fake-test-key-7f3a"

const testCallID = "v3:fake-call-id"

// TC-35, written out here so a changed maxRecordingBytes fails the tests.
const contractMaxRecording = 5 << 20

// fakeLinkQuery stands for the signature of a pre-signed recording link; it
// is not a real one (C-5).
const fakeLinkQuery = "X-Amz-Expires=600&X-Amz-Signature=fake-signature-9c1e"

// captured is one request the fake Telnyx received.
type captured struct {
	method string
	path   string
	query  string
	auth   string
	body   []byte
}

// fakeTelnyx is an httptest.Server that records every request and answers
// with reply.
type fakeTelnyx struct {
	*httptest.Server
	mu       sync.Mutex
	requests []captured
}

func newFake(t *testing.T, reply http.HandlerFunc) *fakeTelnyx {
	t.Helper()
	f := &fakeTelnyx{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake telnyx: read body: %v", err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, captured{
			method: r.Method,
			path:   r.URL.EscapedPath(),
			query:  r.URL.RawQuery,
			auth:   r.Header.Get("Authorization"),
			body:   body,
		})
		f.mu.Unlock()
		reply(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTelnyx) only(t *testing.T) captured {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 {
		t.Fatalf("fake telnyx got %d requests; want 1", len(f.requests))
	}
	return f.requests[0]
}

func replyJSON(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// assertJSONBody compares the captured body with want field by field.
// want is written out literally so a changed setting fails the test.
func assertJSONBody(t *testing.T, body []byte, want map[string]any) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("request body is not JSON: %v; body %s", err, body)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("request body = %s\nwant %v", body, want)
	}
}

func TestDialRequestBody(t *testing.T) {
	tests := []struct {
		name          string
		timeLimitSecs int
		// extra is what the body holds on top of the fixed fields.
		extra map[string]any
	}{
		{"no time limit", 0, nil},
		{"time limit set", 150, map[string]any{"time_limit_secs": float64(150)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, replyJSON(http.StatusOK,
				`{"data":{"call_control_id":"v3:dialled-id","call_leg_id":"leg-1"}}`))
			c := NewClient(fake.URL, fakeKey, fake.Client())

			id, err := c.Dial(context.Background(), DialRequest{
				To:            "+12125550142",
				From:          "+13125550100",
				ConnectionID:  "conn-123",
				WebhookURL:    "https://example-tunnel.trycloudflare.com/webhook",
				TimeLimitSecs: tc.timeLimitSecs,
			})
			if err != nil {
				t.Fatalf("Dial error = %v; want nil", err)
			}
			if id != "v3:dialled-id" {
				t.Errorf("Dial id = %q; want %q", id, "v3:dialled-id")
			}

			got := fake.only(t)
			if got.method != http.MethodPost || got.path != "/calls" {
				t.Errorf("request = %s %s; want POST /calls", got.method, got.path)
			}
			// JSON numbers decode as float64. The compare is exact, so a
			// time_limit_secs sent without a limit fails it.
			want := map[string]any{
				"to":                          "+12125550142",
				"from":                        "+13125550100",
				"connection_id":               "conn-123",
				"webhook_url":                 "https://example-tunnel.trycloudflare.com/webhook",
				"timeout_secs":                float64(30),
				"retry_on_timeout":            false,
				"answering_machine_detection": "premium",
				"answering_machine_detection_config": map[string]any{
					"total_analysis_time_millis": float64(20000),
				},
			}
			maps.Copy(want, tc.extra)
			assertJSONBody(t, got.body, want)
		})
	}
}

func TestDialReplyWithoutCallID(t *testing.T) {
	fake := newFake(t, replyJSON(http.StatusOK, `{"data":{}}`))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	id, err := c.Dial(context.Background(), DialRequest{To: "+12125550142"})
	if err == nil || id != "" {
		t.Fatalf("Dial = %q, %v; want empty id and an error", id, err)
	}
	if !strings.Contains(err.Error(), "call_control_id") {
		t.Errorf("Dial error = %q; want it to name the missing call_control_id", err)
	}
}

func TestStartRecordingBody(t *testing.T) {
	fake := newFake(t, replyJSON(http.StatusOK, `{"data":{"result":"ok"}}`))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	if err := c.StartRecording(context.Background(), testCallID); err != nil {
		t.Fatalf("StartRecording error = %v; want nil", err)
	}

	got := fake.only(t)
	wantPath := "/calls/" + testCallID + "/actions/record_start"
	if got.method != http.MethodPost || got.path != wantPath {
		t.Errorf("request = %s %s; want POST %s", got.method, got.path, wantPath)
	}
	assertJSONBody(t, got.body, map[string]any{
		"format":                 "mp3",
		"channels":               "single",
		"max_length":             float64(10),
		"transcription":          true,
		"transcription_engine":   "deepgram/nova-3",
		"transcription_language": "en-US",
	})
}

func TestHangup(t *testing.T) {
	fake := newFake(t, replyJSON(http.StatusOK, `{"data":{"result":"ok"}}`))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	if err := c.Hangup(context.Background(), testCallID); err != nil {
		t.Fatalf("Hangup error = %v; want nil", err)
	}

	got := fake.only(t)
	wantPath := "/calls/" + testCallID + "/actions/hangup"
	if got.method != http.MethodPost || got.path != wantPath {
		t.Errorf("request = %s %s; want POST %s", got.method, got.path, wantPath)
	}
}

func TestCommandRefusesEmptyCallID(t *testing.T) {
	fake := newFake(t, replyJSON(http.StatusOK, `{}`))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	if err := c.Hangup(context.Background(), ""); err == nil {
		t.Error("Hangup(\"\") error = nil; want an error")
	}
	if err := c.StartRecording(context.Background(), ""); err == nil {
		t.Error("StartRecording(\"\") error = nil; want an error")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("fake telnyx got %d requests for an empty call id; want 0", len(fake.requests))
	}
}

func TestDownload(t *testing.T) {
	// Every byte value, so any text conversion or truncation changes the result.
	recording := make([]byte, 3*256)
	for i := range recording {
		recording[i] = byte(i)
	}
	fake := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(recording)
	})
	c := NewClient(fake.URL, fakeKey, fake.Client())

	var dst bytes.Buffer
	if err := c.Download(context.Background(), fake.URL+"/recordings/rec-1.mp3", &dst); err != nil {
		t.Fatalf("Download error = %v; want nil", err)
	}
	if !bytes.Equal(dst.Bytes(), recording) {
		t.Errorf("Download copied %d bytes that differ from the %d recording bytes", dst.Len(), len(recording))
	}
	got := fake.only(t)
	if got.method != http.MethodGet || got.path != "/recordings/rec-1.mp3" {
		t.Errorf("request = %s %s; want GET /recordings/rec-1.mp3", got.method, got.path)
	}
}

func TestDownloadExpiredLink(t *testing.T) {
	fake := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "Request has expired")
	})
	c := NewClient(fake.URL, fakeKey, fake.Client())

	var dst bytes.Buffer
	err := c.Download(context.Background(), fake.URL+"/rec.mp3", &dst)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Download error = %v (%T); want *APIError", err, err)
	}
	if apiErr.Status != http.StatusForbidden || apiErr.Detail != "Request has expired" {
		t.Errorf("APIError = %+v; want status 403, detail %q", apiErr, "Request has expired")
	}
	if dst.Len() != 0 {
		t.Errorf("Download wrote %d bytes on an error reply; want 0", dst.Len())
	}
}

// D-2: a recording link is pre-signed and its store refuses a request that
// also carries the Telnyx key.
func TestDownloadSendsNoKey(t *testing.T) {
	recording := []byte("fake mp3 bytes \x00\xff")
	for _, where := range []string{"link on another host", "link on the API host"} {
		t.Run(where, func(t *testing.T) {
			api := newFake(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = w.Write(recording)
					return
				}
				replyJSON(http.StatusOK, `{"data":{"result":"ok"}}`)(w, r)
			})
			store := api
			if where == "link on another host" {
				store = newFake(t, func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write(recording)
				})
			}
			c := NewClient(api.URL, fakeKey, api.Client())
			ctx := context.Background()

			// Control: this client does send its key with a command, so the
			// empty header below is the download's doing.
			if err := c.Hangup(ctx, testCallID); err != nil {
				t.Fatalf("Hangup error = %v", err)
			}
			var dst bytes.Buffer
			if err := c.Download(ctx, store.URL+"/recordings/rec-1.mp3?"+fakeLinkQuery, &dst); err != nil {
				t.Fatalf("Download error = %v; want nil", err)
			}

			api.mu.Lock()
			hangup := api.requests[0]
			api.mu.Unlock()
			if hangup.method != http.MethodPost || hangup.auth != "Bearer "+fakeKey {
				t.Fatalf("control request = %s with Authorization %q; want the hangup POST with the bearer key", hangup.method, hangup.auth)
			}
			store.mu.Lock()
			got := store.requests[len(store.requests)-1]
			store.mu.Unlock()
			if got.method != http.MethodGet || got.path != "/recordings/rec-1.mp3" || got.query != fakeLinkQuery {
				t.Errorf("download request = %s %s?%s; want GET /recordings/rec-1.mp3?%s", got.method, got.path, got.query, fakeLinkQuery)
			}
			if got.auth != "" {
				t.Errorf("download sent Authorization %q; want no Authorization header", got.auth)
			}
			if !bytes.Equal(dst.Bytes(), recording) {
				t.Errorf("Download wrote %q; want %q", dst.Bytes(), recording)
			}
		})
	}
}

func TestDownloadErrors(t *testing.T) {
	const linkPath = "/recordings/rec-1.mp3"
	ctx := context.Background()

	// Control: exactly 5 MiB is accepted, so the refusal below comes from the
	// one byte more.
	atLimit := newFake(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, contractMaxRecording))
	})
	var whole bytes.Buffer
	if err := NewClient(atLimit.URL, fakeKey, atLimit.Client()).Download(ctx, atLimit.URL+linkPath+"?"+fakeLinkQuery, &whole); err != nil || whole.Len() != contractMaxRecording {
		t.Fatalf("control: Download of exactly 5 MiB = %v, %d bytes written; want nil and %d", err, whole.Len(), contractMaxRecording)
	}

	failingClient := func(cause error) *http.Client {
		return &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, cause })}
	}
	tests := []struct {
		name string
		// reply is the store's answer. When it is nil no store runs and the
		// request goes to hc at base.
		reply http.HandlerFunc
		hc    *http.Client
		base  string
		path  string
		// wantStatus is the *APIError status; 0 when the error is not a reply.
		wantStatus int
		wantText   string
	}{
		{
			name: "store refuses an expired link",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, "Request has expired")
			},
			path: linkPath, wantStatus: http.StatusForbidden, wantText: "Request has expired",
		},
		{
			name: "store reply repeats the query and the key",
			reply: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, "refused "+r.URL.RawQuery+" from "+fakeKey)
			},
			path: linkPath, wantStatus: http.StatusBadRequest, wantText: "refused [redacted] from [redacted]",
		},
		{
			name:  "empty recording",
			reply: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
			path:  linkPath, wantText: "the recording is empty",
		},
		{
			name: "one byte more than 5 MiB",
			reply: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(make([]byte, contractMaxRecording+1))
			},
			path: linkPath, wantText: "larger than 5242880 bytes",
		},
		{
			// net/http wraps this cause in a *url.Error that names the whole link.
			name: "connection fails",
			hc:   failingClient(errors.New("connection reset")),
			base: "https://recording.invalid", path: linkPath, wantText: "connection reset",
		},
		{
			name: "link is not a URL",
			hc:   failingClient(errors.New("the request must not be sent")),
			base: "https://recording.invalid", path: "/%zz", wantText: "build request",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base, hc := tc.base, tc.hc
			var store *fakeTelnyx
			if tc.reply != nil {
				store = newFake(t, tc.reply)
				base, hc = store.URL, store.Client()
			}
			shown := base + tc.path
			c := NewClient("https://api.invalid/v2", fakeKey, hc)

			var dst bytes.Buffer
			err := c.Download(ctx, shown+"?"+fakeLinkQuery, &dst)
			if err == nil {
				t.Fatalf("Download error = nil; want an error")
			}
			text := err.Error()
			if !strings.Contains(text, tc.wantText) {
				t.Errorf("error text %q; want it to contain %q", text, tc.wantText)
			}
			if !strings.Contains(text, "download recording "+shown) {
				t.Errorf("error text %q; want it to name the link without its query, %q", text, shown)
			}
			if strings.Contains(text, fakeLinkQuery) || strings.Contains(text, "fake-signature") {
				t.Errorf("error text contains the link's query: %q", text)
			}
			if strings.Contains(text, fakeKey) {
				t.Errorf("error text contains the API key: %q", text)
			}
			var apiErr *APIError
			if tc.wantStatus != 0 && (!errors.As(err, &apiErr) || apiErr.Status != tc.wantStatus) {
				t.Errorf("error = %v (%T); want *APIError with status %d", err, err, tc.wantStatus)
			}
			if dst.Len() != 0 {
				t.Errorf("Download wrote %d bytes on an error; want 0", dst.Len())
			}
			if store != nil {
				if got := store.only(t); got.auth != "" || got.query != fakeLinkQuery {
					t.Errorf("store got Authorization %q and query %q; want no Authorization and the link's query", got.auth, got.query)
				}
			}
		})
	}
}

type recordingReadFailure struct{ err error }

func (r recordingReadFailure) Read([]byte) (int, error) { return 0, r.err }

func TestDownloadQueryRedaction(t *testing.T) {
	const query = "Signature=private%2Fsignature%2Bvalue&Expires=600"
	const signature = "private/signature+value"
	const link = "https://recording.invalid/rec.mp3?" + query
	for _, echo := range []struct{ name, text string }{
		{"whole query", query},
		{"encoded query", url.QueryEscape(query)},
		{"one encoded value", url.QueryEscape(signature)},
		{"one decoded value", signature},
	} {
		for _, source := range []string{"transport", "body reader", "error reply reader", "writer", "store reply"} {
			t.Run(echo.name+"/"+source, func(t *testing.T) {
				cause := errors.New("failure echoed " + echo.text + " and " + fakeKey)
				requests := 0
				hc := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					requests++
					if r.URL.RawQuery != query || r.Header.Get("Authorization") != "" {
						t.Fatalf("request lost query or sent authorization: %v", r)
					}
					if source == "transport" {
						return nil, cause
					}
					resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("recording bytes"))}
					switch source {
					case "body reader", "error reply reader":
						resp.Body = io.NopCloser(recordingReadFailure{err: cause})
						if source == "error reply reader" {
							resp.StatusCode = http.StatusForbidden
						}
					case "store reply":
						resp.StatusCode = http.StatusForbidden
						body, err := json.Marshal(map[string]any{"errors": []map[string]string{{"code": echo.text, "detail": cause.Error()}}})
						if err != nil {
							t.Fatal(err)
						}
						resp.Body = io.NopCloser(bytes.NewReader(body))
					}
					return resp, nil
				})}
				var dst bytes.Buffer
				var writer io.Writer = &dst
				if source == "writer" {
					writer = failingWriter{err: cause}
				}
				err := NewClient("https://api.invalid/v2", fakeKey, hc).Download(context.Background(), link, writer)
				if requests != 1 || err == nil {
					t.Fatalf("requests = %d, error = %v; want one request and failure", requests, err)
				}
				if source == "transport" || source == "body reader" || source == "writer" {
					if !errors.Is(err, cause) {
						t.Errorf("original cause lost: %v", err)
					}
				}
				texts := []string{err.Error()}
				if source == "store reply" || source == "error reply reader" {
					var apiErr *APIError
					if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
						t.Fatalf("error = %v; want APIError status 403", err)
					}
					texts = append(texts, apiErr.Code, apiErr.Detail)
				}
				for _, text := range texts {
					for _, secret := range []string{query, echo.text, signature, url.QueryEscape(signature), fakeKey} {
						if strings.Contains(text, secret) {
							t.Errorf("error exposes credential %q: %q", secret, text)
						}
					}
				}
				if !strings.Contains(err.Error(), "download recording https://recording.invalid/rec.mp3") || !strings.Contains(err.Error(), "[redacted]") {
					t.Errorf("missing safe operation context: %v", err)
				}
				if dst.Len() != 0 {
					t.Errorf("failed download wrote %d bytes", dst.Len())
				}
			})
		}
	}
}

func TestAuthHeaderAndErrorHidesKey(t *testing.T) {
	const wantAuth = "Bearer " + fakeKey

	t.Run("every command carries the bearer key", func(t *testing.T) {
		fake := newFake(t, replyJSON(http.StatusOK, `{"data":{"call_control_id":"v3:x"}}`))
		c := NewClient(fake.URL, fakeKey, fake.Client())
		ctx := context.Background()
		if _, err := c.Dial(ctx, DialRequest{To: "+12125550142"}); err != nil {
			t.Fatalf("Dial error = %v", err)
		}
		if err := c.StartRecording(ctx, testCallID); err != nil {
			t.Fatalf("StartRecording error = %v", err)
		}
		if err := c.Hangup(ctx, testCallID); err != nil {
			t.Fatalf("Hangup error = %v", err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if len(fake.requests) != 3 {
			t.Fatalf("fake telnyx got %d requests; want 3", len(fake.requests))
		}
		for _, r := range fake.requests {
			if r.auth != wantAuth {
				t.Errorf("%s %s Authorization = %q; want %q", r.method, r.path, r.auth, wantAuth)
			}
		}
	})

	// The fake echoes the Authorization header back in its error text, so the
	// key reaches the client inside the reply and must be removed from the error.
	echoes := map[string]http.HandlerFunc{
		"json errors reply": func(w http.ResponseWriter, r *http.Request) {
			reply, _ := json.Marshal(map[string]any{"errors": []map[string]string{{
				"code":   "10009",
				"title":  "Authentication failed",
				"detail": "header was " + r.Header.Get("Authorization"),
			}}})
			replyJSON(http.StatusUnauthorized, string(reply))(w, r)
		},
		"plain text reply": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, "bad key "+r.Header.Get("Authorization"))
		},
	}
	for name, handler := range echoes {
		t.Run("4xx "+name+" hides the key", func(t *testing.T) {
			fake := newFake(t, handler)
			c := NewClient(fake.URL, fakeKey, fake.Client())

			err := c.Hangup(context.Background(), testCallID)
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("Hangup error = %v (%T); want *APIError", err, err)
			}
			if apiErr.Status != http.StatusUnauthorized {
				t.Errorf("APIError.Status = %d; want 401", apiErr.Status)
			}
			if fake.only(t).auth != wantAuth {
				t.Fatalf("fake did not receive the key, so the check below proves nothing")
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(apiErr.Detail, fakeKey) {
				t.Errorf("error text contains the API key: %q", err.Error())
			}
			if !strings.Contains(apiErr.Detail, "[redacted]") {
				t.Errorf("APIError.Detail = %q; want the echoed key replaced by [redacted]", apiErr.Detail)
			}
		})
	}

	t.Run("4xx json reply fills code and detail", func(t *testing.T) {
		fake := newFake(t, replyJSON(http.StatusUnprocessableEntity,
			`{"errors":[{"code":"90018","title":"Call has already ended","detail":"This call is no longer active."}]}`))
		c := NewClient(fake.URL, fakeKey, fake.Client())

		err := c.StartRecording(context.Background(), testCallID)
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("StartRecording error = %v (%T); want *APIError", err, err)
		}
		want := APIError{Status: 422, Code: "90018", Detail: "This call is no longer active."}
		if apiErr.Status != want.Status || apiErr.Code != want.Code || apiErr.Detail != want.Detail {
			t.Errorf("APIError = %+v; want %+v", *apiErr, want)
		}
		if !strings.Contains(err.Error(), "record_start call "+testCallID) {
			t.Errorf("error text %q does not name the operation and call id", err.Error())
		}
	})
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestTransportErrorsHideKey(t *testing.T) {
	for _, method := range []string{"dial", "record", "hangup", "download"} {
		t.Run(method, func(t *testing.T) {
			cause := errors.New("transport echoed Bearer " + fakeKey)
			requests := 0
			hc := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				// D-2: a download carries no key; every command does.
				wantAuth := "Bearer " + fakeKey
				if method == "download" {
					wantAuth = ""
				}
				if got := r.Header.Get("Authorization"); got != wantAuth {
					t.Errorf("transport got Authorization %q; want %q", got, wantAuth)
				}
				return nil, cause
			})}
			c := NewClient("https://api.invalid/v2", fakeKey, hc)
			ctx := context.Background()
			var err error
			switch method {
			case "dial":
				_, err = c.Dial(ctx, DialRequest{To: "+12125550142"})
			case "record":
				err = c.StartRecording(ctx, testCallID)
			case "hangup":
				err = c.Hangup(ctx, testCallID)
			case "download":
				err = c.Download(ctx, "https://recording.invalid/rec.mp3", io.Discard)
			}
			if requests != 1 || !errors.Is(err, cause) {
				t.Fatalf("requests = %d, error = %v; want one request and original cause", requests, err)
			}
			if strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), "[redacted]") {
				t.Errorf("transport error was not redacted: %s", err)
			}
		})
	}
}

func TestDownloadWriterErrorHidesKey(t *testing.T) {
	fake := newFake(t, replyJSON(http.StatusOK, "recording bytes"))
	c := NewClient(fake.URL, fakeKey, fake.Client())
	cause := errors.New("writer echoed " + fakeKey)
	err := c.Download(context.Background(), fake.URL+"/rec.mp3", failingWriter{err: cause})
	if !errors.Is(err, cause) {
		t.Fatalf("Download error = %v; want writer failure", err)
	}
	if strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), "[redacted]") {
		t.Errorf("writer error was not redacted: %s", err)
	}
	if got := fake.only(t); got.auth != "" {
		t.Errorf("download Authorization = %q; want no Authorization header (D-2)", got.auth)
	}
}

// filterLine is "<path>" followed by each query parameter as " name=value",
// decoded and sorted, so an id that was not query-escaped shows up as extra
// parameters.
func filterLine(path string, q url.Values) string {
	line := path
	for _, name := range slices.Sorted(maps.Keys(q)) {
		for _, value := range q[name] {
			line += " " + name + "=" + value
		}
	}
	return line
}

func recordingsAPI(t *testing.T, pages map[string]string, deleteStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			replyJSON(deleteStatus, `{"data":{}}`)(w, r)
			return
		}
		line := filterLine(r.URL.Path, r.URL.Query())
		page, ok := pages[line]
		if r.Method != http.MethodGet || !ok {
			t.Errorf("fake telnyx: unexpected request %s %s", r.Method, line)
			replyJSON(http.StatusBadRequest, `{"errors":[{"code":"400","detail":"unexpected"}]}`)(w, r)
			return
		}
		replyJSON(http.StatusOK, page)(w, r)
	}
}

// requestLines returns every captured request as a sorted "METHOD filterLine"
// line, and fails the test when one lacks the bearer key.
func (f *fakeTelnyx) requestLines(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		if r.auth != "Bearer "+fakeKey {
			t.Errorf("%s %s Authorization = %q; want the bearer key", r.method, r.path, r.auth)
		}
		q, err := url.ParseQuery(r.query)
		if err != nil {
			t.Errorf("%s %s: query %q does not parse: %v", r.method, r.path, r.query, err)
		}
		lines = append(lines, r.method+" "+filterLine(r.path, q))
	}
	slices.Sort(lines)
	return lines
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("requests =\n %q\nwant\n %q", got, want)
	}
}

func TestDeleteRecording(t *testing.T) {
	cases := []struct {
		name   string
		status int
		ref    RecordingRef
		want   []string
	}{
		{"200 deletes", http.StatusOK, RecordingRef{RecordingID: "rec-1", TranscriptionID: "tr-1"},
			[]string{"DELETE /recording_transcriptions/tr-1", "DELETE /recordings/rec-1"}},
		{"404 is already deleted", http.StatusNotFound, RecordingRef{RecordingID: "rec-1", TranscriptionID: "tr-1"},
			[]string{"DELETE /recording_transcriptions/tr-1", "DELETE /recordings/rec-1"}},
		{"ids are path-escaped", http.StatusOK, RecordingRef{RecordingID: "rec/1?x", TranscriptionID: "tr 1#y"},
			[]string{"DELETE /recording_transcriptions/tr%201%23y", "DELETE /recordings/rec%2F1%3Fx"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, recordingsAPI(t, nil, tc.status))
			c := NewClient(fake.URL, fakeKey, fake.Client())

			if err := c.DeleteRecording(context.Background(), tc.ref); err != nil {
				t.Fatalf("DeleteRecording error = %v; want nil", err)
			}
			assertLines(t, fake.requestLines(t), tc.want)
		})
	}
}

func TestDeleteRecordingPartial(t *testing.T) {
	t.Run("only the transcription id", func(t *testing.T) {
		fake := newFake(t, recordingsAPI(t, nil, http.StatusOK))
		c := NewClient(fake.URL, fakeKey, fake.Client())

		if err := c.DeleteRecording(context.Background(), RecordingRef{TranscriptionID: "tr-1"}); err != nil {
			t.Fatalf("DeleteRecording error = %v; want nil", err)
		}
		assertLines(t, fake.requestLines(t), []string{"DELETE /recording_transcriptions/tr-1"})
	})

	t.Run("only the recording id", func(t *testing.T) {
		fake := newFake(t, recordingsAPI(t, map[string]string{
			"/recording_transcriptions filter[recording_id]=rec-1": `{"data":[{"id":"tr-a"},{"id":"tr-b"}]}`,
		}, http.StatusOK))
		c := NewClient(fake.URL, fakeKey, fake.Client())

		if err := c.DeleteRecording(context.Background(), RecordingRef{RecordingID: "rec-1"}); err != nil {
			t.Fatalf("DeleteRecording error = %v; want nil", err)
		}
		assertLines(t, fake.requestLines(t), []string{
			"GET /recording_transcriptions filter[recording_id]=rec-1",
			"DELETE /recording_transcriptions/tr-a",
			"DELETE /recording_transcriptions/tr-b",
			"DELETE /recordings/rec-1",
		})
	})

	t.Run("no id at all", func(t *testing.T) {
		fake := newFake(t, recordingsAPI(t, nil, http.StatusOK))
		c := NewClient(fake.URL, fakeKey, fake.Client())

		err := c.DeleteRecording(context.Background(), RecordingRef{})
		if !errors.Is(err, errNoRecordingID) {
			t.Fatalf("DeleteRecording error = %v; want %v", err, errNoRecordingID)
		}
		if !strings.Contains(err.Error(), "no recording, transcription or call leg id is known") {
			t.Errorf("error text %q; want it to name that no id is known", err)
		}
		if got := fake.requestLines(t); len(got) != 0 {
			t.Errorf("fake telnyx got %q; want no request", got)
		}
	})
}

func TestDeleteRecordingByLeg(t *testing.T) {
	cases := []struct {
		name  string
		leg   string
		lists map[string]string
		want  []string
	}{
		{
			name: "lists and deletes each recording and its transcriptions",
			leg:  "leg-1",
			lists: map[string]string{
				"/recordings filter[call_leg_id]=leg-1":                `{"data":[{"id":"rec-a"},{"id":"rec-b"}]}`,
				"/recording_transcriptions filter[recording_id]=rec-a": `{"data":[{"id":"tr-a"}]}`,
				"/recording_transcriptions filter[recording_id]=rec-b": `{"data":[]}`,
			},
			want: []string{
				"GET /recordings filter[call_leg_id]=leg-1",
				"GET /recording_transcriptions filter[recording_id]=rec-a",
				"GET /recording_transcriptions filter[recording_id]=rec-b",
				"DELETE /recording_transcriptions/tr-a",
				"DELETE /recordings/rec-a",
				"DELETE /recordings/rec-b",
			},
		},
		{
			name:  "empty recordings list",
			leg:   "leg-1",
			lists: map[string]string{"/recordings filter[call_leg_id]=leg-1": `{"data":[]}`},
			want:  []string{"GET /recordings filter[call_leg_id]=leg-1"},
		},
		{
			// Unescaped, the "&" would split it into a second parameter x=y.
			name:  "leg id is query-escaped",
			leg:   "leg/1&x=y",
			lists: map[string]string{"/recordings filter[call_leg_id]=leg/1&x=y": `{"data":[]}`},
			want:  []string{"GET /recordings filter[call_leg_id]=leg/1&x=y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFake(t, recordingsAPI(t, tc.lists, http.StatusOK))
			c := NewClient(fake.URL, fakeKey, fake.Client())

			if err := c.DeleteRecording(context.Background(), RecordingRef{CallLegID: tc.leg}); err != nil {
				t.Fatalf("DeleteRecording error = %v; want nil", err)
			}
			assertLines(t, fake.requestLines(t), tc.want)
		})
	}
}

func TestDeleteRecordingErrorHidesKey(t *testing.T) {
	// The fake echoes the Authorization header back, so the key reaches the
	// client inside the reply and must be removed from the error.
	echoKey := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "failed for "+r.Header.Get("Authorization"))
	}
	assertSafe := func(t *testing.T, err error, ops ...string) {
		t.Helper()
		if err == nil {
			t.Fatal("DeleteRecording error = nil; want an error")
		}
		text := err.Error()
		for _, op := range ops {
			if !strings.Contains(text, op) {
				t.Errorf("error text %q; want it to name %q", text, op)
			}
		}
		if strings.Contains(text, fakeKey) || !strings.Contains(text, "[redacted]") {
			t.Errorf("error text was not redacted: %q", text)
		}
	}

	t.Run("500 on every delete", func(t *testing.T) {
		fake := newFake(t, echoKey)
		c := NewClient(fake.URL, fakeKey, fake.Client())

		err := c.DeleteRecording(context.Background(), RecordingRef{RecordingID: "rec-1", TranscriptionID: "tr-1"})
		assertSafe(t, err, "delete transcription tr-1", "delete recording rec-1")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
			t.Errorf("error = %v (%T); want *APIError with status 500", err, err)
		}
		// Both were tried although the first failed.
		assertLines(t, fake.requestLines(t), []string{"DELETE /recording_transcriptions/tr-1", "DELETE /recordings/rec-1"})
	})

	t.Run("500 on the list by leg", func(t *testing.T) {
		fake := newFake(t, echoKey)
		c := NewClient(fake.URL, fakeKey, fake.Client())

		err := c.DeleteRecording(context.Background(), RecordingRef{CallLegID: "leg-1"})
		assertSafe(t, err, "list recordings of call leg leg-1", "status 500")
		assertLines(t, fake.requestLines(t), []string{"GET /recordings filter[call_leg_id]=leg-1"})
	})

	t.Run("transport error", func(t *testing.T) {
		cause := errors.New("transport echoed Bearer " + fakeKey)
		var requests []string
		hc := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			requests = append(requests, r.Method+" "+r.URL.EscapedPath())
			if got := r.Header.Get("Authorization"); got != "Bearer "+fakeKey {
				t.Errorf("transport got Authorization %q; want the bearer key", got)
			}
			return nil, cause
		})}
		c := NewClient("https://api.invalid/v2", fakeKey, hc)

		err := c.DeleteRecording(context.Background(), RecordingRef{RecordingID: "rec-1", TranscriptionID: "tr-1"})
		assertSafe(t, err, "delete transcription tr-1", "delete recording rec-1")
		if !errors.Is(err, cause) {
			t.Errorf("original cause lost: %v", err)
		}
		assertLines(t, requests, []string{"DELETE /v2/recording_transcriptions/tr-1", "DELETE /v2/recordings/rec-1"})
	})
}

func TestDeleteRecordingAcrossPages(t *testing.T) {
	fake := newFake(t, recordingsAPI(t, map[string]string{
		"/recordings filter[call_leg_id]=leg-1":                                    `{"data":[{"id":"rec-a"}],"meta":{"page_number":1,"total_pages":2}}`,
		"/recordings filter[call_leg_id]=leg-1 page[number]=2":                     `{"data":[{"id":"rec-b"}],"meta":{"page_number":2,"total_pages":2}}`,
		"/recording_transcriptions filter[recording_id]=rec-a":                     `{"data":[]}`,
		"/recording_transcriptions filter[recording_id]=rec-b":                     `{"data":[{"id":"tr-a"}],"meta":{"cursors":{"after":"token+="},"next":"/v2/recording_transcriptions?page[after]=token%2B%3D"}}`,
		"/recording_transcriptions filter[recording_id]=rec-b page[after]=token+=": `{"data":[{"id":"tr-b"}],"meta":{"next":null}}`,
	}, http.StatusOK))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	if err := c.DeleteRecording(context.Background(), RecordingRef{CallLegID: "leg-1"}); err != nil {
		t.Fatalf("DeleteRecording: %v", err)
	}
	assertLines(t, fake.requestLines(t), []string{
		"GET /recordings filter[call_leg_id]=leg-1",
		"GET /recordings filter[call_leg_id]=leg-1 page[number]=2",
		"GET /recording_transcriptions filter[recording_id]=rec-a",
		"GET /recording_transcriptions filter[recording_id]=rec-b",
		"GET /recording_transcriptions filter[recording_id]=rec-b page[after]=token+=",
		"DELETE /recording_transcriptions/tr-a",
		"DELETE /recording_transcriptions/tr-b",
		"DELETE /recordings/rec-a",
		"DELETE /recordings/rec-b",
	})
}

func TestDeleteRecordingReportsIncompleteDiscovery(t *testing.T) {
	fake := newFake(t, recordingsAPI(t, map[string]string{
		"/recordings filter[call_leg_id]=leg-1":                `{"data":[{},{"id":"rec-a"},{"id":"rec-b"}]}`,
		"/recording_transcriptions filter[recording_id]=rec-a": `{}`,
		"/recording_transcriptions filter[recording_id]=rec-b": `{"data":[{"id":"tr-b"}]}`,
	}, http.StatusOK))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	err := c.DeleteRecording(context.Background(), RecordingRef{CallLegID: "leg-1"})
	if err == nil || !strings.Contains(err.Error(), "list recordings of call leg leg-1: missing id") ||
		!strings.Contains(err.Error(), "list transcriptions of recording rec-a: missing list data") {
		t.Errorf("DeleteRecording error = %v; want both discovery errors", err)
	}
	assertLines(t, fake.requestLines(t), []string{
		"GET /recordings filter[call_leg_id]=leg-1",
		"GET /recording_transcriptions filter[recording_id]=rec-a",
		"GET /recording_transcriptions filter[recording_id]=rec-b",
		"DELETE /recording_transcriptions/tr-b",
		"DELETE /recordings/rec-a",
		"DELETE /recordings/rec-b",
	})
}

func TestDeleteRecordingRejectsDotSegments(t *testing.T) {
	fake := newFake(t, recordingsAPI(t, nil, http.StatusOK))
	c := NewClient(fake.URL, fakeKey, fake.Client())

	err := c.DeleteRecording(context.Background(), RecordingRef{RecordingID: "..", TranscriptionID: "."})
	if err == nil || !strings.Contains(err.Error(), "delete recording ..: invalid resource id") ||
		!strings.Contains(err.Error(), "delete transcription .: invalid resource id") {
		t.Errorf("DeleteRecording error = %v; want both invalid ID errors", err)
	}
	assertLines(t, fake.requestLines(t), nil)
}
