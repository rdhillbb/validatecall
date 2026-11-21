package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"checknumber/internal/check"
	"checknumber/internal/config"
	"checknumber/internal/logfile"
	"checknumber/internal/result"
	"checknumber/internal/store"
	"checknumber/internal/telnyx"
)

// C-5: tests touch only this database and never a real Telnyx; handlers are
// called directly. The one test that opens listeners (TestShutdownHangsUpCall)
// lets the system pick free ports.
const testDatabase = "checknumber_test"

const (
	inputNumber   = "+15552230001"
	otherNumber   = "+15552230002"
	queuedNumber  = "+15552230003"
	testPublicURL = "https://tunnel-test.example.test"
	day           = 24 * time.Hour

	hangupBody = `{"data":{"event_type":"call.hangup","occurred_at":"2026-11-13T15:59:58Z","payload":{` +
		`"call_control_id":"v3:call-split","hangup_cause":"user_busy"}}}`
	headerSignature = "telnyx-signature-ed25519"
	headerTimestamp = "telnyx-timestamp"
	contentHTML     = "text/html; charset=utf-8"
	pageTitle       = "<title>checknumber Test Page</title>"
	// Independent of the production constant so a wrong reason fails DD-2.
	wantRestartReason = "app restarted"

	// A test value only; no real key is used (C-4).
	testAPIKey          = "KEYtest-check-api-key-not-real"
	headerAuthorization = "Authorization"
	bearerPrefix        = "Bearer "
	formContentType     = "application/x-www-form-urlencoded"

	// These ports only form host names and addresses; no listener is opened
	// on them.
	testLocalPort  = 61001
	testEventsPort = 61002
	testLocalHost  = "127.0.0.1:61001"
	testEventsHost = "127.0.0.1:61002"

	// Independent of the production constants so a changed setting value or
	// events path fails DD-3.
	tunnelSetting     = "cloudflared"
	webhookPath       = "/telnyx/events"
	fakeTunnelAddress = "https://fake-test-tunnel.trycloudflare.com"
	fakeTunnelHost    = "fake-test-tunnel.trycloudflare.com"

	// Test values only; no real key or account id is used (C-4).
	testTelnyxKey    = "KEYtest-telnyx-key-not-real"
	testConnectionID = "test-connection-id-not-real"
	startupDatabase  = "checknumber_startup_test"

	// anyLoopbackPort makes the system pick a free port (no fixed port).
	anyLoopbackPort = "127.0.0.1:0"
	serverCount     = 2
	// serveWait bounds each wait for serve; probeTimeout one request to it.
	serveWait    = 20 * time.Second
	probeTimeout = 5 * time.Second
)

var restartAt = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

type fakeChecker struct {
	mu     sync.Mutex
	inputs []string
}

func (c *fakeChecker) Check(_ context.Context, input string) (check.Answer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, input)
	return check.Answer{Number: input, Code: result.Busy, Label: result.Label(result.Busy), CalledAt: restartAt}, nil
}

func (c *fakeChecker) History(context.Context, string) ([]check.Answer, error) {
	return nil, nil
}

func (c *fakeChecker) checked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.inputs...)
}

type fakeSink struct {
	mu     sync.Mutex
	events []telnyx.Event
}

func (s *fakeSink) HandleEvent(ev telnyx.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *fakeSink) received() []telnyx.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]telnyx.Event(nil), s.events...)
}

// handlers builds both handlers with fakes and a key generated here.
func handlers(t *testing.T) (local, events http.Handler, cfg config.Config, priv ed25519.PrivateKey, checker *fakeChecker, sink *fakeSink) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	cfg = config.Config{
		Port:            testLocalPort,
		PublicURL:       testPublicURL,
		RecordingsDir:   t.TempDir(),
		TelnyxPublicKey: pub,
	}
	checker, sink = &fakeChecker{}, &fakeSink{}
	local, events = buildHandlers(cfg, checker, sink)
	return local, events, cfg, priv, checker, sink
}

func serveOnce(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// serveLocal sends req to the local handler addressed as the local listener;
// under any other host name the handler refuses it before it is routed (D-9).
func serveLocal(local http.Handler, req *http.Request) *httptest.ResponseRecorder {
	req.Host = testLocalHost
	return serveOnce(local, req)
}

func checkRequest() *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/check", strings.NewReader(`{"number":"`+inputNumber+`"}`))
}

