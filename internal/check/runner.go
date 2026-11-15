package check

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"checknumber/internal/result"
	"checknumber/internal/telnyx"
)

// recordingExt matches the mp3 format the client asks Telnyx for (TO-0004).
const recordingExt = ".mp3"

// Reasons and texts the runner gives (R-20, TC-19, TC-21).
const (
	reasonTelnyxError     = "Telnyx error: "
	reasonNoEvents        = "no call events received"
	textNotAvailable      = "not available: "
	reasonRecordingFailed = "recording failed"
	reasonRecordingStart  = "recording could not start: "
	reasonDownloadFailed  = "recording could not be downloaded: "
	reasonRecordingLate   = "recording not received in time"
	reasonTextLate        = "transcription not received in time"
	reasonTextEmpty       = "transcription was empty"
	// queryRemoved stands where a recording link's query was (TC-35).
	queryRemoved = "[redacted]"
)

const (
	// TC-36: Telnyx sent the text about 2 seconds after the recording.
	textWaitLimit = 15 * time.Second
	// TC-37: the check limit ends a call the runner follows. The margin lets
	// Telnyx end a call the app lost track of (D-6) without cutting short one
	// that is still followed.
	callLimitExtraSecs = 30
)

// channelPrefix is the channel label Telnyx puts in front of the text (TC-36).
var channelPrefix = regexp.MustCompile(`^Channel [0-9]+: `)

// Commands is the part of the Telnyx client the runner uses. *telnyx.Client
// satisfies it; tests use a fake so no real call is placed (C-5).
type Commands interface {
	Dial(ctx context.Context, req telnyx.DialRequest) (string, error)
	StartRecording(ctx context.Context, id string) error
	Hangup(ctx context.Context, id string) error
	Download(ctx context.Context, url string, w io.Writer) error
}

var (
	_ Commands = (*telnyx.Client)(nil)
	_ Dialer   = (*Runner)(nil)
)

// RunnerConfig holds the runner's settings. Limit is the time limit of one
// call (TC-18). After and Now are the clock; nil means time.After and time.Now.
type RunnerConfig struct {
	From          string
	ConnectionID  string
	WebhookURL    string
	RecordingsDir string
	Limit         time.Duration
	Combine       bool
	After         func(time.Duration) <-chan time.Time
	Now           func() time.Time
}

// Runner places calls through Telnyx and turns their events into one Outcome
// per call. It implements Dialer.
type Runner struct {
	cmd Commands
	cfg RunnerConfig

	mu    sync.Mutex
	calls map[string]*callEvents
	// owners maps the leg and session ids (identityKeys) of calls in calls
	// and late to their call id. Telnyx omits call_control_id from
	// call.recording.saved, so those ids are how such events find their call.
	owners map[string]string
	// late holds finished calls that Telnyx may still send recording events
	// for. Such an event must not reach a later call (F-4).
	late map[string]*lateCall
	// A completed recording can be redelivered even after every expected event
	// arrived. Later calls therefore need identity on their recording events.
	recordingHistory bool
	// dialing counts Dial requests still waiting for their call id. Events
	// that arrive meanwhile are kept in early, because Telnyx can report a
	// quick failure before the dial reply reaches us.
	dialing int
	early   []telnyx.Event
	// pending lets Shutdown wait for replies without losing a call whose id
	// is not known yet (D-6). Access is protected by mu until endDial.
	pending map[*dialFlight]struct{}
}

// done publishes the hang-up result to Shutdown after the dial reply arrives.
type dialFlight struct {
	shutdown context.Context
	done     chan struct{}
	err      error
}

// callEvents is the event queue of one call. HandleEvent never blocks: it
// appends under Runner.mu and signals wake, which holds at most one token.
type callEvents struct {
	queue []telnyx.Event
	wake  chan struct{}
	// keys are the owners entries learned for this call.
	keys []string
}

// lateCall is a finished call whose recording or transcription has not
// arrived yet.
type lateCall struct {
	keys            []string
	recording, text bool
}

