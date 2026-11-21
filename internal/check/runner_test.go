package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"checknumber/internal/result"
	"checknumber/internal/telnyx"
)

// shutdownWaitContext exposes entry into Shutdown's pending-dial select.
// The channel is an ordering barrier, not a clock-based inference.
type shutdownWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *shutdownWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// C-5: every command goes to fakeCommands; no real Telnyx call is placed.
const (
	runnerNumber     = "+15552230101"
	runnerCallID     = "v3:call-one"
	runnerOtherID    = "v3:call-other"
	runnerFrom       = "+15552239999"
	runnerConnection = "conn-test"
	runnerWebhook    = "https://tunnel.example.test/telnyx/events"
	runnerLimit      = 120 * time.Second
	recordingLink    = "https://files.example.test/rec.mp3"
	recordingBytes   = "ID3 fake mp3 bytes"
	greeting         = "You have reached the office, please leave a message."
	// runnerWait bounds every wait on the runner goroutine so a broken flow
	// fails the test instead of hanging it.
	runnerWait = 5 * time.Second
	// TC-36 and TC-37, written out here so a changed runner constant fails.
	runnerTextWait      = 15 * time.Second
	runnerTimeLimitSecs = 150
	// TC-57: the text wait plus 10 seconds after the hang-up.
	runnerHangupWait = 25 * time.Second
	// The ids a recording's events carry, and what DeleteRecording gets (S-2).
	runnerLeg             = "leg-call-one"
	runnerRecordingID     = "rec-call-one"
	runnerTranscriptionID = "tr-call-one"
	// D-11: the carrier message before the far end hangs up.
	disconnectedWords = "This number is not in use. Goodbye"
	// The fixed line logged when a recording cannot be deleted (R-37).
	notDeletedLine = "check: recording of call " + runnerCallID + " could not be deleted"
)

// runnerRef is what DeleteRecording gets once the transcription arrived.
var runnerRef = telnyx.RecordingRef{RecordingID: runnerRecordingID, TranscriptionID: runnerTranscriptionID, CallLegID: runnerLeg}

var runnerStart = time.Date(2026, 11, 2, 15, 0, 0, 0, time.UTC)

// fakeCommands records every command. onDial, when set, runs inside Dial
// before the id is returned (events that beat the dial reply).
type fakeCommands struct {
	id            string
	dialErr       error
	recordErr     error
	downloadErr   error
	deleteErr     error
	onDial        func()
	dialBlock     func(context.Context) error
	recordBlock   func(context.Context) error
	hangupBlock   func(context.Context) error
	downloadBlock func(context.Context) error
	deleteBlock   func(context.Context) error

	dialed chan struct{}
	hungup chan struct{}

	mu        sync.Mutex
	dials     []telnyx.DialRequest
	records   []string
	hangups   []string
	downloads []string
	deletes   []telnyx.RecordingRef
}

func newFakeCommands() *fakeCommands {
	return &fakeCommands{id: runnerCallID, dialed: make(chan struct{}, 1), hungup: make(chan struct{}, 1)}
}

func (f *fakeCommands) Dial(ctx context.Context, req telnyx.DialRequest) (string, error) {
	f.mu.Lock()
	f.dials = append(f.dials, req)
	f.mu.Unlock()
	if f.onDial != nil {
		f.onDial()
	}
	f.dialed <- struct{}{}
	if f.dialBlock != nil {
		return "", f.dialBlock(ctx)
	}
	if f.dialErr != nil {
		return "", f.dialErr
	}
	return f.id, nil
}

func (f *fakeCommands) StartRecording(ctx context.Context, id string) error {
	f.mu.Lock()
	f.records = append(f.records, id)
	f.mu.Unlock()
	if f.recordBlock != nil {
		return f.recordBlock(ctx)
	}
	return f.recordErr
}

func (f *fakeCommands) Hangup(ctx context.Context, id string) error {
	f.mu.Lock()
	f.hangups = append(f.hangups, id)
	f.mu.Unlock()
	f.hungup <- struct{}{}
	if f.hangupBlock != nil {
		return f.hangupBlock(ctx)
	}
	return nil
}

func (f *fakeCommands) Download(ctx context.Context, url string, w io.Writer) error {
	f.mu.Lock()
	f.downloads = append(f.downloads, url)
	f.mu.Unlock()
	if f.downloadBlock != nil {
		return f.downloadBlock(ctx)
	}
	if _, err := io.WriteString(w, recordingBytes); err != nil {
		return err
	}
	return f.downloadErr
}

func (f *fakeCommands) DeleteRecording(ctx context.Context, ref telnyx.RecordingRef) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, ref)
	f.mu.Unlock()
	if f.deleteBlock != nil {
		return f.deleteBlock(ctx)
	}
	return f.deleteErr
}

// deleted returns a copy of the DeleteRecording requests.
func (f *fakeCommands) deleted() []telnyx.RecordingRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]telnyx.RecordingRef(nil), f.deletes...)
}

// snapshot returns copies of the recorded commands.
func (f *fakeCommands) snapshot() (records, hangups, downloads []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.records...), append([]string(nil), f.hangups...), append([]string(nil), f.downloads...)
}

// runnerHarness runs one Call in the background with a limit and a text wait
// the test fires.
type runnerHarness struct {
	cmd    *fakeCommands
	runner *Runner
	dir    string
	limit  chan time.Time
	asked  chan time.Duration
	// wait is the channel After hands out for runnerTextWait; waitAsked gets
	// a token when the runner asks for it.
	wait       chan time.Time
	waitAsked  chan struct{}
	hangupWait chan time.Time
	done       chan Outcome
}

func newRunnerHarness(t *testing.T, cmd *fakeCommands, combine bool) *runnerHarness {
	t.Helper()
	h := &runnerHarness{
		cmd:        cmd,
		dir:        t.TempDir(),
		limit:      make(chan time.Time),
		asked:      make(chan time.Duration, 1),
		wait:       make(chan time.Time),
		waitAsked:  make(chan struct{}, 1),
		hangupWait: make(chan time.Time),
		done:       make(chan Outcome, 1),
	}
	h.runner = NewRunner(cmd, RunnerConfig{
		From:          runnerFrom,
		ConnectionID:  runnerConnection,
		WebhookURL:    runnerWebhook,
		RecordingsDir: h.dir,
		Limit:         runnerLimit,
		Combine:       combine,
		After: func(d time.Duration) <-chan time.Time {
			// Not every test reads a token, and a harness can place two calls.
			switch d {
			case runnerTextWait:
				select {
				case h.waitAsked <- struct{}{}:
				default:
				}
				return h.wait
			case runnerHangupWait:
				return h.hangupWait
			}
			h.asked <- d
			return h.limit
		},
		Now: func() time.Time { return runnerStart },
	})
	return h
}

// start places the call and waits until Dial was reached.
func (h *runnerHarness) start(t *testing.T) {
	t.Helper()
	go func() { h.done <- h.runner.Call(context.Background(), runnerNumber) }()
	select {
	case <-h.cmd.dialed:
	case <-time.After(runnerWait):
		t.Fatal("runner never dialled")
	}
}

func (h *runnerHarness) event(typ string, fill func(*telnyx.Event)) {
	ev := telnyx.Event{Type: typ, CallControlID: runnerCallID}
	if fill != nil {
		fill(&ev)
	}
	h.runner.HandleEvent(ev)
}

func (h *runnerHarness) detection(value string) {
	h.event(telnyx.EventMachinePremiumDetectionEnded, func(ev *telnyx.Event) { ev.DetectionResult = value })
}

// answered, saved and transcribed send the events of an answered, recorded
// call with the ids Telnyx gives them.
func (h *runnerHarness) answered() {
	h.event(telnyx.EventCallAnswered, func(ev *telnyx.Event) { ev.CallLegID = runnerLeg })
}

func (h *runnerHarness) saved() {
	h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) {
		ev.CallLegID, ev.RecordingURL = runnerLeg, recordingLink
	})
}

func (h *runnerHarness) transcribed(text string) {
	h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) {
		ev.CallLegID, ev.TranscriptionText = runnerLeg, text
		ev.RecordingID, ev.TranscriptionID = runnerRecordingID, runnerTranscriptionID
	})
}

