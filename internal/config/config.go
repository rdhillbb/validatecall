// Package config reads the service's settings from the environment and, for
// the four Telnyx settings, from the operator's local settings file (TS-8,
// R-25, C-3, C-4). Values are never written to a file, a log or an error.
package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Setting names (TC-27). The ports come from the port registry (C-2): the
// operator exports what `get_port` returns, so no port number is in the code.
const (
	envPort          = "CHECKNUMBER_PORT"
	envEventsPort    = "CHECKNUMBER_EVENTS_PORT"
	envPublicURL     = "CHECKNUMBER_PUBLIC_URL"
	envRecordingsDir = "CHECKNUMBER_RECORDINGS_DIR"
	envAPIKey        = "TELNYX_API_KEY"
	envPublicKey     = "TELNYX_PUBLIC_KEY"
	envFromNumber    = "TELNYX_FROM_NUMBER"
	envConnectionID  = "TELNYX_CONNECTION_ID"
	envCheckLimit    = "CHECKNUMBER_CHECK_LIMIT"
	envCombine       = "CHECKNUMBER_COMBINE_DISCONNECTED"
	// envCheckAPIKey is the shared key the fax system sends to the check API
	// in M-2 (AD-6). Optional: unset, the API answers without a key.
	envCheckAPIKey = "CHECKNUMBER_API_KEY"
	// envFile is the path of the settings file (TC-31). Optional.
	envFile = "CHECKNUMBER_ENV_FILE"
	// envDatabase is read by the database driver. Load requires it because
	// the driver would silently use the database named after the user (D-7).
	envDatabase = "PGDATABASE"
	envTunnel   = "CHECKNUMBER_TUNNEL"
	// envLogFile is the path of the app's log file (TC-44). Optional: unset,
	// log lines go to standard error.
	envLogFile = "CHECKNUMBER_LOG_FILE"
)

// TunnelCloudflared is the one CHECKNUMBER_TUNNEL value besides empty: the app
// starts the tunnel itself and learns the public address from it (TC-32).
const TunnelCloudflared = "cloudflared"

// The settings file (TC-31). The operator's file already uses the short names.
const (
	fileAPIKey       = "APIKEY"
	filePublicKey    = "PUBLICKEY"
	fileConnectionID = "APPLICATIONID"
	fileFromNumber   = "FROMNUMBER"
	// A line is NAME:VALUE or NAME=VALUE; the first of the two ends the name,
	// so a base64 value may end in "=".
	fileSeparators = ":="
	// groupOtherAccess is every permission bit of group and others. The file
	// holds keys, so only its owner may have access (C-4).
	groupOtherAccess fs.FileMode = 0o077
)

// fileNames maps each name the settings file may use to its setting.
var fileNames = map[string]string{
	envAPIKey:        envAPIKey,
	fileAPIKey:       envAPIKey,
	envPublicKey:     envPublicKey,
	filePublicKey:    envPublicKey,
	envConnectionID:  envConnectionID,
	fileConnectionID: envConnectionID,
	envFromNumber:    envFromNumber,
	fileFromNumber:   envFromNumber,
}

const (
	// TC-18: one check's time limit when CHECKNUMBER_CHECK_LIMIT is unset.
	defaultCheckLimit = 120 * time.Second
	// R-23: Disconnected and Unallocated stay separate until the test calls
	// show they cannot be told apart.
	defaultCombine = false

	// AD-6: both listeners bind the loopback address only; the tunnel is what
	// makes the events port reachable from outside.
	loopbackHost = "127.0.0.1"
	minPort      = 1
	maxPort      = 65535
	// Telnyx needs a public HTTPS address for events (TS-6).
	publicScheme = "https"
	// webhookPath is where httpapi.NewEvents accepts Telnyx events (TC-13).
	webhookPath = "/telnyx/events"
)