// note marks the recording event ev as arrived.
func (l *lateCall) note(ev telnyx.Event) {
	switch ev.Type {
	case telnyx.EventRecordingSaved:
		l.recording = false
	case telnyx.EventRecordingError:
		// No transcription follows a failed recording.
		l.recording, l.text = false, false
	case telnyx.EventTranscriptionSaved:
		l.text = false
	}
}

// NewRunner returns a Runner that sends commands through cmd.
func NewRunner(cmd Commands, cfg RunnerConfig) *Runner {
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Runner{
		cmd:     cmd,
		cfg:     cfg,
		calls:   map[string]*callEvents{},
		owners:  map[string]string{},
		late:    map[string]*lateCall{},
		pending: map[*dialFlight]struct{}{},
	}
}

// HandleEvent passes ev to the call it belongs to. Events for a call id the
// runner does not know are dropped.
func (r *Runner) HandleEvent(ev telnyx.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id := r.owner(ev); id != "" {
		if c, ok := r.calls[id]; ok {
			r.learn(id, c, ev)
			c.push(ev)
			return
		}
		if l, ok := r.late[id]; ok {
			// A finished call's event: it never reaches the call in progress.
			r.lateArrived(id, l, ev)
			return
		}
	}
	switch {
	case r.unidentified(ev) && r.soleOwner():
		for _, c := range r.calls {
			c.push(ev)
		}
	case r.dialing > 0 && (ev.CallControlID != "" || recordingEvent(ev.Type)):
		r.early = append(r.early, ev)
	}
}

// identityKeys are the owners keys of ev's leg and session ids.
func identityKeys(ev telnyx.Event) []string {
	var keys []string
	if ev.CallLegID != "" {
		keys = append(keys, "leg:"+ev.CallLegID)
	}
	if ev.CallSessionID != "" {
		keys = append(keys, "session:"+ev.CallSessionID)
	}
	return keys
}

// owner is the call id ev belongs to: its call_control_id, else the call
// its leg or session id was learned from, else "".
func (r *Runner) owner(ev telnyx.Event) string {
	if ev.CallControlID != "" {
		return ev.CallControlID
	}
	for _, k := range identityKeys(ev) {
		if id, ok := r.owners[k]; ok {
			return id
		}
	}
	return ""
}

// learn records the leg and session ids of an event that names call id.
func (r *Runner) learn(id string, c *callEvents, ev telnyx.Event) {
	if ev.CallControlID != id {
		return
	}
	for _, k := range identityKeys(ev) {
		if _, taken := r.owners[k]; !taken {
			r.owners[k] = id
			c.keys = append(c.keys, k)
		}
	}
}

// unidentified reports a recording event that carries no call identity at
// all. The Telnyx spec gives every recording event a leg and session id;
// this covers payloads that still lack them.
func (r *Runner) unidentified(ev telnyx.Event) bool {
	return recordingEvent(ev.Type) && ev.CallControlID == "" && len(identityKeys(ev)) == 0
}

// soleOwner reports that exactly one call can own an unidentified event:
// one call is in progress (calls never overlap, AD-2) and no earlier call
// attempted recording. Clearing late does not rule out webhook redelivery.
func (r *Runner) soleOwner() bool {
	return len(r.calls) == 1 && !r.recordingHistory && len(r.late) == 0
}

// lateArrived notes a late event of finished call id and forgets the call
// once nothing more is expected from it.
func (r *Runner) lateArrived(id string, l *lateCall, ev telnyx.Event) {
	l.note(ev)
	if l.recording || l.text {
		return
	}
	delete(r.late, id)
	for _, k := range l.keys {
		delete(r.owners, k)
	}
}

// Only recording payloads can omit call_control_id (TO-0005). A malformed
// detection or hangup must not be attributed to whichever call is active.
func recordingEvent(typ string) bool {
	switch typ {
	case telnyx.EventRecordingSaved, telnyx.EventRecordingError, telnyx.EventTranscriptionSaved:
		return true
	}
	return false
}