func (h *runnerHarness) farHangup() {
	h.event(telnyx.EventCallHangup, func(ev *telnyx.Event) { ev.HangupCause, ev.SIPHangupCause = "normal_clearing", "200" })
}

func (h *runnerHarness) fireLimit(t *testing.T) {
	t.Helper()
	select {
	case h.limit <- runnerStart.Add(runnerLimit):
	case <-time.After(runnerWait):
		t.Fatal("runner is not waiting on the time limit")
	}
}

// awaitTextWait returns once the runner has asked for the text wait.
func (h *runnerHarness) awaitTextWait(t *testing.T) {
	t.Helper()
	select {
	case <-h.waitAsked:
	case <-time.After(runnerWait):
		t.Fatalf("runner never asked for the %v text wait", runnerTextWait)
	}
}

// fireWait ends the text wait. It succeeds only while the runner waits on it.
func (h *runnerHarness) fireWait(t *testing.T) {
	t.Helper()
	select {
	case h.wait <- runnerStart.Add(runnerTextWait):
	case <-time.After(runnerWait):
		t.Fatalf("runner is not in the %v text wait", runnerTextWait)
	}
}

// fireHangupWait ends the wait for the recording after the hang-up. It
// succeeds only while the runner waits on it.
func (h *runnerHarness) fireHangupWait(t *testing.T) {
	t.Helper()
	select {
	case h.hangupWait <- runnerStart.Add(runnerHangupWait):
	case <-time.After(runnerWait):
		t.Fatalf("runner is not in the %v wait after the hang-up", runnerHangupWait)
	}
}

func (h *runnerHarness) outcome(t *testing.T) Outcome {
	t.Helper()
	select {
	case out := <-h.done:
		return out
	case <-time.After(runnerWait):
		t.Fatal("call never returned an outcome")
		return Outcome{}
	}
}

// pending reports that Call has not returned yet.
func (h *runnerHarness) pending(t *testing.T) {
	t.Helper()
	select {
	case out := <-h.done:
		t.Fatalf("call returned early with %+v", out)
	default:
	}
}

func assertOutcome(t *testing.T, got, want Outcome) {
	t.Helper()
	want.StartedAt = runnerStart
	if got != want {
		t.Fatalf("outcome = %+v, want %+v", got, want)
	}
}

func assertCommands(t *testing.T, cmd *fakeCommands, records, hangups, downloads []string) {
	t.Helper()
	if len(hangups) != 0 {
		awaitHangup(t, cmd)
	}
	gotRecords, gotHangups, gotDownloads := cmd.snapshot()
	if !reflect.DeepEqual(gotRecords, records) {
		t.Errorf("StartRecording calls = %q, want %q", gotRecords, records)
	}
	if !reflect.DeepEqual(gotHangups, hangups) {
		t.Errorf("Hangup calls = %q, want %q", gotHangups, hangups)
	}
	if !reflect.DeepEqual(gotDownloads, downloads) {
		t.Errorf("Download calls = %q, want %q", gotDownloads, downloads)
	}
}

func awaitHangup(t *testing.T, cmd *fakeCommands) {
	t.Helper()
	select {
	case <-cmd.hungup:
	case <-time.After(runnerWait):
		t.Fatal("runner never sent hangup")
	}
}

func assertDeletes(t *testing.T, cmd *fakeCommands, want []telnyx.RecordingRef) {
	t.Helper()
	if got := cmd.deleted(); !reflect.DeepEqual(got, want) {
		t.Errorf("DeleteRecording calls = %+v, want %+v", got, want)
	}
}

// logBuffer is a log destination that runner goroutines may write to while
// the test reads it.
type logBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}

// lines returns the logged lines, without the newline.
func (b *logBuffer) lines() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Split(strings.TrimSuffix(b.text.String(), "\n"), "\n")
}

// captureLog sends the standard logger, without its date prefix, to a buffer
// until the test ends.
func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	buf := &logBuffer{}
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return buf
}

// assertLog checks the captured lines; want nil means nothing was logged.
func assertLog(t *testing.T, buf *logBuffer, want []string) {
	t.Helper()
	if want == nil {
		want = []string{""}
	}
	if got := buf.lines(); !reflect.DeepEqual(got, want) {
		t.Errorf("log lines = %q, want %q", got, want)
	}
}

func assertNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read recordings dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("recordings dir holds %d files, want none", len(entries))
	}
}

var oneCall = []string{runnerCallID}

func TestFaxAndSilence(t *testing.T) {
	cases := []struct {
		detection string
		want      Outcome
	}{
		{"fax_detected", Outcome{Code: result.Fax}},
		{"silence", Outcome{Code: result.PickedUp}},
		{"not_sure", Outcome{Code: result.Unknown, Reason: "Telnyx could not tell what answered"}},
	}
	for _, tc := range cases {
		t.Run(tc.detection, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.detection(tc.detection)
			// The recording started on the answer ends with the hang-up (TC-55).
			h.saved()
			h.transcribed("Hello? Hello?")
			assertOutcome(t, h.outcome(t), tc.want)
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, []telnyx.RecordingRef{runnerRef})
			assertNoFiles(t, h.dir)
			wantDial := []telnyx.DialRequest{{
				To: runnerNumber, From: runnerFrom, ConnectionID: runnerConnection, WebhookURL: runnerWebhook,
				TimeLimitSecs: runnerTimeLimitSecs,
			}}
			cmd.mu.Lock()
			defer cmd.mu.Unlock()
			if !reflect.DeepEqual(cmd.dials, wantDial) {
				t.Errorf("dial requests = %+v, want %+v", cmd.dials, wantDial)
			}
		})
	}
}

func TestPersonNeverRecorded(t *testing.T) {
	for _, detection := range []string{"human_residence", "human_business"} {
		t.Run(detection, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.detection(detection)
			// The seconds recorded from the answer are deleted unread (R-37):
			// a greeting phrase in them must not turn the result into Voice message.
			h.saved()
			h.transcribed(greeting)
			assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
			// Late recording events for the finished call must not be acted on.
			h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			h.detection("machine")
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, []telnyx.RecordingRef{runnerRef})
			assertNoFiles(t, h.dir)
		})
	}
}

func TestVoiceMessageRecordedAndText(t *testing.T) {
	saved := func(h *runnerHarness) {
		h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
	}
	text := func(h *runnerHarness) {
		h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = greeting })
	}
	cases := []struct {
		name          string
		first, second func(*runnerHarness)
	}{
		{"recording then text", saved, text},
		{"text then recording", text, saved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.event(telnyx.EventCallAnswered, nil)
			h.detection("machine")
			tc.first(h)
			// Neither event alone completes the call; the far end hanging up does not either.
			h.event(telnyx.EventCallHangup, func(ev *telnyx.Event) { ev.HangupCause = "normal_clearing" })
			h.pending(t)
			tc.second(h)
			out := h.outcome(t)
			name := runnerCallID + ".mp3"
			assertOutcome(t, out, Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: name})
			got, err := os.ReadFile(filepath.Join(h.dir, name))
			if err != nil {
				t.Fatalf("read recording: %v", err)
			}
			if string(got) != recordingBytes {
				t.Errorf("recording = %q, want %q", got, recordingBytes)
			}
			_, hangups, _ := cmd.snapshot()
			if len(hangups) > 1 {
				t.Errorf("Hangup calls = %q, want at most one", hangups)
			}
			records, _, downloads := cmd.snapshot()
			if !reflect.DeepEqual(records, oneCall) || !reflect.DeepEqual(downloads, []string{recordingLink}) {
				t.Errorf("StartRecording = %q, Download = %q; want one each", records, downloads)
			}
		})
	}
}