// signedEvent is a Telnyx event signed now with priv, posted to path.
func signedEvent(t *testing.T, priv ed25519.PrivateKey, path string) *http.Request {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(priv, []byte(ts+"|"+hangupBody))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(hangupBody))
	req.Header.Set(headerTimestamp, ts)
	req.Header.Set(headerSignature, base64.StdEncoding.EncodeToString(sig))
	return req
}

func TestBuildHandlersSplit(t *testing.T) {
	local, events, cfg, priv, checker, sink := handlers(t)

	// The local handler serves the page and the check API.
	rec := serveLocal(local, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != contentHTML {
		t.Errorf("local GET / = %d %q, want 200 %q", rec.Code, rec.Header().Get("Content-Type"), contentHTML)
	}
	if !strings.Contains(rec.Body.String(), pageTitle) {
		t.Errorf("local GET / does not contain the checknumber page title")
	}
	rec = serveLocal(local, checkRequest())
	var reply struct {
		Number     string `json:"number"`
		ResultCode string `json:"result_code"`
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("local POST /api/check = %d, want 200; body %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("local POST /api/check reply %q: %v", rec.Body, err)
	}
	if reply.Number != inputNumber || reply.ResultCode != string(result.Busy) {
		t.Errorf("local POST /api/check reply = %+v, want %s %s", reply, inputNumber, result.Busy)
	}
	if got := checker.checked(); len(got) != 1 || got[0] != inputNumber {
		t.Fatalf("checker inputs after local check = %v, want [%s]", got, inputNumber)
	}

	// The events handler answers 404 for the page and the API, and runs no check.
	for _, req := range []*http.Request{httptest.NewRequest(http.MethodGet, "/", nil), checkRequest()} {
		rec := serveOnce(events, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("events %s %s = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
	if got := checker.checked(); len(got) != 1 {
		t.Errorf("checker inputs after events requests = %v, want only the local check", got)
	}

	// Telnyx events are accepted at the webhook URL's path on the events
	// handler only.
	webhook, err := url.Parse(cfg.WebhookURL())
	if err != nil {
		t.Fatalf("parse webhook URL %q: %v", cfg.WebhookURL(), err)
	}
	rec = serveLocal(local, signedEvent(t, priv, webhook.Path))
	if rec.Code == http.StatusOK || len(sink.received()) != 0 {
		t.Errorf("local POST %s = %d with %d events delivered, want refused", webhook.Path, rec.Code, len(sink.received()))
	}
	rec = serveOnce(events, signedEvent(t, priv, webhook.Path))
	if rec.Code != http.StatusOK {
		t.Errorf("events POST %s = %d, want 200", webhook.Path, rec.Code)
	}
	if got := sink.received(); len(got) != 1 || got[0].Type != telnyx.EventCallHangup {
		t.Errorf("events delivered = %+v, want one %s", got, telnyx.EventCallHangup)
	}
}

// fakeDialer counts calls; no real call is placed (C-5).
type fakeDialer struct {
	mu      sync.Mutex
	numbers []string
}

func (d *fakeDialer) Call(_ context.Context, number string) check.Outcome {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.numbers = append(d.numbers, number)
	return check.Outcome{Code: result.Busy, StartedAt: restartAt}
}

func (d *fakeDialer) dialled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.numbers...)
}

// newTestStore opens the store on checknumber_test in a fresh, empty schema
// dropped when the test ends; the store is built only from the environment.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	t.Setenv("PGDATABASE", testDatabase)
	admin, err := pgx.Connect(ctx, "")
	if err != nil {
		t.Fatalf("connect to %s: %v", testDatabase, err)
	}
	t.Cleanup(func() {
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	schema := fmt.Sprintf("main_test_%d", time.Now().UnixNano())
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+ident); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+ident+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})
	t.Setenv("PGOPTIONS", "-c search_path="+schema)
	st, err := store.Open(ctx)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// seedDone stores a finished check of number whose call started at startedAt.
func seedDone(t *testing.T, st *store.Store, number string, code result.Code, startedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	id, err := st.Begin(ctx, number, startedAt)
	if err != nil {
		t.Fatalf("Begin %s: %v", number, err)
	}
	if err := st.MarkStarted(ctx, id, startedAt); err != nil {
		t.Fatalf("MarkStarted %d: %v", id, err)
	}
	if err := st.Finish(ctx, id, string(code), "", "", "", startedAt.Add(time.Minute)); err != nil {
		t.Fatalf("Finish %d: %v", id, err)
	}
}

func TestPrepareMarksUnfinished(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	// On an empty schema prepare creates the table (Migrate runs first).
	if err := prepare(ctx, st, restartAt.Add(-day)); err != nil {
		t.Fatalf("prepare on an empty schema: %v", err)
	}

	// Recent fax results would be reused (R-18) unless a newer check was
	// cut off. Cover both an active call and a request still queued to dial.
	seedDone(t, st, inputNumber, result.Fax, restartAt.Add(-2*day))
	seedDone(t, st, otherNumber, result.Fax, restartAt.Add(-2*day))
	cutOff, err := st.Begin(ctx, inputNumber, restartAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Begin cut-off check: %v", err)
	}
	if err := st.MarkStarted(ctx, cutOff, restartAt.Add(-time.Hour)); err != nil {
		t.Fatalf("MarkStarted cut-off check: %v", err)
	}

	seedDone(t, st, queuedNumber, result.Fax, restartAt.Add(-2*day))
	queued, err := st.Begin(ctx, queuedNumber, restartAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("Begin queued check: %v", err)
	}

	if err := prepare(ctx, st, restartAt); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	for _, tc := range []struct {
		number string
		id     int64
	}{{inputNumber, cutOff}, {queuedNumber, queued}} {
		history, err := st.History(ctx, tc.number)
		if err != nil {
			t.Fatalf("History %s: %v", tc.number, err)
		}
		if len(history) != 2 {
			t.Fatalf("history of %s has %d checks, want 2", tc.number, len(history))
		}
		got, earlier := history[0], history[1]
		if got.ID != tc.id || got.State != store.StateDone || got.ResultCode != string(result.Unknown) ||
			got.Reason != wantRestartReason || !got.FinishedAt.Equal(restartAt) {
			t.Errorf("cut-off check after prepare = %+v, want done %s %q finished %s",
				got, result.Unknown, wantRestartReason, restartAt)
		}
		if earlier.ResultCode != string(result.Fax) || earlier.Reason != "" {
			t.Errorf("earlier check changed by prepare: %+v", earlier)
		}
	}

	// The next requests dial both restarted numbers again (R-21); the other
	// number still gets its saved result without a call.
	dialer := &fakeDialer{}
	svc := check.NewService(st, dialer, func() time.Time { return restartAt.Add(time.Minute) })
	for _, number := range []string{inputNumber, queuedNumber} {
		answer, err := svc.Check(ctx, number)
		if err != nil {
			t.Fatalf("Check %s: %v", number, err)
		}
		if answer.Saved || answer.Code != result.Busy {
			t.Errorf("answer for %s = %+v, want a new call's result", number, answer)
		}
	}

	other, err := svc.Check(ctx, otherNumber)
	if err != nil {
		t.Fatalf("Check %s: %v", otherNumber, err)
	}
	if !other.Saved || other.Code != result.Fax {
		t.Errorf("answer for %s = %+v, want the saved fax result", otherNumber, other)
	}
	if got, want := dialer.dialled(), []string{inputNumber, queuedNumber}; !reflect.DeepEqual(got, want) {
		t.Errorf("dialled %v, want %v", got, want)
	}
}

// keyedHandlers builds the local handler with CHECKNUMBER_API_KEY set to the
// test key (AD-6, M-2).
func keyedHandlers(t *testing.T) (local http.Handler, checker *fakeChecker) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	cfg := config.Config{
		Port:            testLocalPort,
		PublicURL:       testPublicURL,
		RecordingsDir:   t.TempDir(),
		TelnyxPublicKey: pub,
		APIKey:          testAPIKey,
	}
	checker = &fakeChecker{}
	local, _ = buildHandlers(cfg, checker, &fakeSink{})
	return local, checker
}

func TestAPIKeyRequired(t *testing.T) {
	local, checker := keyedHandlers(t)

	// Without the key, or with a wrong one, every API route gets 401 and no
	// check runs.
	for _, auth := range []string{"", bearerPrefix + "wrong-test-key", testAPIKey} {
		for _, req := range []*http.Request{checkRequest(), httptest.NewRequest(http.MethodGet, "/api/history?number="+inputNumber, nil)} {
			if auth != "" {
				req.Header.Set(headerAuthorization, auth)
			}
			rec := serveLocal(local, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with Authorization %q = %d, want 401", req.Method, req.URL.Path, auth, rec.Code)
			}
			if strings.Contains(rec.Body.String(), testAPIKey) {
				t.Errorf("%s %s reply contains the key", req.Method, req.URL.Path)
			}
		}
	}
	if got := checker.checked(); len(got) != 0 {
		t.Fatalf("checker inputs without the key = %v, want none", got)
	}

	// With the key the check reply comes back.
	req := checkRequest()
	req.Header.Set(headerAuthorization, bearerPrefix+testAPIKey)
	rec := serveLocal(local, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/check with the key = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var reply struct {
		Number     string `json:"number"`
		ResultCode string `json:"result_code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
		t.Fatalf("POST /api/check reply %q: %v", rec.Body, err)
	}
	if reply.Number != inputNumber || reply.ResultCode != string(result.Busy) {
		t.Errorf("POST /api/check reply = %+v, want %s %s", reply, inputNumber, result.Busy)
	}
	if got := checker.checked(); len(got) != 1 || got[0] != inputNumber {
		t.Errorf("checker inputs with the key = %v, want [%s]", got, inputNumber)
	}
}

func TestPageNeedsNoKey(t *testing.T) {
	local, checker := keyedHandlers(t)

	rec := serveLocal(local, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != contentHTML {
		t.Errorf("GET / without a key = %d %q, want 200 %q", rec.Code, rec.Header().Get("Content-Type"), contentHTML)
	}
	if !strings.Contains(rec.Body.String(), pageTitle) {
		t.Errorf("GET / without a key does not contain the checknumber page title")
	}

	// The page's own check form needs no key either.
	req := httptest.NewRequest(http.MethodPost, "/check", strings.NewReader(url.Values{"number": {inputNumber}}.Encode()))
	req.Header.Set("Content-Type", formContentType)
	rec = serveLocal(local, req)
	if rec.Code != http.StatusOK {
		t.Errorf("POST /check without a key = %d, want 200", rec.Code)
	}
	if got := checker.checked(); len(got) != 1 || got[0] != inputNumber {
		t.Errorf("checker inputs after the page check = %v, want [%s]", got, inputNumber)
	}
}

func TestLocalHostCheck(t *testing.T) {
	local, events, _, priv, checker, sink := handlers(t)

	for _, tc := range []struct {
		name string
		host string
		want int
	}{
		{"loopback address with the port", testLocalHost, http.StatusOK},
		{"localhost with the port", "localhost:61001", http.StatusOK},
		{"foreign name", "rebind.example.test", http.StatusForbidden},
		{"foreign name with the port", "rebind.example.test:61001", http.StatusForbidden},
		{"foreign name that starts with localhost", "localhost.example.test:61001", http.StatusForbidden},
		{"the tunnel's name", fakeTunnelHost, http.StatusForbidden},
		{"loopback address without the port", "127.0.0.1", http.StatusForbidden},
		{"localhost without the port", "localhost", http.StatusForbidden},
		{"loopback address with the events port", testEventsHost, http.StatusForbidden},
		{"localhost with the events port", "localhost:61002", http.StatusForbidden},
		{"no host", "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(checker.checked())

			page := httptest.NewRequest(http.MethodGet, "/", nil)
			page.Host = tc.host
			rec := serveOnce(local, page)
			if rec.Code != tc.want {
				t.Errorf("GET / addressed to %q = %d, want %d", tc.host, rec.Code, tc.want)
			}
			if served := strings.Contains(rec.Body.String(), pageTitle); served != (tc.want == http.StatusOK) {
				t.Errorf("GET / addressed to %q: page served = %t", tc.host, served)
			}

			api := checkRequest()
			api.Host = tc.host
			if rec := serveOnce(local, api); rec.Code != tc.want {
				t.Errorf("POST /api/check addressed to %q = %d, want %d", tc.host, rec.Code, tc.want)
			}
			wantChecks := 0
			if tc.want == http.StatusOK {
				wantChecks = 1
			}
			if got := len(checker.checked()) - before; got != wantChecks {
				t.Errorf("checks run for host %q = %d, want %d", tc.host, got, wantChecks)
			}
		})
	}

	// The events handler has no host check: Telnyx addresses it by the
	// tunnel's name (AD-6).
	req := signedEvent(t, priv, webhookPath)
	req.Host = fakeTunnelHost
	if rec := serveOnce(events, req); rec.Code != http.StatusOK {
		t.Errorf("events POST %s addressed to %q = %d, want 200", webhookPath, fakeTunnelHost, rec.Code)
	}
	if got := sink.received(); len(got) != 1 || got[0].Type != telnyx.EventCallHangup {
		t.Errorf("events delivered = %+v, want one %s", got, telnyx.EventCallHangup)
	}
}

var errNoCall = errors.New("test: no call is placed")

// fakeCommands records each dial request and refuses it, so no call is
// placed and the runner returns at once (C-5).
type fakeCommands struct {
	mu    sync.Mutex
	dials []telnyx.DialRequest
}

func (c *fakeCommands) Dial(_ context.Context, req telnyx.DialRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dials = append(c.dials, req)
	return "", errNoCall
}

func (c *fakeCommands) StartRecording(context.Context, string) error { return errNoCall }

func (c *fakeCommands) Hangup(context.Context, string) error { return errNoCall }

func (c *fakeCommands) Download(context.Context, string, io.Writer) error { return errNoCall }

func (c *fakeCommands) DeleteRecording(context.Context, telnyx.RecordingRef) error { return errNoCall }

func (c *fakeCommands) dialled() []telnyx.DialRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telnyx.DialRequest(nil), c.dials...)
}

// fakeTunnel writes a shell script into the test's temp folder that stands
// in for the tunnel program; the real one is never run. The script writes its
// arguments, one per line, to argsFile, prints the address between other
// lines, and then becomes one sleep process, so stopping it leaves no child.
func fakeTunnel(t *testing.T) (program, argsFile string) {
	t.Helper()
	program = filepath.Join(t.TempDir(), "fake-tunnel")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > \"$0.args\"\n" +
		"echo 'INF Requesting new quick Tunnel on trycloudflare.com...'\n" +
		"echo 'INF |  " + fakeTunnelAddress + "  |' >&2\n" +
		"exec sleep 30\n"
	if err := os.WriteFile(program, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake tunnel program: %v", err)
	}
	return program, program + ".args"
}

func TestTunnelAddressUsed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tunnel      string
		wantTunnel  bool
		wantWebhook string
	}{
		{"tunnel setting", tunnelSetting, true, fakeTunnelAddress + webhookPath},
		{"no tunnel setting", "", false, testPublicURL + webhookPath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			program, argsFile := fakeTunnel(t)
			// A public address is set in both cases: with the tunnel setting
			// the tunnel's address must replace it.
			cfg := config.Config{
				Port:          testLocalPort,
				EventsPort:    testEventsPort,
				PublicURL:     testPublicURL,
				RecordingsDir: t.TempDir(),
				CheckLimit:    time.Minute,
				Tunnel:        tc.tunnel,
			}

			got, tun, err := openTunnel(context.Background(), cfg, program)
			if tun != nil {
				t.Cleanup(tun.Stop)
			}
			if err != nil {
				t.Fatalf("openTunnel: %v", err)
			}
			if (tun != nil) != tc.wantTunnel {
				t.Fatalf("openTunnel started a tunnel = %t, want %t", tun != nil, tc.wantTunnel)
			}

			// AD-6: the tunnel is pointed at the events listener and nothing else.
			args, err := os.ReadFile(argsFile)
			if tc.wantTunnel {
				wantArgs := "tunnel\n--no-autoupdate\n--url\nhttp://" + testEventsHost + "\n"
				if err != nil || string(args) != wantArgs {
					t.Errorf("tunnel program arguments = %q (%v), want %q", args, err, wantArgs)
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the tunnel program ran without the tunnel setting: arguments %q (%v)", args, err)
			}

			// The address reaches Telnyx in the dial request of a call.
			cmds := &fakeCommands{}
			newRunner(got, cmds).Call(context.Background(), inputNumber)
			dials := cmds.dialled()
			if len(dials) != 1 || dials[0].To != inputNumber {
				t.Fatalf("dial requests = %+v, want one to %s", dials, inputNumber)
			}
			if dials[0].WebhookURL != tc.wantWebhook {
				t.Errorf("webhook address of the call = %q, want %q", dials[0].WebhookURL, tc.wantWebhook)
			}
		})
	}
}

// fakeRunner stands in for the runner when the app stops. Its Shutdown asks
// every listener for a reply first, which shows whether the listeners were
// still open when it was called.
type fakeRunner struct {
	listening []string

	mu     sync.Mutex
	calls  int
	ctxErr error
	closed []string
}

func (r *fakeRunner) Shutdown(ctx context.Context) error {
	var closed []string
	for _, addr := range r.listening {
		if !answers(addr) {
			closed = append(closed, addr)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.ctxErr = ctx.Err()
	r.closed = closed
	return nil
}

func (r *fakeRunner) seen() (calls int, ctxErr error, closed []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.ctxErr, r.closed
}

// answers reports whether the listener at addr replies to a request.
func answers(addr string) bool {
	client := &http.Client{Timeout: probeTimeout, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get("http://" + addr)
	if err != nil {
		return false
	}
	if err := resp.Body.Close(); err != nil {
		return false
	}
	return resp.StatusCode == http.StatusNoContent
}

func TestShutdownHangsUpCall(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tunnelExits bool
		want        error
	}{
		{"stop signal", false, nil},
		{"tunnel exits", true, errTunnelExited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Each server reports the address the system gave its listener.
			addrs := make(chan string, serverCount)
			var servers []*http.Server
			for range serverCount {
				servers = append(servers, &http.Server{
					Addr: anyLoopbackPort,
					Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.WriteHeader(http.StatusNoContent)
					}),
					BaseContext: func(l net.Listener) context.Context {
						addrs <- l.Addr().String()
						return context.Background()
					},
				})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tunnelExit := make(chan struct{})
			runner := &fakeRunner{}

			served := make(chan error, 1)
			go func() { served <- serve(ctx, servers, runner, tunnelExit) }()
			var listening []string
			for range servers {
				select {
				case addr := <-addrs:
					listening = append(listening, addr)
				case err := <-served:
					t.Fatalf("serve returned before both servers listened: %v", err)
				case <-time.After(serveWait):
					t.Fatalf("servers not listening after %s", serveWait)
				}
			}
			for _, addr := range listening {
				if !answers(addr) {
					t.Fatalf("listener %s does not answer before the stop", addr)
				}
			}
			if calls, _, _ := runner.seen(); calls != 0 {
				t.Fatalf("Shutdown called %d time(s) before the stop", calls)
			}
			// Set before the stop; serve calls Shutdown only after it.
			runner.listening = listening

			if tc.tunnelExits {
				close(tunnelExit)
			} else {
				cancel()
			}

			var err error
			select {
			case err = <-served:
			case <-time.After(serveWait):
				t.Fatalf("serve did not return %s after the stop", serveWait)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("serve = %v, want %v", err, tc.want)
			}
			calls, ctxErr, closed := runner.seen()
			if calls != 1 {
				t.Errorf("Shutdown called %d time(s), want 1", calls)
			}
			if ctxErr != nil {
				t.Errorf("Shutdown got a context that had already ended (%v); the hang-up could not be sent", ctxErr)
			}
			if len(closed) != 0 {
				t.Errorf("listeners %v were closed when Shutdown was called; want both open", closed)
			}
			for _, addr := range listening {
				if answers(addr) {
					t.Errorf("listener %s still answers after serve returned", addr)
				}
			}
		})
	}
}

func TestStartupLine(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	for _, tc := range []struct {
		name   string
		tunnel string
		apiKey string
		want   string
	}{
		{
			"tunnel and key check", tunnelSetting, testAPIKey,
			`checknumber: starting with database "checknumber_startup_test", tunnel cloudflared, API key check on, local 127.0.0.1:61001, events 127.0.0.1:61002`,
		},
		{
			"no tunnel, no key check", "", "",
			`checknumber: starting with database "checknumber_startup_test", tunnel off, API key check off, local 127.0.0.1:61001, events 127.0.0.1:61002`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Port:               testLocalPort,
				EventsPort:         testEventsPort,
				PublicURL:          testPublicURL,
				RecordingsDir:      t.TempDir(),
				TelnyxAPIKey:       testTelnyxKey,
				TelnyxPublicKey:    pub,
				TelnyxFromNumber:   otherNumber,
				TelnyxConnectionID: testConnectionID,
				APIKey:             tc.apiKey,
				Database:           startupDatabase,
				Tunnel:             tc.tunnel,
			}
			var captured bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&captured)
			t.Cleanup(func() { log.SetOutput(previous) })

			logStartup(cfg)

			got := captured.String()
			t.Logf("captured start-up log: %q", got)
			if strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, tc.want+"\n") {
				t.Errorf("start-up log = %q, want one line ending in %q", got, tc.want)
			}
			// C-4: no key, and nothing else that comes from the settings file.
			for _, private := range []string{
				testTelnyxKey, testAPIKey, base64.StdEncoding.EncodeToString(pub), testConnectionID, otherNumber,
			} {
				if strings.Contains(got, private) {
					t.Errorf("start-up log contains the private setting value %q", private)
				}
			}
		})
	}
}

// The log file tests (TC-44). Every log file is in t.TempDir().
const (
	logFileName   = "checknumber-test.log"
	setAsideName  = logFileName + ".1"
	plainFileName = "plain-file-test"
	laterLine     = "checknumber: a later test line"
	// The start of the line that names a log file that cannot be opened.
	cannotOpenPrefix = "checknumber: cannot open log file "
)

// keepLogOutput restores the global logger's output when the test ends.
func keepLogOutput(t *testing.T) {
	t.Helper()
	previous := log.Writer()
	t.Cleanup(func() { log.SetOutput(previous) })
}

// readFile returns the content of path, failing the test when it cannot.
func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// pathUnderFile returns a log path whose folder is a regular file, so the log
// cannot be opened.
func pathUnderFile(t *testing.T) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), plainFileName)
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	return filepath.Join(file, logFileName)
}

// TestOpenLogFile: with a path, the start-up line and later lines go into the
// file, nothing goes to standard error, and the writer is the log file that
// sets itself aside (DD-2: R-29, R-30, AD-7).
func TestOpenLogFile(t *testing.T) {
	keepLogOutput(t)
	// The folder is missing: the log file creates it.
	path := filepath.Join(t.TempDir(), "logs-test", logFileName)
	var stderr bytes.Buffer

	out, closeLog := openLog(path, &stderr)
	if _, ok := out.(*logfile.File); !ok {
		t.Fatalf("openLog returned a %T, want the log file *logfile.File", out)
	}
	log.SetOutput(out)
	logStartup(config.Config{Port: testLocalPort, EventsPort: testEventsPort, Database: startupDatabase})
	log.Print(laterLine)
	// Two lines that together pass the limit: the second sets the file aside.
	half := strings.Repeat("x", logfile.MaxBytes/2)
	log.Print(half)
	log.Print(half)
	if err := closeLog(); err != nil {
		t.Fatalf("close the log file: %v", err)
	}

	if stderr.Len() != 0 {
		t.Errorf("standard error got %q, want nothing", stderr.String())
	}
	setAside := readFile(t, filepath.Join(filepath.Dir(path), setAsideName))
	for _, want := range []string{startupPrefixFor(startupDatabase), laterLine} {
		if !strings.Contains(setAside, want) {
			t.Errorf("set-aside log file does not contain %q", want)
		}
	}
	current := readFile(t, path)
	if strings.Count(current, "\n") != 1 || !strings.HasSuffix(current, half+"\n") {
		t.Errorf("log file after the set-aside holds %d line(s), want only the last one", strings.Count(current, "\n"))
	}
	if len(current) > logfile.MaxBytes {
		t.Errorf("log file is %d bytes, more than %d", len(current), logfile.MaxBytes)
	}
}

// TestOpenLogEmpty: with no path, log lines go to standard error as before
// (DD-3: R-32).
func TestOpenLogEmpty(t *testing.T) {
	keepLogOutput(t)
	var stderr bytes.Buffer

	out, closeLog := openLog("", &stderr)
	if out != io.Writer(&stderr) {
		t.Fatalf("openLog returned a %T, want standard error", out)
	}
	log.SetOutput(out)
	log.Print(laterLine)
	if err := closeLog(); err != nil {
		t.Errorf("close with no log file: %v", err)
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, laterLine+"\n") {
		t.Errorf("standard error got %q, want the one line %q", got, laterLine)
	}
}

// TestOpenLogFails: a log file that cannot be opened gives one line on
// standard error naming the path and the cause, and logging continues there
// (DD-4: AD-7).
func TestOpenLogFails(t *testing.T) {
	keepLogOutput(t)
	path := pathUnderFile(t)
	var stderr bytes.Buffer

	out, closeLog := openLog(path, &stderr)
	if out != io.Writer(&stderr) {
		t.Fatalf("openLog returned a %T, want standard error", out)
	}
	notice := stderr.String()
	if strings.Count(notice, "\n") != 1 || !strings.Contains(notice, cannotOpenPrefix+path) {
		t.Errorf("standard error got %q, want one line with %q", notice, cannotOpenPrefix+path)
	}
	if !strings.Contains(notice, syscall.ENOTDIR.Error()) {
		t.Errorf("line %q does not name the cause %q", notice, syscall.ENOTDIR.Error())
	}

	log.SetOutput(out)
	log.Print(laterLine)
	if err := closeLog(); err != nil {
		t.Errorf("close with no log file: %v", err)
	}
	if got := strings.TrimPrefix(stderr.String(), notice); strings.Count(got, "\n") != 1 || !strings.HasSuffix(got, laterLine+"\n") {
		t.Errorf("standard error after the notice got %q, want the one line %q", got, laterLine)
	}
	if _, err := os.Stat(path); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("stat %s: %v, want %v", path, err, syscall.ENOTDIR)
	}
}

// TestRunLogFile: run opens the log right after the settings, before the
// start-up line, still starts when the file cannot be opened, and the error
// that ends it goes into the log too (TC-44, AD-7). The start ends at the
// database step: the search path names a schema that does not exist, so
// nothing is created in checknumber_test and no listener is opened.
func TestRunLogFile(t *testing.T) {
	// The error of the database step, as run returns it and as it logs it.
	const (
		wantErr = "prepare database: "
		wantEnd = "checknumber: " + wantErr
	)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	for _, tc := range []struct {
		name string
		// toFile: the log file opens, so the lines go there.
		toFile bool
	}{
		{"log file", true},
		{"log file cannot be opened", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keepLogOutput(t)
			path := filepath.Join(t.TempDir(), logFileName)
			if !tc.toFile {
				path = pathUnderFile(t)
			}
			settings := map[string]string{
				"CHECKNUMBER_PORT":                 strconv.Itoa(testLocalPort),
				"CHECKNUMBER_EVENTS_PORT":          strconv.Itoa(testEventsPort),
				"CHECKNUMBER_PUBLIC_URL":           testPublicURL,
				"CHECKNUMBER_RECORDINGS_DIR":       t.TempDir(),
				"TELNYX_API_KEY":                   testTelnyxKey,
				"TELNYX_PUBLIC_KEY":                base64.StdEncoding.EncodeToString(pub),
				"TELNYX_FROM_NUMBER":               otherNumber,
				"TELNYX_CONNECTION_ID":             testConnectionID,
				"CHECKNUMBER_CHECK_LIMIT":          "",
				"CHECKNUMBER_COMBINE_DISCONNECTED": "",
				"CHECKNUMBER_API_KEY":              "",
				"CHECKNUMBER_ENV_FILE":             "",
				"CHECKNUMBER_TUNNEL":               "",
				"CHECKNUMBER_LOG_FILE":             path,
				"PGDATABASE":                       testDatabase,
				"PGOPTIONS":                        fmt.Sprintf("-c search_path=main_test_missing_%d", time.Now().UnixNano()),
			}
			for name, v := range settings {
				t.Setenv(name, v)
			}
			stderr, err := os.CreateTemp(t.TempDir(), "stderr-test")
			if err != nil {
				t.Fatalf("create standard error file: %v", err)
			}
			realStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() {
				os.Stderr = realStderr
				if err := stderr.Close(); err != nil {
					t.Errorf("close standard error file: %v", err)
				}
			})

			err = run(context.Background())
			if err == nil || !strings.HasPrefix(err.Error(), wantErr) {
				t.Fatalf("run = %v, want the error of the database step", err)
			}
			if log.Writer() != io.Writer(stderr) {
				t.Errorf("after run the log goes to a %T, want standard error again", log.Writer())
			}
			gotStderr := readFile(t, stderr.Name())
			wantLines := []string{startupPrefixFor(testDatabase), wantEnd}
			logged := gotStderr
			if tc.toFile {
				logged = readFile(t, path)
				if gotStderr != "" {
					t.Errorf("standard error got %q, want nothing", gotStderr)
				}
			} else {
				wantLines = append([]string{cannotOpenPrefix + path}, wantLines...)
			}
			lines := strings.Split(strings.TrimSuffix(logged, "\n"), "\n")
			if len(lines) != len(wantLines) {
				t.Fatalf("log holds %d line(s) %q, want %d", len(lines), logged, len(wantLines))
			}
			for i, want := range wantLines {
				if !strings.Contains(lines[i], want) {
					t.Errorf("log line %d = %q, want it to contain %q", i+1, lines[i], want)
				}
			}
		})
	}
}

// startupPrefixFor is the start of the start-up line for database (D-8).
func startupPrefixFor(database string) string {
	return fmt.Sprintf("checknumber: starting with database %q", database)
}