func (c *callEvents) push(ev telnyx.Event) {
	c.queue = append(c.queue, ev)
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// startDial marks a Dial in flight so early events are kept.
func (r *Runner) startDial() *dialFlight {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialing++
	d := &dialFlight{done: make(chan struct{})}
	r.pending[d] = struct{}{}
	return d
}

// endDial ends a Dial. With an id it registers the call and moves the early
// events of that id into its queue; early events of other ids are dropped
// once no Dial is in flight. The returned context belongs to a Shutdown that
// must receive the hang-up result before it can report success.
func (r *Runner) endDial(id string, d *dialFlight) (c *callEvents, shutdown context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dialing--
	delete(r.pending, d)
	shutdown = d.shutdown
	if id != "" {
		c = &callEvents{wake: make(chan struct{}, 1)}
		r.calls[id] = c
		// Learn the call's leg and session ids first, so early events that
		// carry only those still match, in their original order.
		for _, ev := range r.early {
			r.learn(id, c, ev)
		}
		kept := r.early[:0]
		for _, ev := range r.early {
			if r.owner(ev) == id || (r.unidentified(ev) && r.dialing == 0 && r.soleOwner()) {
				c.push(ev)
			} else {
				kept = append(kept, ev)
			}
		}
		r.early = kept
	}
	if r.dialing == 0 {
		r.early = nil
	}
	return c, shutdown
}

// forget ends call id. If Telnyx may still send its recording or
// transcription (awaited), the call moves to late so those events are
// recognised and dropped instead of reaching the next call.
func (r *Runner) forget(id string, c *callEvents, recording, text, machine bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.calls, id)
	r.recordingHistory = r.recordingHistory || machine
	l := &lateCall{keys: c.keys, recording: recording, text: text}
	// Events queued after the result was reached have arrived too.
	for _, ev := range c.queue {
		l.note(ev)
	}
	c.queue = nil
	if l.recording || l.text {
		r.late[id] = l
		return
	}
	for _, k := range l.keys {
		delete(r.owners, k)
	}
}

func (r *Runner) take(c *callEvents) []telnyx.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	evs := c.queue
	c.queue = nil
	return evs
}

// Call dials number, follows the call's events until it has a result or
// the time limit is reached, and returns the Outcome.
func (r *Runner) Call(ctx context.Context, number string) Outcome {
	startedAt := r.cfg.Now()
	limit := r.cfg.After(r.cfg.Limit)
	// The limit cancels in-flight HTTP operations as well as event waiting.
	// Request cancellation only governs Dial; a placed call still needs cleanup.
	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	go func() {
		select {
		case <-limit:
			cancel()
		case <-callCtx.Done():
		}
	}()
	dialCtx, cancelDial := context.WithCancel(callCtx)
	stop := context.AfterFunc(ctx, cancelDial)
	defer stop()
	defer cancelDial()
	if ctx.Err() != nil {
		cancelDial()
	}

	dial := r.startDial()
	id, err := r.cmd.Dial(dialCtx, telnyx.DialRequest{
		To:           number,
		From:         r.cfg.From,
		ConnectionID: r.cfg.ConnectionID,
		WebhookURL:   r.cfg.WebhookURL,
		// Whole seconds: Telnyx takes no fraction.
		TimeLimitSecs: int(r.cfg.Limit/time.Second) + callLimitExtraSecs,
	})
	stop()
	if err != nil {
		r.endDial("", dial)
		close(dial.done)
		reason := reasonTelnyxError + telnyxCode(err)
		if callCtx.Err() != nil {
			reason = reasonNoEvents
		}
		return Outcome{Code: result.Unknown, Reason: reason, StartedAt: startedAt}
	}
	c, shutdown := r.endDial(id, dial)

	// Once the call exists it must still be ended and its recording kept,
	// even if the requester stops waiting (C-1, TC-4).
	f := &callFlow{runner: r, ctx: callCtx, id: id}
	if shutdown == nil {
		close(dial.done)
	} else {
		// Shutdown's context bounds this hang-up and can outlive the check
		// limit, so it runs apart from the call: Shutdown waits for it through
		// dial.done, the call only up to its limit.
		go func() {
			defer close(dial.done)
			if err := r.cmd.Hangup(shutdown, id); err != nil {
				dial.err = fmt.Errorf("hang up call %s at shutdown: %w", id, err)
			}
		}()
		select {
		case <-dial.done:
		case <-callCtx.Done():
		}
		// A hang-up still held at the limit is not counted: the call then
		// sends its own. One that finished wins over a limit that fired
		// together with it, so no second hang-up follows a successful one.
		select {
		case <-dial.done:
			f.ended = dial.err == nil
		default:
		}
	}
	out := f.follow(c)
	recording, text := f.awaited()
	r.forget(id, c, recording, text, f.machine)
	out.StartedAt = startedAt
	return out
}