func TestTranscriptionMissing(t *testing.T) {
	name := runnerCallID + ".mp3"
	cases := []struct {
		name      string
		setup     func(*fakeCommands)
		events    func(*runnerHarness)
		fireLimit bool
		fireWait  bool
		want      Outcome
		file      bool
	}{
		{
			name: "empty transcription",
			events: func(h *runnerHarness) {
				h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
				h.event(telnyx.EventTranscriptionSaved, nil)
			},
			want: Outcome{Code: result.VoiceMessage, Text: "not available: transcription was empty", RecordingPath: name},
			file: true,
		},
		{
			name:   "recording error",
			events: func(h *runnerHarness) { h.event(telnyx.EventRecordingError, nil) },
			want:   Outcome{Code: result.VoiceMessage, Text: "not available: recording failed"},
		},
		{
			name: "transcription late",
			events: func(h *runnerHarness) {
				h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			},
			fireLimit: true,
			want:      Outcome{Code: result.VoiceMessage, Text: "not available: transcription not received in time", RecordingPath: name},
			file:      true,
		},
		{
			name: "text wait ends",
			events: func(h *runnerHarness) {
				h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			},
			fireWait: true,
			want:     Outcome{Code: result.VoiceMessage, Text: "not available: transcription not received in time", RecordingPath: name},
			file:     true,
		},
		{
			name:      "recording never saved",
			events:    func(h *runnerHarness) {},
			fireLimit: true,
			want:      Outcome{Code: result.VoiceMessage, Text: "not available: recording not received in time"},
		},
		{
			name:   "start recording refused",
			setup:  func(c *fakeCommands) { c.recordErr = errors.New("telnyx record_start: status 422") },
			events: func(h *runnerHarness) {},
			want:   Outcome{Code: result.VoiceMessage, Text: "not available: recording could not start: telnyx record_start: status 422"},
		},
		{
			name:  "download fails",
			setup: func(c *fakeCommands) { c.downloadErr = errors.New("status 404") },
			events: func(h *runnerHarness) {
				h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			},
			// D-3: the failed download no longer ends the check at once.
			fireWait: true,
			want:     Outcome{Code: result.VoiceMessage, Text: "not available: recording could not be downloaded: download recording of call v3:call-one: status 404"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			if tc.setup != nil {
				tc.setup(cmd)
			}
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.event(telnyx.EventCallAnswered, nil)
			h.detection("machine")
			tc.events(h)
			if tc.fireLimit {
				h.pending(t)
				h.fireLimit(t)
			}
			if tc.fireWait {
				h.pending(t)
				h.fireWait(t)
			}
			assertOutcome(t, h.outcome(t), tc.want)
			awaitHangup(t, cmd)
			_, hangups, _ := cmd.snapshot()
			if !reflect.DeepEqual(hangups, oneCall) {
				t.Errorf("Hangup calls = %q, want %q", hangups, oneCall)
			}
			if tc.file {
				if _, err := os.Stat(filepath.Join(h.dir, name)); err != nil {
					t.Errorf("recording file missing: %v", err)
				}
			} else {
				assertNoFiles(t, h.dir)
			}
		})
	}
}

// D-3: the text Telnyx already sent survives a failed recording download.
func TestTextKeptWhenDownloadFails(t *testing.T) {
	cmd := newFakeCommands()
	cmd.downloadErr = errors.New("status 404")
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	h.event(telnyx.EventCallAnswered, nil)
	h.detection("machine")
	h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = greeting })
	h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
	// No wait is fired: with the text in hand the failed download ends the check.
	assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting})
	assertCommands(t, cmd, oneCall, oneCall, []string{recordingLink})
	assertNoFiles(t, h.dir)
	select {
	case <-h.waitAsked:
		t.Error("runner asked for the text wait although the text had arrived")
	default:
	}
}

// D-3: text that arrived is kept when the recording itself fails afterwards.
func TestTextKeptWhenRecordingFails(t *testing.T) {
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	h.event(telnyx.EventCallAnswered, nil)
	h.detection("machine")
	h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = greeting })
	h.event(telnyx.EventRecordingError, nil)
	assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting})
	assertCommands(t, cmd, oneCall, oneCall, nil)
	assertNoFiles(t, h.dir)
}

// D-3, TC-36: a download that fails before the text arrived starts the text
// wait instead of ending the check.
func TestWaitsForTextAfterDownloadFailure(t *testing.T) {
	const (
		query = "X-Amz-Signature=secret-signature&X-Amz-Expires=600"
		link  = recordingLink + "?" + query
	)
	cases := []struct {
		name string
		text bool
		want Outcome
	}{
		{"text arrives inside the wait", true, Outcome{Code: result.VoiceMessage, Text: greeting}},
		// The whole reason is compared: the link's query is not in it (TC-35).
		{"wait ends without text", false, Outcome{
			Code: result.VoiceMessage,
			Text: `not available: recording could not be downloaded: download recording of call v3:call-one: get "` +
				recordingLink + `?[redacted]": status 403`,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			// A careless transport repeats the whole link, query included.
			cmd.downloadErr = fmt.Errorf("get %q: status 403", link)
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.event(telnyx.EventCallAnswered, nil)
			h.detection("machine")
			h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = link })
			h.awaitTextWait(t)
			h.pending(t)
			if tc.text {
				h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = greeting })
			} else {
				h.fireWait(t)
			}
			assertOutcome(t, h.outcome(t), tc.want)
			assertCommands(t, cmd, oneCall, oneCall, []string{link})
			assertNoFiles(t, h.dir)
		})
	}
}

// TC-36: Telnyx labels the text with its channel; only that leading label goes.
func TestChannelPrefixRemoved(t *testing.T) {
	const midText = "Press 1. Channel 0: sales."
	cases := []struct {
		name, text, want string
	}{
		{"prefix", "Channel 0: " + greeting, greeting},
		{"other channel number", "Channel 12: " + greeting, greeting},
		{"no prefix", greeting, greeting},
		{"not leading", midText, midText},
		{"no channel number", "Channel : " + greeting, "Channel : " + greeting},
		{"nothing after the prefix", "Channel 0: ", "not available: transcription was empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.event(telnyx.EventCallAnswered, nil)
			h.detection("machine")
			h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = tc.text })
			assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: tc.want, RecordingPath: runnerCallID + ".mp3"})
			awaitHangup(t, cmd)
		})
	}
}

// D-6, TC-37: Telnyx ends a call the app lost track of.
func TestDialSetsTimeLimit(t *testing.T) {
	cases := []struct {
		name  string
		limit time.Duration
		want  int
	}{
		{"whole seconds", runnerLimit, runnerTimeLimitSecs},
		{"fraction dropped", 90*time.Second + 500*time.Millisecond, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			// The dial is refused, so the call ends once the request was recorded.
			cmd.dialErr = &telnyx.APIError{Status: 503}
			runner := NewRunner(cmd, RunnerConfig{
				From:         runnerFrom,
				ConnectionID: runnerConnection,
				WebhookURL:   runnerWebhook,
				Limit:        tc.limit,
				After:        func(time.Duration) <-chan time.Time { return nil },
				Now:          func() time.Time { return runnerStart },
			})
			assertOutcome(t, runner.Call(context.Background(), runnerNumber), Outcome{Code: result.Unknown, Reason: "Telnyx error: status 503"})
			want := []telnyx.DialRequest{{
				To: runnerNumber, From: runnerFrom, ConnectionID: runnerConnection, WebhookURL: runnerWebhook,
				TimeLimitSecs: tc.want,
			}}
			cmd.mu.Lock()
			defer cmd.mu.Unlock()
			if !reflect.DeepEqual(cmd.dials, want) {
				t.Errorf("dial requests = %+v, want %+v", cmd.dials, want)
			}
		})
	}
}

// followedCall starts a call and returns once the runner follows it. h.start
// alone returns while the dial reply is still on its way; a recording is only
// started for a call that is registered.
func followedCall(t *testing.T, cmd *fakeCommands) *runnerHarness {
	t.Helper()
	recording := make(chan struct{})
	cmd.recordBlock = func(context.Context) error {
		close(recording)
		return nil
	}
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	h.event(telnyx.EventCallAnswered, nil)
	h.detection("machine")
	select {
	case <-recording:
	case <-time.After(runnerWait):
		t.Fatal("runner never started recording")
	}
	return h
}