// Why a setting was refused. None of them quotes the value (C-4).
const (
	whyMissing   = "missing"
	whyPort      = "not a port number from 1 to 65535"
	whyURL       = "not an https address with a hostname and valid port, without credentials, query or fragment"
	whyDir       = "not an existing folder"
	whyPublicKey = "not a base64 ed25519 public key"
	whyDuration  = "not a positive duration such as 120s"
	whyBool      = "not true or false"
	whyTunnel    = "not empty or " + TunnelCloudflared

	// Why the settings file was refused; each follows the file's path.
	whyFileMissing    = "file not found"
	whyFileUnreadable = "file cannot be read"
	whyFileNotRegular = "not a regular file"
	whyFileOpen       = "group or others have access; only the owner may"
)

// Config holds every setting the service runs with.
type Config struct {
	Port                int
	EventsPort          int
	PublicURL           string
	RecordingsDir       string
	TelnyxAPIKey        string
	TelnyxPublicKey     ed25519.PublicKey
	TelnyxFromNumber    string
	TelnyxConnectionID  string
	CheckLimit          time.Duration
	CombineDisconnected bool
	// APIKey, when not empty, is required on every check API request (AD-6).
	APIKey string
	// Database is the database name the driver connects to (D-7).
	Database string
	// Tunnel is empty or TunnelCloudflared. With TunnelCloudflared PublicURL
	// may be empty: the tunnel's address is only known once it runs (TC-32).
	Tunnel string
	// LogFile, when not empty, is the log file every log line goes to (TC-44).
	// Any value is accepted: a file that cannot be opened must not stop the
	// start (AD-7).
	LogFile string
}

// Load reads the settings. A missing or bad setting gives one error that
// names every bad setting and never contains a value; the one exception is
// the path of a refused settings file, which the operator needs to find it.
func Load() (Config, error) {
	var l loader
	l.readFile(envFile)
	apiKey, _ := optional(envCheckAPIKey)
	logFile, _ := optional(envLogFile)
	tunnel := l.tunnel(envTunnel)
	cfg := Config{
		Port:                l.port(envPort),
		EventsPort:          l.port(envEventsPort),
		PublicURL:           l.publicURL(envPublicURL, tunnel != TunnelCloudflared),
		RecordingsDir:       l.dir(envRecordingsDir),
		TelnyxAPIKey:        l.required(envAPIKey),
		TelnyxPublicKey:     l.publicKey(envPublicKey),
		TelnyxFromNumber:    l.required(envFromNumber),
		TelnyxConnectionID:  l.required(envConnectionID),
		CheckLimit:          l.duration(envCheckLimit, defaultCheckLimit),
		CombineDisconnected: l.boolean(envCombine, defaultCombine),
		APIKey:              apiKey,
		Database:            l.required(envDatabase),
		Tunnel:              tunnel,
		LogFile:             logFile,
	}
	if len(l.problems) > 0 {
		return Config{}, errors.New("config: bad settings: " + strings.Join(l.problems, "; "))
	}
	return cfg, nil
}

// LocalAddr is the address of the test page and check API listener.
func (c Config) LocalAddr() string {
	return net.JoinHostPort(loopbackHost, strconv.Itoa(c.Port))
}

// EventsAddr is the address of the Telnyx events listener.
func (c Config) EventsAddr() string {
	return net.JoinHostPort(loopbackHost, strconv.Itoa(c.EventsPort))
}

// WebhookURL is the events address Telnyx is given on each call (TC-13).
func (c Config) WebhookURL() string {
	return c.PublicURL + webhookPath
}

// loader collects every refused setting so Load reports them all at once.
type loader struct {
	problems []string
	// file holds the settings file's values by setting name.
	file map[string]string
}

func (l *loader) fail(name, why string) {
	l.problems = append(l.problems, name+" ("+why+")")
}

// required returns the environment's value, or else the settings file's: the
// environment wins (TC-31).
func (l *loader) required(name string) string {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		v = l.file[name]
	}
	if v == "" {
		l.fail(name, whyMissing)
	}
	return v
}