// Shutdown hangs up every call in progress and returns (D-6, TC-37); with no
// call it does nothing. The calls are not waited for: each still ends through
// its own events or limit. Pending dial replies and their hang-ups are waited
// for within ctx, so success cannot precede the cleanup command.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	ids := make([]string, 0, len(r.calls))
	for id := range r.calls {
		ids = append(ids, id)
	}
	pending := make([]*dialFlight, 0, len(r.pending))
	for d := range r.pending {
		if d.shutdown == nil {
			d.shutdown = ctx
		}
		pending = append(pending, d)
	}
	r.mu.Unlock()
	var err error
	for _, id := range ids {
		if hangupErr := r.cmd.Hangup(ctx, id); hangupErr != nil {
			err = errors.Join(err, fmt.Errorf("hang up call %s at shutdown: %w", id, hangupErr))
		}
	}
	for _, d := range pending {
		select {
		case <-d.done:
			err = errors.Join(err, d.err)
		case <-ctx.Done():
			return errors.Join(err, fmt.Errorf("wait for dial hang-up at shutdown: %w", ctx.Err()))
		}
	}
	return err
}

// telnyxCode is the short form of a dial error used in the reason (R-20).
func telnyxCode(err error) string {
	var apiErr *telnyx.APIError
	if !errors.As(err, &apiErr) {
		return err.Error()
	}
	if apiErr.Code != "" {
		return apiErr.Code
	}
	return fmt.Sprintf("status %d", apiErr.Status)
}

// callFlow is the state of one call while its events are followed.
type callFlow struct {
	runner *Runner
	ctx    context.Context
	id     string
	out    Outcome

	answered bool
	// machine is set once detection said machine and recording was asked for.
	machine bool
	// ended is set once the call is over or a hang-up was sent.
	ended         bool
	recordingDone bool
	textDone      bool
	// recording is set while Telnyx may send recording events: from a
	// successful StartRecording until a recording error.
	recording bool
	// noText is the reason given when the call ends without text. It is set
	// once recording has started.
	noText string
	// textWait fires when the wait for the text is over (TC-36). It is nil,
	// which never fires, until the recording was saved or its download failed.
	textWait <-chan time.Time
}

// awaited reports whether Telnyx may still send the recording and the
// transcription of the call.
func (f *callFlow) awaited() (recording, text bool) {
	if !f.recording {
		return false, false
	}
	return !f.recordingDone, !f.textDone
}

func (f *callFlow) follow(c *callEvents) Outcome {
	for {
		select {
		case <-c.wake:
			if f.handleAll(c) {
				return f.out
			}
			continue
		case <-f.textWait:
		case <-f.ctx.Done():
		}
		// Events queued before the wait or the limit ran out still count.
		if f.handleAll(c) {
			return f.out
		}
		return f.expire()
	}
}

func (f *callFlow) handleAll(c *callEvents) bool {
	for _, ev := range f.runner.take(c) {
		if f.handle(ev) {
			return true
		}
	}
	return false
}

