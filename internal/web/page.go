// Package web serves the test page of UX-1: one number typed by hand, its
// result, and the earlier checks of that number. The page is a Go template
// with one small script (TC-25); it is mounted only on the local listener (AD-6).
package web

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	// The zone database is built in so Eastern time never depends on the host.
	_ "time/tzdata"

	"checknumber/internal/check"
	"checknumber/internal/phone"
	"checknumber/internal/result"
)

const (
	routePage  = "GET /{$}"
	routeCheck = "POST /check"
	// recordingsPrefix is matched before the mux, which would clean "/recordings/../x"
	// into a redirect instead of the 404 TC-22 requires.
	recordingsPrefix = "/recordings/"
	numberField      = "number"

	// TC-24: times are shown in US Eastern with the zone named.
	easternZone    = "America/New_York"
	timeLayout     = "2006-01-02 15:04"
	easternSuffix  = " (Eastern)"
	notCalled      = "Not called"
	calling        = "Calling"
	day            = 24 * time.Hour
	msgCheckFailed = "The check could not be completed. Try again."

	hintNewCall  = "New call just made"
	hintFax      = "A fax machine answered"
	hintPerson   = "The app hung up. Nothing was kept."
	hintUnknown  = "Follow up by hand"
	chipOK       = "c-ok"
	chipWarn     = "c-warn"
	chipBad      = "c-bad"
	chipInfo     = "c-info"
	contentType  = "Content-Type"
	contentHTML  = "text/html; charset=utf-8"
	parentDirRef = ".."
)

//go:embed page.html
var pageSource string

var (
	pageTemplate = template.Must(template.New("page").Parse(pageSource))
	eastern      = mustLoadLocation(easternZone)

	hints = map[result.Code]string{
		result.Fax:            hintFax,
		result.PersonAnswered: hintPerson,
		result.Unknown:        hintUnknown,
	}
	chips = map[result.Code]string{
		result.Fax:            chipOK,
		result.PersonAnswered: chipWarn,
		result.Unknown:        chipBad,
	}
	// S-3: the results that may carry a recording and its words. C-1: a person
	// is never among them, so nothing of a person is shown even if it reached the page.
	wordResults = map[result.Code]bool{
		result.VoiceMessage:            true,
		result.Disconnected:            true,
		result.DisconnectedUnallocated: true,
		result.Unknown:                 true,
	}
)

// Checker runs checks and reads the history; *check.Service satisfies it.
// It repeats httpapi.Checker so the page does not import the API layer.
type Checker interface {
	Check(ctx context.Context, input string) (check.Answer, error)
	History(ctx context.Context, input string) ([]check.Answer, error)
}

type pageData struct {
	Input   string
	Error   string
	Result  *resultView
	History []historyRow
}

type resultView struct {
	Label     string
	Chip      string
	Hint      string
	Number    string
	CalledAt  string
	Reason    string
	Saved     bool
	SavedDays int
	Audio     string
	Text      string
	Redial    bool
}

type historyRow struct {
	CalledAt string
	Label    string
}

type page struct {
	checker       Checker
	recordingsDir string
	mux           *http.ServeMux
}

// NewPage returns the test page handler: GET / shows the form, GET /?number=
// adds the history, POST /check runs one check, and GET /recordings/<file>
// serves a recording from recordingsDir by plain file name (TC-22).
func NewPage(c Checker, recordingsDir string) http.Handler {
	// CrossOriginProtection refuses a POST another site's page makes the
	// browser send, so no stranger's page can place a paid call (AD-6).
	p := &page{checker: c, recordingsDir: recordingsDir, mux: http.NewServeMux()}
	p.mux.HandleFunc(routePage, p.showPage)
	p.mux.HandleFunc(routeCheck, p.runCheck)
	return http.NewCrossOriginProtection().Handler(p)
}

func (p *page) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if name, ok := strings.CutPrefix(r.URL.Path, recordingsPrefix); ok {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		p.serveRecording(w, r, name)
		return
	}
	p.mux.ServeHTTP(w, r)
}