// readFile loads the settings file named by the setting name, when one is set
// (R-25). A refused file is reported by its path and the cause only.
func (l *loader) readFile(name string) {
	path, ok := optional(name)
	if !ok {
		return
	}
	values, why := parseFile(path)
	if why != "" {
		l.fail(name, path+": "+why)
		return
	}
	l.file = values
}

// parseFile returns the Telnyx settings in the file at path, or why the file
// is refused. No error text of the system is passed on, and no line: a line
// may hold a key (C-4).
func parseFile(path string) (values map[string]string, why string) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, whyFileMissing
	case err != nil:
		return nil, whyFileUnreadable
	case !info.Mode().IsRegular():
		return nil, whyFileNotRegular
	case info.Mode().Perm()&groupOtherAccess != 0:
		return nil, whyFileOpen
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, whyFileUnreadable
	}
	values = make(map[string]string)
	for line := range strings.Lines(string(raw)) {
		// A blank line, a "#" comment and any other name match no entry of
		// fileNames and are skipped.
		sep := strings.IndexAny(line, fileSeparators)
		if sep < 0 {
			continue
		}
		if setting, known := fileNames[strings.TrimSpace(line[:sep])]; known {
			values[setting] = strings.TrimSpace(line[sep+1:])
		}
	}
	return values, ""
}

// tunnel accepts the closed set of tunnel modes: empty or TunnelCloudflared.
func (l *loader) tunnel(name string) string {
	v, _ := optional(name)
	if v != "" && v != TunnelCloudflared {
		l.fail(name, whyTunnel)
		return ""
	}
	return v
}

// optional returns the trimmed value and whether one was set.
func optional(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(name))
	return v, v != ""
}

func (l *loader) port(name string) int {
	v := l.required(name)
	if v == "" {
		return 0
	}
	p, err := strconv.Atoi(v)
	if err != nil || p < minPort || p > maxPort {
		l.fail(name, whyPort)
		return 0
	}
	return p
}

// publicURL accepts the tunnel's https address and drops a trailing slash so
// WebhookURL never has a double slash. A query or fragment would absorb the
// appended webhook path instead of routing events to it. When the address is
// not needed (TC-32) it may be unset, and is still checked when set.
func (l *loader) publicURL(name string, needed bool) string {
	v, ok := optional(name)
	if !ok {
		if needed {
			l.fail(name, whyMissing)
		}
		return ""
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme != publicScheme || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(v, "#") {
		l.fail(name, whyURL)
		return ""
	}
	if u.Port() != "" {
		p, err := strconv.Atoi(u.Port())
		if err != nil || p < minPort || p > maxPort {
			l.fail(name, whyURL)
			return ""
		}
	} else if strings.HasSuffix(u.Host, ":") {
		l.fail(name, whyURL)
		return ""
	}
	return strings.TrimRight(v, "/")
}

// dir accepts an existing folder; the runner creates recording files in it
// (TC-22) and would otherwise fail only during a paid call.
func (l *loader) dir(name string) string {
	v := l.required(name)
	if v == "" {
		return ""
	}
	info, err := os.Stat(v)
	if err != nil || !info.IsDir() {
		l.fail(name, whyDir)
		return ""
	}
	return v
}

// publicKey decodes the base64 Telnyx public key; a key of the wrong length
// would make every event fail its signature check (TC-7).
func (l *loader) publicKey(name string) ed25519.PublicKey {
	v := l.required(name)
	if v == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(key) != ed25519.PublicKeySize {
		l.fail(name, whyPublicKey)
		return nil
	}
	return ed25519.PublicKey(key)
}

func (l *loader) duration(name string, def time.Duration) time.Duration {
	v, ok := optional(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		l.fail(name, whyDuration)
		return 0
	}
	return d
}

func (l *loader) boolean(name string, def bool) bool {
	v, ok := optional(name)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail(name, whyBool)
		return false
	}
	return b
}