// handle applies one event and reports whether the call has its result.
func (f *callFlow) handle(ev telnyx.Event) bool {
	switch ev.Type {
	case telnyx.EventCallAnswered:
		f.answered = true
	case telnyx.EventCallHangup:
		if f.machine {
			// The recording is saved when the far end hangs up; keep waiting for it.
			f.ended = true
			return false
		}
		if f.answered {
			// Answered but ended before detection finished: detection gave no answer (R-20).
			f.out.Code, f.out.Reason = result.FromDetection("")
		} else {
			f.out.Code, f.out.Reason = result.FromHangup(ev.HangupCause, ev.SIPHangupCause, f.runner.cfg.Combine)
		}
		return true
	case telnyx.EventMachinePremiumDetectionEnded:
		if f.machine {
			return false
		}
		f.out.Code, f.out.Reason = result.FromDetection(ev.DetectionResult)
		if f.out.Code != result.VoiceMessage {
			// C-1, TC-5: only a machine is ever recorded.
			f.hangup()
			return true
		}
		f.machine = true
		if err := f.runner.cmd.StartRecording(f.ctx, f.id); err != nil {
			f.out.Text = textNotAvailable + reasonRecordingStart + err.Error()
			f.hangup()
			return true
		}
		f.recording = true
		f.noText = reasonRecordingLate
	case telnyx.EventRecordingSaved:
		if !f.machine || f.recordingDone {
			return false
		}
		f.recordingDone = true
		// The 10 seconds are kept (TC-4); nothing more of the call is needed.
		f.hangup()
		f.noText = reasonTextLate
		if err := f.download(ev.RecordingURL); err != nil {
			// D-3: a failed download neither ends the check nor replaces the
			// text. It is logged because text that arrives leaves it no
			// other place.
			failure := withoutQuery(err.Error(), ev.RecordingURL)
			log.Printf("check: %s", failure)
			f.noText = reasonDownloadFailed + failure
		}
		if f.textDone {
			return true
		}
		f.textWait = f.runner.cfg.After(textWaitLimit)
	case telnyx.EventRecordingError:
		if !f.machine || f.recordingDone {
			return false
		}
		// D-3: text that arrived is kept.
		if !f.textDone {
			f.out.Text = textNotAvailable + reasonRecordingFailed
		}
		f.recording = false
		f.hangup()
		return true
	case telnyx.EventTranscriptionSaved:
		if !f.machine || f.textDone {
			return false
		}
		f.textDone = true
		f.out.Text = channelPrefix.ReplaceAllString(ev.TranscriptionText, "")
		if f.out.Text == "" {
			f.out.Text = textNotAvailable + reasonTextEmpty
		}
		return f.recordingDone
	}
	return false
}

// expire ends a call whose time limit or text wait ran out (TC-19, TC-21,
// TC-36).
func (f *callFlow) expire() Outcome {
	f.hangup()
	if !f.machine {
		return Outcome{Code: result.Unknown, Reason: reasonNoEvents}
	}
	if !f.textDone {
		f.out.Text = textNotAvailable + f.noText
	}
	return f.out
}

// hangup ends the call once. A failed hang-up does not change the result,
// so it is logged.
func (f *callFlow) hangup() {
	if f.ended {
		return
	}
	f.ended = true
	// Cleanup must be sent with a live context even when the call limit fired.
	// It is bounded separately, and cannot hold up the timed-out request.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(f.ctx), f.runner.cfg.Limit)
	done := make(chan struct{})
	go func() {
		defer cancel()
		defer close(done)
		if err := f.runner.cmd.Hangup(ctx, f.id); err != nil {
			log.Printf("check: hang up call %s: %v", f.id, err)
		}
	}()
	select {
	case <-done:
	case <-f.ctx.Done():
	}
}

// withoutQuery removes the query of link from text. A recording link is
// pre-signed, so its query is a credential, and the reason is saved and shown
// (TC-35).
func withoutQuery(text, link string) string {
	_, query, _ := strings.Cut(link, "?")
	if query == "" {
		return text
	}
	return strings.ReplaceAll(text, query, queryRemoved)
}

// download saves the recording at url as <RecordingsDir>/<call id>.mp3 and
// sets RecordingPath to the file name (TC-22).
func (f *callFlow) download(url string) error {
	name := f.id + recordingExt
	if filepath.Base(name) != name {
		return fmt.Errorf("call id %q is not a plain file name", f.id)
	}
	path := filepath.Join(f.runner.cfg.RecordingsDir, name)
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create recording of call %s: %w", f.id, err)
	}
	err = f.runner.cmd.Download(f.ctx, url, file)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return errors.Join(fmt.Errorf("download recording of call %s: %w", f.id, err), os.Remove(path))
	}
	f.out.RecordingPath = name
	return nil
}