// D-6: the app hangs up its call when it stops.
func TestShutdownHangsUp(t *testing.T) {
	recordingLate := Outcome{Code: result.VoiceMessage, Text: "not available: recording not received in time"}
	hangups := func(cmd *fakeCommands) []string {
		_, got, _ := cmd.snapshot()
		return got
	}

	t.Run("no call", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		if err := h.runner.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown = %v, want nil", err)
		}
		assertCommands(t, cmd, nil, nil, nil)
	})

	t.Run("call in progress", func(t *testing.T) {
		cmd := newFakeCommands()
		h := followedCall(t, cmd)
		if err := h.runner.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown = %v, want nil", err)
		}
		// Shutdown sends the hang-up itself, before it returns.
		if got := hangups(cmd); !reflect.DeepEqual(got, oneCall) {
			t.Fatalf("Hangup calls when Shutdown returned = %q, want %q", got, oneCall)
		}
		// Telnyx reports the hang-up; the call then ends without a second one.
		h.event(telnyx.EventCallHangup, func(ev *telnyx.Event) { ev.HangupCause = "normal_clearing" })
		h.fireLimit(t)
		assertOutcome(t, h.outcome(t), recordingLate)
		assertCommands(t, cmd, oneCall, oneCall, nil)
		// The call is over: a later Shutdown has nothing to hang up.
		if err := h.runner.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown after the call = %v, want nil", err)
		}
		if got := hangups(cmd); !reflect.DeepEqual(got, oneCall) {
			t.Errorf("Hangup calls after the call = %q, want %q", got, oneCall)
		}
	})

	t.Run("hang-up refused", func(t *testing.T) {
		refused := errors.New("status 422")
		cmd := newFakeCommands()
		cmd.hangupBlock = func(context.Context) error { return refused }
		h := followedCall(t, cmd)
		err := h.runner.Shutdown(context.Background())
		if !errors.Is(err, refused) || !strings.Contains(err.Error(), runnerCallID) {
			t.Errorf("Shutdown = %v, want an error that wraps %q and names call %s", err, refused, runnerCallID)
		}
		awaitHangup(t, cmd)
		// The call was not told of the refusal: at its limit it hangs up itself.
		h.fireLimit(t)
		assertOutcome(t, h.outcome(t), recordingLate)
		assertCommands(t, cmd, oneCall, []string{runnerCallID, runnerCallID}, nil)
	})

	t.Run("dial reply on its way", func(t *testing.T) {
		for _, name := range []string{"success", "hang-up refused", "cancelled"} {
			t.Run(name, func(t *testing.T) {
				cmd := newFakeCommands()
				h := newRunnerHarness(t, cmd, false)
				entered, release := make(chan struct{}), make(chan struct{})
				var releaseOnce sync.Once
				releaseDial := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseDial)
				cmd.onDial = func() { close(entered); <-release }
				refused := errors.New("status 422")
				cmd.hangupBlock = func(ctx context.Context) error {
					if name == "hang-up refused" {
						return refused
					}
					return ctx.Err()
				}
				go func() { h.done <- h.runner.Call(context.Background(), runnerNumber) }()
				select {
				case <-entered:
				case <-time.After(runnerWait):
					t.Fatal("dial did not start")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				observed := &shutdownWaitContext{Context: ctx, waiting: make(chan struct{})}
				done := make(chan error, 1)
				go func() { done <- h.runner.Shutdown(observed) }()
				// Done is observed only when Shutdown reaches its pending-dial
				// wait; this forces the race instead of measuring elapsed time.
				select {
				case <-observed.waiting:
				case err := <-done:
					t.Fatalf("Shutdown returned before the dial reply: %v", err)
				case <-time.After(runnerWait):
					t.Fatal("Shutdown did not wait for the dial reply")
				}
				if name == "cancelled" {
					cancel()
				} else {
					releaseDial()
				}
				var err error
				select {
				case err = <-done:
				case <-time.After(runnerWait):
					t.Fatal("Shutdown did not return")
				}
				switch name {
				case "success":
					if err != nil || !reflect.DeepEqual(hangups(cmd), oneCall) {
						t.Fatalf("Shutdown = %v, hangups = %q; want nil and %q before return", err, hangups(cmd), oneCall)
					}
				case "hang-up refused":
					if !errors.Is(err, refused) || !strings.Contains(err.Error(), runnerCallID) {
						t.Fatalf("Shutdown = %v, want dial hang-up refusal with call id", err)
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) || len(hangups(cmd)) != 0 {
						t.Fatalf("Shutdown = %v, hangups = %q; want cancellation before the reply", err, hangups(cmd))
					}
					releaseDial()
				}
				awaitHangup(t, cmd)
				h.fireLimit(t)
				assertOutcome(t, h.outcome(t), Outcome{Code: result.Unknown, Reason: "no call events received"})
				wantHangups := oneCall
				if name != "success" {
					wantHangups = []string{runnerCallID, runnerCallID}
					awaitHangup(t, cmd)
				}
				records, gotHangups, downloads := cmd.snapshot()
				if len(records) != 0 || len(downloads) != 0 || !reflect.DeepEqual(gotHangups, wantHangups) {
					t.Errorf("commands = %q, %q, %q; want no recording or download and hangups %q", records, gotHangups, downloads, wantHangups)
				}
			})
		}
	})
}

func TestHangupBeforeAnswer(t *testing.T) {
	cases := []struct {
		cause   string
		sip     string
		combine bool
		want    Outcome
	}{
		{"user_busy", "", false, Outcome{Code: result.Busy}},
		{"timeout", "", false, Outcome{Code: result.NoAnswer}},
		{"no_answer", "", false, Outcome{Code: result.NoAnswer}},
		{"not_found", "", false, Outcome{Code: result.Unallocated}},
		{"call_rejected", "", false, Outcome{Code: result.Disconnected}},
		{"not_found", "", true, Outcome{Code: result.DisconnectedUnallocated}},
		{"call_rejected", "", true, Outcome{Code: result.DisconnectedUnallocated}},
		{"user_busy", "", true, Outcome{Code: result.Busy}},
		{"normal_clearing", "", false, Outcome{Code: result.Unknown, Reason: "phone company message, not recorded"}},
		{"interworking", "", false, Outcome{Code: result.Unknown, Reason: "network ended the call: interworking"}},
		// D-8: the event's network code reaches the Unknown reason, and only that.
		{"unspecified", "480", false, Outcome{Code: result.Unknown, Reason: "phone company message, not recorded (network code 480)"}},
		{"no_answer", "480", false, Outcome{Code: result.NoAnswer}},
	}
	for _, tc := range cases {
		name := tc.cause
		if tc.sip != "" {
			name += " code " + tc.sip
		}
		if tc.combine {
			name += " combined"
		}
		t.Run(name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, tc.combine)
			h.start(t)
			h.event(telnyx.EventCallHangup, func(ev *telnyx.Event) {
				ev.HangupCause = tc.cause
				ev.SIPHangupCause = tc.sip
			})
			assertOutcome(t, h.outcome(t), tc.want)
			// The network already ended the call: no hang-up is sent.
			assertCommands(t, cmd, nil, nil, nil)
		})
	}
}

func TestDialError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"api error code", &telnyx.APIError{Status: 422, Code: "10010", Detail: "invalid to number"}, "Telnyx error: 10010"},
		{"api error without code", &telnyx.APIError{Status: 503}, "Telnyx error: status 503"},
		{"transport error", errors.New("telnyx dial +15552230101: connection refused"), "Telnyx error: telnyx dial +15552230101: connection refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			cmd.dialErr = tc.err
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			assertOutcome(t, h.outcome(t), Outcome{Code: result.Unknown, Reason: tc.reason})
			assertCommands(t, cmd, nil, nil, nil)
		})
	}
}

func TestTimeLimit(t *testing.T) {
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	if got := <-h.asked; got != runnerLimit {
		t.Errorf("time limit asked = %v, want %v", got, runnerLimit)
	}
	h.pending(t)
	h.fireLimit(t)
	assertOutcome(t, h.outcome(t), Outcome{Code: result.Unknown, Reason: "no call events received"})
	assertCommands(t, cmd, nil, oneCall, nil)
}