func (p *page) showPage(w http.ResponseWriter, r *http.Request) {
	data := pageData{Input: r.URL.Query().Get(numberField)}
	if !r.URL.Query().Has(numberField) {
		render(w, http.StatusOK, data)
		return
	}
	answers, err := p.checker.History(r.Context(), data.Input)
	if err != nil {
		status, msg := errorMessage("history", err)
		data.Error = msg
		render(w, status, data)
		return
	}
	data.History = historyRows(answers)
	render(w, http.StatusOK, data)
}

func (p *page) runCheck(w http.ResponseWriter, r *http.Request) {
	data := pageData{Input: r.FormValue(numberField)}
	answer, err := p.checker.Check(r.Context(), data.Input)
	if err != nil {
		status, msg := errorMessage("check", err)
		data.Error = msg
		render(w, status, data)
		return
	}
	data.Result = toResultView(answer, time.Now())
	answers, err := p.checker.History(r.Context(), answer.Number)
	if err != nil {
		status, msg := errorMessage("history after check", err)
		data.Error = msg
		render(w, status, data)
		return
	}
	data.History = historyRows(answers)
	render(w, http.StatusOK, data)
}

// serveRecording serves recordingsDir/name only when name is a plain file
// name; os.OpenInRoot also refuses a symbolic link that leads outside (TC-22).
func (p *page) serveRecording(w http.ResponseWriter, r *http.Request, name string) {
	if name == "" || name == parentDirRef || filepath.Base(name) != name || strings.ContainsRune(name, '\\') {
		http.NotFound(w, r)
		return
	}
	f, err := os.OpenInRoot(p.recordingsDir, name)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("web: open recording failed (%T)", err)
		}
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		if err != nil {
			log.Printf("web: stat recording failed (%T)", err)
		}
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// errorMessage gives the R-24 message for a refused number and a plain
// failure message for anything else. Error strings may hold phone numbers,
// so only the type is logged.
func errorMessage(op string, err error) (int, string) {
	var invalid *phone.InvalidError
	if errors.As(err, &invalid) {
		return http.StatusBadRequest, invalid.Message
	}
	log.Printf("web: %s failed (%T)", op, err)
	return http.StatusInternalServerError, msgCheckFailed
}

func toResultView(a check.Answer, now time.Time) *resultView {
	v := &resultView{
		Label:    a.Label,
		Chip:     chipInfo,
		Hint:     hints[a.Code],
		Number:   a.Number,
		CalledAt: easternTime(a.CalledAt) + easternSuffix,
		Reason:   a.Reason,
		Saved:    a.Saved,
		Redial:   !result.Reusable(a.Code),
	}
	if chip, ok := chips[a.Code]; ok {
		v.Chip = chip
	}
	if v.Hint == "" && !a.Saved {
		v.Hint = hintNewCall
	}
	if a.Saved {
		v.SavedDays = int(now.Sub(a.CalledAt) / day)
	}
	if wordResults[a.Code] {
		if a.RecordingPath != "" {
			v.Audio = recordingsPrefix + a.RecordingPath
		}
		v.Text = a.Text
	}
	return v
}

func historyRows(answers []check.Answer) []historyRow {
	rows := make([]historyRow, 0, len(answers))
	for _, a := range answers {
		label := a.Label
		// History includes unfinished checks, which do not have a result yet.
		if a.Code == "" {
			label = calling
		}
		rows = append(rows, historyRow{CalledAt: easternTime(a.CalledAt), Label: label})
	}
	return rows
}

// easternTime formats t in US Eastern; a check closed before it was dialled
// (TC-20) has no call time.
func easternTime(t time.Time) string {
	if t.IsZero() {
		return notCalled
	}
	return t.In(eastern).Format(timeLayout)
}

// render executes the template into a buffer first, so a template failure
// becomes a 500 instead of a half-written page.
func render(w http.ResponseWriter, status int, data pageData) {
	var buf bytes.Buffer
	if err := pageTemplate.Execute(&buf, data); err != nil {
		log.Printf("web: render page failed: %v", err)
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set(contentType, contentHTML)
	w.WriteHeader(status)
	if _, err := buf.WriteTo(w); err != nil {
		log.Printf("web: write page failed (%T)", err)
	}
}

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic("web: load time zone " + name + ": " + err.Error())
	}
	return loc
}