func TestEventsRouted(t *testing.T) {
	t.Run("other call id", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		h.start(t)
		// Sent first: if it reached this call, the result would be Fax.
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, CallControlID: runnerOtherID, DetectionResult: "fax_detected"})
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallHangup, CallControlID: runnerOtherID, HangupCause: "user_busy"})
		h.detection("silence")
		assertOutcome(t, h.outcome(t), Outcome{Code: result.PickedUp})
		assertCommands(t, cmd, nil, oneCall, nil)
	})

	t.Run("events before dial reply", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		cmd.onDial = func() {
			h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallHangup, CallControlID: runnerOtherID, HangupCause: "user_busy"})
			h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallHangup, CallControlID: runnerCallID, HangupCause: "not_found"})
		}
		h.start(t)
		assertOutcome(t, h.outcome(t), Outcome{Code: result.Unallocated})
	})

	t.Run("recording event without call id", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		h.start(t)
		h.event(telnyx.EventCallAnswered, nil)
		h.detection("machine")
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, RecordingURL: recordingLink})
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, CallControlID: runnerOtherID, TranscriptionText: "other call"})
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, TranscriptionText: greeting})
		assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerCallID + ".mp3"})
	})

	t.Run("events from another goroutine", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		h.start(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, CallControlID: runnerOtherID, DetectionResult: "fax_detected"})
			}()
		}
		wg.Wait()
		h.answered()
		h.detection("human_business")
		h.saved()
		h.transcribed(greeting)
		assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
		assertCommands(t, cmd, oneCall, oneCall, nil)
		h.runner.mu.Lock()
		defer h.runner.mu.Unlock()
		if len(h.runner.calls) != 0 || h.runner.dialing != 0 {
			t.Errorf("after the call: %d calls registered, %d dials in flight; want none", len(h.runner.calls), h.runner.dialing)
		}
	})
}

// Each stalled operation acknowledges entry before the controlled clock fires.
// Return is checked while cleanup is deliberately held, not inferred from speed.
func TestTimeLimitDuringCommands(t *testing.T) {
	for _, operation := range []string{"dial", "record", "download", "hangup"} {
		t.Run(operation, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			block := func(ctx context.Context) error {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			}
			cleanupEntered := make(chan error, 1)
			cleanupRelease := make(chan struct{})
			cleanupDone := make(chan struct{})
			defer close(cleanupRelease)
			cmd.hangupBlock = func(ctx context.Context) error {
				defer close(cleanupDone)
				cleanupEntered <- ctx.Err()
				if operation == "download" {
					return nil
				}
				if operation == "hangup" {
					close(entered)
				}
				select {
				case <-cleanupRelease:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			switch operation {
			case "dial":
				cmd.dialBlock = block
			case "record":
				cmd.recordBlock = block
			case "download":
				cmd.downloadBlock = block
			}
			h.start(t)
			switch operation {
			case "record", "download":
				h.event(telnyx.EventCallAnswered, nil)
				h.detection("machine")
				if operation == "download" {
					h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
					// The download waits until the result is decided (TC-57).
					h.fireWait(t)
				}
			case "hangup":
				h.detection("fax_detected")
			}
			select {
			case <-entered:
			case <-time.After(runnerWait):
				t.Fatal("operation never started")
			}
			h.fireLimit(t)
			out := h.outcome(t)
			switch operation {
			case "dial":
				assertOutcome(t, out, Outcome{Code: result.Unknown, Reason: "no call events received"})
			case "record":
				assertOutcome(t, out, Outcome{Code: result.VoiceMessage, Text: "not available: recording could not start: context canceled"})
			case "download":
				assertOutcome(t, out, Outcome{Code: result.VoiceMessage, Text: "not available: recording could not be downloaded: download recording of call v3:call-one: context canceled"})
			case "hangup":
				assertOutcome(t, out, Outcome{Code: result.Fax})
			}
			if operation != "dial" {
				select {
				case err := <-cleanupEntered:
					if err != nil {
						t.Fatalf("cleanup got cancelled context: %v", err)
					}
				case <-time.After(runnerWait):
					t.Fatal("cleanup never started")
				}
				if operation != "download" {
					select {
					case <-cleanupDone:
						t.Fatal("cleanup was not held through result delivery")
					default:
					}
				}
			} else {
				assertCommands(t, cmd, nil, nil, nil)
			}
			if operation != "hangup" {
				select {
				case <-cancelled:
				case <-time.After(runnerWait):
					t.Fatal("operation was not cancelled")
				}
			}
			assertNoFiles(t, h.dir)
			h.runner.mu.Lock()
			defer h.runner.mu.Unlock()
			if len(h.runner.calls) != 0 || h.runner.dialing != 0 {
				t.Fatal("expired call still registered")
			}
		})
	}
}

// shutdownHangupKey marks the context given to Shutdown, so the fake can tell
// the hang-up sent for Shutdown from the one the call sends at its limit.
type shutdownHangupKey struct{}

// The check limit ends every wait, also the one for the hang-up of a call
// whose dial reply arrived during Shutdown (D-6). The order is forced: that
// hang-up acknowledges entry and is held across the limit; Call must return
// while it is held, and Shutdown must go on waiting for it within its own
// context.
func TestTimeLimitDuringShutdownHangup(t *testing.T) {
	refused := errors.New("status 422")
	cases := []struct {
		name string
		// The held hang-up ends with reply, or by cancelling Shutdown's context.
		reply   error
		cancel  bool
		wantErr error
	}{
		{name: "hang-up succeeds"},
		{name: "hang-up refused", reply: refused, wantErr: refused},
		{name: "shutdown cancelled", cancel: true, wantErr: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			dialEntered, dialRelease := make(chan struct{}), make(chan struct{})
			var dialOnce sync.Once
			releaseDial := func() { dialOnce.Do(func() { close(dialRelease) }) }
			t.Cleanup(releaseDial)
			cmd.onDial = func() { close(dialEntered); <-dialRelease }
			cleanupEntered := make(chan struct{})
			cleanupRelease := make(chan struct{})
			cleanupDone := make(chan struct{})
			cmd.hangupBlock = func(ctx context.Context) error {
				if ctx.Value(shutdownHangupKey{}) == nil {
					// The call's own hang-up at its limit.
					return nil
				}
				defer close(cleanupDone)
				close(cleanupEntered)
				select {
				case <-cleanupRelease:
					return tc.reply
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			go func() { h.done <- h.runner.Call(context.Background(), runnerNumber) }()
			select {
			case <-dialEntered:
			case <-time.After(runnerWait):
				t.Fatal("dial did not start")
			}
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), shutdownHangupKey{}, true))
			defer cancel()
			observed := &shutdownWaitContext{Context: ctx, waiting: make(chan struct{})}
			shutdownDone := make(chan error, 1)
			go func() { shutdownDone <- h.runner.Shutdown(observed) }()
			select {
			case <-observed.waiting:
			case <-time.After(runnerWait):
				t.Fatal("Shutdown did not wait for the dial reply")
			}
			releaseDial()
			select {
			case <-cleanupEntered:
			case <-time.After(runnerWait):
				t.Fatal("hang-up for Shutdown never started")
			}
			h.fireLimit(t)
			assertOutcome(t, h.outcome(t), Outcome{Code: result.Unknown, Reason: "no call events received"})
			select {
			case <-cleanupDone:
				t.Fatal("hang-up for Shutdown was not held through the call's return")
			case err := <-shutdownDone:
				t.Fatalf("Shutdown returned %v while its hang-up was held", err)
			default:
			}
			if tc.cancel {
				cancel()
			} else {
				close(cleanupRelease)
			}
			select {
			case err := <-shutdownDone:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("Shutdown = %v, want %v", err, tc.wantErr)
				}
			case <-time.After(runnerWait):
				t.Fatal("Shutdown did not return")
			}
			select {
			case <-cleanupDone:
			case <-time.After(runnerWait):
				t.Fatal("hang-up for Shutdown never ended")
			}
			// The held hang-up could not be counted at the limit, so the call
			// sent its own as well.
			awaitHangup(t, cmd)
			assertCommands(t, cmd, nil, []string{runnerCallID, runnerCallID}, nil)
		})
	}
}

func TestEventsRoutedEarlyRecording(t *testing.T) {
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	cmd.onDial = func() {
		h.event(telnyx.EventCallAnswered, nil)
		h.detection("machine")
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, RecordingURL: recordingLink})
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, TranscriptionText: greeting})
	}
	h.start(t)
	assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerCallID + ".mp3"})
	assertCommands(t, cmd, oneCall, oneCall, []string{recordingLink})
	data, err := os.ReadFile(filepath.Join(h.dir, runnerCallID+".mp3"))
	if err != nil || string(data) != recordingBytes {
		t.Fatalf("recording = %q, %v", data, err)
	}
}

func TestEventsRoutedRequiresIDForDetection(t *testing.T) {
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	cmd.onDial = func() {
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, DetectionResult: "machine"})
		h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallHangup, HangupCause: "user_busy"})
	}
	h.start(t)
	// Detection with an id is queued after the malformed events, forcing the discriminator.
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, DetectionResult: "machine"})
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallHangup, HangupCause: "user_busy"})
	h.detection("silence")
	assertOutcome(t, h.outcome(t), Outcome{Code: result.PickedUp})
	assertCommands(t, cmd, nil, oneCall, nil)
	assertNoFiles(t, h.dir)
}

func TestVoiceMessageRejectsPreviousTranscription(t *testing.T) {
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	<-h.asked
	h.event(telnyx.EventCallAnswered, nil)
	h.detection("machine")
	h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
	h.fireLimit(t)
	assertOutcome(t, h.outcome(t), Outcome{
		Code: result.VoiceMessage, Text: "not available: transcription not received in time",
		RecordingPath: runnerCallID + ".mp3",
	})
	awaitHangup(t, cmd)

	// The previous call is finished before another call starts (AD-2), but
	// Telnyx can still deliver its late transcription after the new call starts.
	cmd.id = runnerOtherID
	h.start(t)
	<-h.asked
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallAnswered, CallControlID: runnerOtherID})
	h.runner.HandleEvent(telnyx.Event{
		Type: telnyx.EventMachinePremiumDetectionEnded, CallControlID: runnerOtherID, DetectionResult: "machine",
	})
	h.runner.HandleEvent(telnyx.Event{
		Type: telnyx.EventTranscriptionSaved, TranscriptionText: "previous call's late message",
		OccurredAt: runnerStart.Add(time.Second),
	})
	h.runner.HandleEvent(telnyx.Event{
		Type: telnyx.EventRecordingSaved, CallControlID: runnerOtherID, RecordingURL: recordingLink,
	})
	h.runner.HandleEvent(telnyx.Event{
		Type: telnyx.EventTranscriptionSaved, CallControlID: runnerOtherID, TranscriptionText: greeting,
	})
	assertOutcome(t, h.outcome(t), Outcome{
		Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerOtherID + ".mp3",
	})
}

// Telnyx sends call.recording.saved without call_control_id; its leg and
// session ids name the call. A finished call's late events, matched by those
// ids, must not reach the next call, which still gets its own (F-4).
func TestEventsRoutedByLegID(t *testing.T) {
	const (
		legOne, sessionOne = "leg-one", "session-one"
		legTwo, sessionTwo = "leg-two", "session-two"
		otherLink          = "https://files.example.test/other.mp3"
	)
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	<-h.asked
	h.event(telnyx.EventCallAnswered, func(ev *telnyx.Event) { ev.CallLegID, ev.CallSessionID = legOne, sessionOne })
	h.detection("machine")
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, CallLegID: legOne, CallSessionID: sessionOne, RecordingURL: recordingLink})
	h.fireLimit(t)
	assertOutcome(t, h.outcome(t), Outcome{
		Code: result.VoiceMessage, Text: "not available: transcription not received in time",
		RecordingPath: runnerCallID + ".mp3",
	})
	awaitHangup(t, cmd)

	cmd.id = runnerOtherID
	h.start(t)
	<-h.asked
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallAnswered, CallControlID: runnerOtherID, CallLegID: legTwo, CallSessionID: sessionTwo})
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, CallControlID: runnerOtherID, DetectionResult: "machine"})
	// The first call's late events, by leg id and by session id only.
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, CallLegID: legOne, TranscriptionText: "previous call's late message"})
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, CallSessionID: sessionOne, RecordingURL: otherLink})
	h.pending(t)
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, CallLegID: legTwo, CallSessionID: sessionTwo, RecordingURL: recordingLink})
	h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, CallSessionID: sessionTwo, TranscriptionText: greeting})
	assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerOtherID + ".mp3"})
	awaitHangup(t, cmd)
	records, _, downloads := cmd.snapshot()
	if want := []string{runnerCallID, runnerOtherID}; !reflect.DeepEqual(records, want) {
		t.Errorf("StartRecording calls = %q, want %q", records, want)
	}
	if want := []string{recordingLink, recordingLink}; !reflect.DeepEqual(downloads, want) {
		t.Errorf("Download calls = %q, want %q", downloads, want)
	}
	// Both calls are complete: nothing is kept for either of them.
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	if len(h.runner.calls) != 0 || len(h.runner.late) != 0 || len(h.runner.owners) != 0 {
		t.Errorf("after both calls: %d calls, %d late, %d owner ids; want none", len(h.runner.calls), len(h.runner.late), len(h.runner.owners))
	}
}

func TestVoiceMessageRejectsCompletedCallRedelivery(t *testing.T) {
	for _, early := range []bool{false, true} {
		name := "after dial reply"
		if early {
			name = "before dial reply"
		}
		t.Run(name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			<-h.asked
			h.event(telnyx.EventCallAnswered, nil)
			h.detection("machine")
			h.event(telnyx.EventRecordingSaved, func(ev *telnyx.Event) { ev.RecordingURL = recordingLink })
			h.event(telnyx.EventTranscriptionSaved, func(ev *telnyx.Event) { ev.TranscriptionText = greeting })
			assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerCallID + ".mp3"})
			awaitHangup(t, cmd)
			h.runner.mu.Lock()
			late := len(h.runner.late)
			h.runner.mu.Unlock()
			if late != 0 {
				t.Fatalf("previous call still late: %d", late)
			}

			cmd.id = runnerOtherID
			feed := func() {
				const leg = "current-call-leg"
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventCallAnswered, CallControlID: runnerOtherID, CallLegID: leg})
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventMachinePremiumDetectionEnded, CallControlID: runnerOtherID, CallLegID: leg, DetectionResult: "machine"})
				// Redelivery is possible after the first call has both artifacts.
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, TranscriptionText: "previous call's duplicate"})
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, RecordingURL: "https://files.example.test/previous.mp3"})
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingError})
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventRecordingSaved, CallLegID: leg, RecordingURL: recordingLink})
				h.runner.HandleEvent(telnyx.Event{Type: telnyx.EventTranscriptionSaved, CallLegID: leg, TranscriptionText: greeting})
			}
			if early {
				cmd.onDial = feed
			}
			h.start(t)
			<-h.asked
			if !early {
				feed()
			}
			assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: greeting, RecordingPath: runnerOtherID + ".mp3"})
			awaitHangup(t, cmd)
			records, _, downloads := cmd.snapshot()
			if want := []string{runnerCallID, runnerOtherID}; !reflect.DeepEqual(records, want) {
				t.Errorf("StartRecording calls = %q, want %q", records, want)
			}
			if want := []string{recordingLink, recordingLink}; !reflect.DeepEqual(downloads, want) {
				t.Errorf("Download calls = %q, want %q", downloads, want)
			}
			data, err := os.ReadFile(filepath.Join(h.dir, runnerOtherID+".mp3"))
			if err != nil || string(data) != recordingBytes {
				t.Fatalf("current recording = %q, %v; want %q", data, err, recordingBytes)
			}
		})
	}
}

// step is one event a test sends, in the order the test fixes.
type step func(*runnerHarness)

func detected(value string) step { return func(h *runnerHarness) { h.detection(value) } }

func transcript(text string) step { return func(h *runnerHarness) { h.transcribed(text) } }

var (
	answer     step = (*runnerHarness).answered
	saveRec    step = (*runnerHarness).saved
	farHangup  step = (*runnerHarness).farHangup
	recFailure step = func(h *runnerHarness) { h.event(telnyx.EventRecordingError, nil) }
)

// D-11 (R-33, R-34): the line answers, plays its message and hangs up before
// detection gives a result.
func TestDisconnectedMessage(t *testing.T) {
	logged := captureLog(t)
	cmd := newFakeCommands()
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	h.answered()
	h.farHangup()
	h.saved()
	h.transcribed(disconnectedWords)
	name := runnerCallID + ".mp3"
	assertOutcome(t, h.outcome(t), Outcome{Code: result.Disconnected, Text: disconnectedWords, RecordingPath: name})
	// The far end ended the call, so no hang-up is sent; the recording is kept.
	assertCommands(t, cmd, oneCall, nil, []string{recordingLink})
	assertDeletes(t, cmd, nil)
	data, err := os.ReadFile(filepath.Join(h.dir, name))
	if err != nil || string(data) != recordingBytes {
		t.Fatalf("recording = %q, %v; want %q", data, err, recordingBytes)
	}
	assertLog(t, logged, nil)
}

// R-33, TC-53: the record command goes out when the answer is reported, before
// detection; a machine whose words hold no phrase stays Voice message with its
// text (R-36, R-13).
func TestRecordOnAnswer(t *testing.T) {
	const noPhrase = "Hi, this is Sam. Talk soon."
	cmd := newFakeCommands()
	recording := make(chan struct{})
	cmd.recordBlock = func(context.Context) error {
		close(recording)
		return nil
	}
	h := newRunnerHarness(t, cmd, false)
	h.start(t)
	h.answered()
	select {
	case <-recording:
	case <-time.After(runnerWait):
		t.Fatal("no record command after the answer")
	}
	// Nothing but the answer has been sent: no detection started the recording.
	if records, hangups, _ := cmd.snapshot(); !reflect.DeepEqual(records, oneCall) || len(hangups) != 0 {
		t.Fatalf("after the answer: StartRecording = %q, Hangup = %q; want %q and none", records, hangups, oneCall)
	}
	h.detection("machine")
	h.saved()
	h.transcribed(noPhrase)
	assertOutcome(t, h.outcome(t), Outcome{Code: result.VoiceMessage, Text: noPhrase, RecordingPath: runnerCallID + ".mp3"})
	assertCommands(t, cmd, oneCall, oneCall, []string{recordingLink})
	assertDeletes(t, cmd, nil)
}

// TC-55, TC-61: the result does not depend on whether the text or detection
// comes first; after the hang-up a detection result that arrives before the
// decision is applied, and one after the result cannot change it.
func TestEventOrders(t *testing.T) {
	const notAvailable = "The person you are calling is not available, please leave a message after the tone"
	name := runnerCallID + ".mp3"
	kept := func(code result.Code, text string) Outcome {
		return Outcome{Code: code, Text: text, RecordingPath: name}
	}
	cases := []struct {
		name    string
		steps   []step
		want    Outcome
		hangups []string
		deletes []telnyx.RecordingRef
	}{
		{"greeting, text after detection", []step{answer, detected("machine"), saveRec, transcript(notAvailable)},
			kept(result.VoiceMessage, notAvailable), oneCall, nil},
		{"greeting, text before detection", []step{answer, saveRec, transcript(notAvailable), detected("machine")},
			kept(result.VoiceMessage, notAvailable), oneCall, nil},
		{"number gone, text after detection", []step{answer, detected("machine"), saveRec, transcript(disconnectedWords)},
			kept(result.Disconnected, disconnectedWords), oneCall, nil},
		{"number gone, text before detection", []step{answer, saveRec, transcript(disconnectedWords), detected("machine")},
			kept(result.Disconnected, disconnectedWords), oneCall, nil},
		{"fax after the hang-up", []step{answer, farHangup, detected("fax_detected"), saveRec, transcript("Goodbye")},
			Outcome{Code: result.Fax}, nil, []telnyx.RecordingRef{runnerRef}},
		{"machine after the hang-up", []step{answer, farHangup, detected("machine"), saveRec, transcript("Goodbye")},
			kept(result.VoiceMessage, "Goodbye"), nil, nil},
		{"person after the hang-up", []step{answer, farHangup, detected("human_residence"), saveRec, transcript(disconnectedWords)},
			Outcome{Code: result.PersonAnswered}, nil, []telnyx.RecordingRef{runnerRef}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			for _, s := range tc.steps {
				s(h)
			}
			assertOutcome(t, h.outcome(t), tc.want)
			var downloads []string
			if tc.want.RecordingPath != "" {
				downloads = []string{recordingLink}
			} else {
				assertNoFiles(t, h.dir)
			}
			assertCommands(t, cmd, oneCall, tc.hangups, downloads)
			assertDeletes(t, cmd, tc.deletes)
		})
	}

	t.Run("machine detection delivered before answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.detection("machine")
			synctest.Wait()
			h.pending(t)
			assertCommands(t, cmd, nil, nil, nil)

			h.answered()
			h.saved()
			h.transcribed(disconnectedWords)
			assertOutcome(t, h.outcome(t), kept(result.Disconnected, disconnectedWords))
			assertCommands(t, cmd, oneCall, oneCall, []string{recordingLink})
		})
	})

	t.Run("recording queued when hang-up wait expires", func(t *testing.T) {
		cmd := newFakeCommands()
		h := newRunnerHarness(t, cmd, false)
		f := callFlow{runner: h.runner, ctx: context.Background(), id: runnerCallID}
		f.handle(telnyx.Event{Type: telnyx.EventCallAnswered, CallLegID: runnerLeg})
		f.handle(telnyx.Event{Type: telnyx.EventCallHangup})

		// Model follow selecting the timer just before draining a queued save.
		f.hangupWait, f.hangupLate = nil, true
		f.handle(telnyx.Event{Type: telnyx.EventRecordingSaved, RecordingURL: recordingLink})
		if f.ready() {
			t.Fatal("hang-up timer ended the check before the saved recording's text wait")
		}
		f.handle(telnyx.Event{Type: telnyx.EventTranscriptionSaved, TranscriptionText: disconnectedWords})
		if !f.ready() {
			t.Fatal("the check did not decide once the recording and its text arrived")
		}
		got := f.decide()
		got.StartedAt = runnerStart
		assertOutcome(t, got, kept(result.Disconnected, disconnectedWords))
		assertCommands(t, cmd, oneCall, nil, []string{recordingLink})
		assertDeletes(t, cmd, nil)
	})

	t.Run("known text waits for detection or far-end hang-up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.saved()
			h.transcribed(disconnectedWords)
			synctest.Wait()
			h.pending(t)
			assertCommands(t, cmd, oneCall, nil, nil)

			h.detection("human_residence")
			assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, []telnyx.RecordingRef{runnerRef})
		})
	})

	t.Run("detection after the result", func(t *testing.T) {
		cmd := newFakeCommands()
		downloading, release := make(chan struct{}), make(chan struct{})
		// The download runs once the result is decided; it is held so the
		// detection is queued before Call returns.
		cmd.downloadBlock = func(context.Context) error {
			close(downloading)
			<-release
			return nil
		}
		h := newRunnerHarness(t, cmd, false)
		h.start(t)
		for _, s := range []step{answer, farHangup, saveRec, transcript(disconnectedWords)} {
			s(h)
		}
		select {
		case <-downloading:
		case <-time.After(runnerWait):
			t.Fatal("the decided result was never downloaded")
		}
		// A person applied now would mean no download and a delete.
		h.detection("human_residence")
		close(release)
		assertOutcome(t, h.outcome(t), kept(result.Disconnected, disconnectedWords))
		h.detection("human_residence")
		assertCommands(t, cmd, oneCall, nil, []string{recordingLink})
		assertDeletes(t, cmd, nil)
	})
}

// R-34 to R-36, R-23, TC-54, TC-56: after a saved recording, matching words
// decide for machine, silence, fax and not-sure; a recording error leaves
// detection's result (TC-55).
func TestWordsDecide(t *testing.T) {
	const (
		greetingWords = "Please leave a message after the tone"
		carrierWords  = "Your call cannot be completed as dialed"
	)
	name := runnerCallID + ".mp3"
	phrases := []struct {
		name    string
		text    string
		combine bool
		want    Outcome
	}{
		{"number gone", disconnectedWords, false, Outcome{Code: result.Disconnected}},
		{"number gone combined", disconnectedWords, true, Outcome{Code: result.DisconnectedUnallocated}},
		{"greeting", greetingWords, false, Outcome{Code: result.VoiceMessage}},
		{"phone company", carrierWords, false, Outcome{Code: result.Unknown, Reason: "phone company message, see text"}},
	}
	for _, detection := range []string{"machine", "silence", "fax_detected", "not_sure"} {
		for _, p := range phrases {
			t.Run(detection+" "+p.name, func(t *testing.T) {
				cmd := newFakeCommands()
				h := newRunnerHarness(t, cmd, p.combine)
				h.start(t)
				h.answered()
				h.detection(detection)
				h.saved()
				// Only the words after Telnyx's channel label are read (S-1).
				h.transcribed("Channel 0: " + p.text)
				want := p.want
				want.Text, want.RecordingPath = p.text, name
				assertOutcome(t, h.outcome(t), want)
				assertCommands(t, cmd, oneCall, oneCall, []string{recordingLink})
				assertDeletes(t, cmd, nil)
			})
		}
	}

	errorCases := []struct {
		detection string
		want      Outcome
		deletes   []telnyx.RecordingRef
	}{
		{"fax_detected", Outcome{Code: result.Fax}, []telnyx.RecordingRef{runnerRef}},
		// D-3: the text of a voice message is kept, but its words do not decide.
		{"machine", Outcome{Code: result.VoiceMessage, Text: disconnectedWords}, nil},
	}
	for _, tc := range errorCases {
		t.Run(tc.detection+" recording error", func(t *testing.T) {
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.detection(tc.detection)
			h.transcribed(disconnectedWords)
			h.event(telnyx.EventRecordingError, nil)
			assertOutcome(t, h.outcome(t), tc.want)
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, tc.deletes)
			assertNoFiles(t, h.dir)
		})
	}
}

// R-37, C-1, TC-57: a person's seconds are never downloaded, shown or logged,
// and are deleted at Telnyx; without a recording event in the wait one fixed
// line is logged and the result is the same.
func TestPersonDeleted(t *testing.T) {
	cases := []struct {
		name      string
		detection string
		deleteErr error
		after     func(*testing.T, *runnerHarness)
		deletes   []telnyx.RecordingRef
		log       []string
	}{
		{
			name: "deleted at Telnyx", detection: "human_residence",
			after: func(_ *testing.T, h *runnerHarness) {
				h.saved()
				h.transcribed(disconnectedWords)
			},
			deletes: []telnyx.RecordingRef{runnerRef},
		},
		{
			name: "text wait ends", detection: "human_business",
			after: func(t *testing.T, h *runnerHarness) {
				h.saved()
				h.fireWait(t)
			},
			// The recording-saved event carries no recording id; the leg finds it (TC-58).
			deletes: []telnyx.RecordingRef{{CallLegID: runnerLeg}},
		},
		{
			name: "no recording event in the wait", detection: "human_residence",
			after:   func(t *testing.T, h *runnerHarness) { h.fireHangupWait(t) },
			deletes: nil,
			log:     []string{notDeletedLine},
		},
		{
			// TO-0002:ADV-1: the error can carry Telnyx's reply; only the fixed line is logged.
			name: "delete refused", detection: "human_residence",
			deleteErr: errors.New("telnyx delete recording rec-call-one: status 500: " + disconnectedWords),
			after: func(_ *testing.T, h *runnerHarness) {
				h.saved()
				h.transcribed(disconnectedWords)
			},
			deletes: []telnyx.RecordingRef{runnerRef},
			log:     []string{notDeletedLine},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLog(t)
			cmd := newFakeCommands()
			cmd.deleteErr = tc.deleteErr
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.detection(tc.detection)
			// The app hangs up as today and waits for the recording.
			awaitHangup(t, cmd)
			h.pending(t)
			tc.after(t, h)
			assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
			records, hangups, downloads := cmd.snapshot()
			if !reflect.DeepEqual(records, oneCall) || !reflect.DeepEqual(hangups, oneCall) || len(downloads) != 0 {
				t.Errorf("StartRecording = %q, Hangup = %q, Download = %q; want one, one, none", records, hangups, downloads)
			}
			assertDeletes(t, cmd, tc.deletes)
			assertNoFiles(t, h.dir)
			assertLog(t, logged, tc.log)
		})
	}

	t.Run("deletion finishes before return at the check limit", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			cmd := newFakeCommands()
			started, release := make(chan error, 1), make(chan struct{})
			cmd.deleteBlock = func(ctx context.Context) error {
				started <- ctx.Err()
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			h.detection("human_residence")
			h.saved()
			synctest.Wait()
			h.fireLimit(t)
			select {
			case err := <-started:
				if err != nil {
					t.Errorf("DeleteRecording started with a cancelled context: %v", err)
				}
			case <-time.After(runnerWait):
				t.Fatal("DeleteRecording never started")
			}
			synctest.Wait()
			select {
			case out := <-h.done:
				close(release)
				t.Fatalf("call returned before deletion finished: %+v", out)
			default:
				close(release)
			}
			assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, []telnyx.RecordingRef{{CallLegID: runnerLeg}})
			assertNoFiles(t, h.dir)
		})
	})
}

// R-38, R-37, TC-57: silence, fax, not sure or no detection with words that
// hold no phrase give today's result; nothing is downloaded or kept, and the
// recording is deleted at Telnyx.
func TestUnrecognisedDeleted(t *testing.T) {
	const noPhrase = "Hello? Hello? Goodbye"
	couldNotTell := Outcome{Code: result.Unknown, Reason: "Telnyx could not tell what answered"}
	cases := []struct {
		name    string
		steps   []step
		want    Outcome
		hangups []string
	}{
		{"silence", []step{answer, detected("silence"), saveRec, transcript(noPhrase)}, Outcome{Code: result.PickedUp}, oneCall},
		{"fax", []step{answer, detected("fax_detected"), saveRec, transcript(noPhrase)}, Outcome{Code: result.Fax}, oneCall},
		{"not sure", []step{answer, detected("not_sure"), saveRec, transcript(noPhrase)}, couldNotTell, oneCall},
		{"no detection", []step{answer, farHangup, saveRec, transcript(noPhrase)}, couldNotTell, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLog(t)
			cmd := newFakeCommands()
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			for _, s := range tc.steps {
				s(h)
			}
			assertOutcome(t, h.outcome(t), tc.want)
			assertCommands(t, cmd, oneCall, tc.hangups, nil)
			assertDeletes(t, cmd, []telnyx.RecordingRef{runnerRef})
			assertNoFiles(t, h.dir)
			assertLog(t, logged, nil)
		})
	}
}

// TC-53: a failed record command is logged; the call goes on and detection
// decides as today, with no word check and nothing to delete.
func TestRecordCommandFails(t *testing.T) {
	const failureLog = "check: start recording of call " + runnerCallID + " failed"
	refusal := errors.New("telnyx record_start: status 422")
	privateRefusal := &telnyx.APIError{Status: 422, Detail: disconnectedWords + " " + recordingLink}
	cases := []struct {
		name      string
		detection string
		recordErr error
		want      Outcome
		log       string
	}{
		{"silence", "silence", refusal, Outcome{Code: result.PickedUp}, failureLog},
		{"machine", "machine", refusal, Outcome{Code: result.VoiceMessage, Text: "not available: recording could not start: " + refusal.Error()}, failureLog},
		{"person", "human_residence", privateRefusal, Outcome{Code: result.PersonAnswered}, failureLog + ": status 422"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logged := captureLog(t)
			cmd := newFakeCommands()
			cmd.recordErr = tc.recordErr
			h := newRunnerHarness(t, cmd, false)
			h.start(t)
			h.answered()
			// Words that would read Disconnected if they were checked.
			h.transcribed(disconnectedWords)
			h.detection(tc.detection)
			assertOutcome(t, h.outcome(t), tc.want)
			assertCommands(t, cmd, oneCall, oneCall, nil)
			assertDeletes(t, cmd, nil)
			assertNoFiles(t, h.dir)
			assertLog(t, logged, []string{tc.log})
		})
	}

	t.Run("detection queued after hang-up", func(t *testing.T) {
		logged := captureLog(t)
		cmd := newFakeCommands()
		cmd.recordErr = refusal
		h := newRunnerHarness(t, cmd, false)
		cmd.onDial = func() {
			h.answered()
			h.farHangup()
			h.detection("human_residence")
		}
		h.start(t)
		assertOutcome(t, h.outcome(t), Outcome{Code: result.PersonAnswered})
		assertCommands(t, cmd, oneCall, nil, nil)
		assertDeletes(t, cmd, nil)
		assertLog(t, logged, []string{failureLog})
	})
}
