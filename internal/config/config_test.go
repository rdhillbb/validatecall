package config

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"checknumber/internal/result"
)

// The setting names are the contract of TC-27, TC-31, TC-32, TC-42, S-1 and
// S-4, written out here so a renamed setting fails the tests instead of
// silently changing the contract.
var (
	// CHECKNUMBER_PUBLIC_URL is required with CHECKNUMBER_TUNNEL empty, which
	// is how setAll leaves it.
	requiredSettings = []string{
		"CHECKNUMBER_PORT",
		"CHECKNUMBER_EVENTS_PORT",
		"CHECKNUMBER_PUBLIC_URL",
		"CHECKNUMBER_RECORDINGS_DIR",
		"TELNYX_API_KEY",
		"TELNYX_PUBLIC_KEY",
		"TELNYX_FROM_NUMBER",
		"TELNYX_CONNECTION_ID",
		"PGDATABASE",
	}
	optionalSettings = []string{
		"CHECKNUMBER_CHECK_LIMIT",
		"CHECKNUMBER_COMBINE_DISCONNECTED",
		"CHECKNUMBER_API_KEY",
		"CHECKNUMBER_ENV_FILE",
		"CHECKNUMBER_TUNNEL",
		"CHECKNUMBER_LOG_FILE",
	}
	// telnyxSettings are the four the settings file may hold (R-25).
	telnyxSettings = []string{"TELNYX_API_KEY", "TELNYX_PUBLIC_KEY", "TELNYX_CONNECTION_ID", "TELNYX_FROM_NUMBER"}
	// fileShortNames are the names the operator's file already uses (TC-31).
	fileShortNames = []string{"APIKEY", "PUBLICKEY", "APPLICATIONID", "FROMNUMBER"}
	// secretSettings are never assigned a value in the guide, under either
	// name (C-4).
	secretSettings = []string{"TELNYX_API_KEY", "TELNYX_PUBLIC_KEY", "CHECKNUMBER_API_KEY", "APIKEY", "PUBLICKEY"}
)

// Test values only; no real key is used (C-4) and the ports are never bound.
const (
	testPort         = "41001"
	testEventsPort   = "41002"
	testPublicURL    = "https://tunnel-test.example.test"
	testAPIKey       = "KEYtest-value-not-a-real-key"
	testFromNumber   = "+15552230099"
	testConnectionID = "test-connection-0001"
	testCheckLimit   = "90s"
	testCheckAPIKey  = "KEYtest-check-api-key-not-real"
	// Only a name: Load never connects.
	testDatabase = "checknumber_test"

	// What the settings file of a test holds, different from the environment's
	// values so the two sources can be told apart.
	inFileAPIKey       = "KEYtest-file-value-not-a-real-key"
	inFileFromNumber   = "+15552230098"
	inFileConnectionID = "test-file-connection-0002"
	ignoredFileValue   = "ignored-test-value"
	testFileName       = "telnyx-settings-test"
	// Independent of the production constant so a changed mode fails DD-5.
	tunnelCloudflared = "cloudflared"

	// The four settings under their long names; {api}, {pub}, {conn} and
	// {from} stand for the test values.
	longNameFile = "TELNYX_API_KEY:{api}\nTELNYX_PUBLIC_KEY:{pub}\nTELNYX_CONNECTION_ID:{conn}\nTELNYX_FROM_NUMBER:{from}\n"

	runningGuide  = "../../docs/RUNNING.md"
	m1Heading     = "## M-1 test calls"
	tunnelCommand = "cloudflared tunnel"
	eventsPortVar = "$CHECKNUMBER_EVENTS_PORT"
	localPortVar  = "$CHECKNUMBER_PORT"
)

// longToken finds anything shaped like a pasted key: a 40+ character run of
// base64 or key characters. A Telnyx public key is 44 base64 characters.
var longToken = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)

// newTestKey returns a fresh public key as Telnyx gives it, in base64.
func newTestKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// setAll sets every setting to a valid test value and returns them by name.
// The settings file, the tunnel mode and the log file are set empty, so a
// file or a mode in the environment the tests run in never reaches Load.
func setAll(t *testing.T) map[string]string {
	t.Helper()
	values := map[string]string{
		"CHECKNUMBER_PORT":                 testPort,
		"CHECKNUMBER_EVENTS_PORT":          testEventsPort,
		"CHECKNUMBER_PUBLIC_URL":           testPublicURL + "/",
		"CHECKNUMBER_RECORDINGS_DIR":       t.TempDir(),
		"TELNYX_API_KEY":                   testAPIKey,
		"TELNYX_PUBLIC_KEY":                newTestKey(t),
		"TELNYX_FROM_NUMBER":               testFromNumber,
		"TELNYX_CONNECTION_ID":             testConnectionID,
		"CHECKNUMBER_CHECK_LIMIT":          testCheckLimit,
		"CHECKNUMBER_COMBINE_DISCONNECTED": "true",
		"CHECKNUMBER_API_KEY":              testCheckAPIKey,
		"PGDATABASE":                       testDatabase,
		"CHECKNUMBER_ENV_FILE":             "",
		"CHECKNUMBER_TUNNEL":               "",
		"CHECKNUMBER_LOG_FILE":             "",
	}
	for name, v := range values {
		t.Setenv(name, v)
	}
	return values
}

func TestLoad(t *testing.T) {
	values := setAll(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantKey, err := base64.StdEncoding.DecodeString(values["TELNYX_PUBLIC_KEY"])
	if err != nil {
		t.Fatalf("decode test key: %v", err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"Port", cfg.Port, 41001},
		{"EventsPort", cfg.EventsPort, 41002},
		{"PublicURL", cfg.PublicURL, testPublicURL},
		{"RecordingsDir", cfg.RecordingsDir, values["CHECKNUMBER_RECORDINGS_DIR"]},
		{"TelnyxAPIKey", cfg.TelnyxAPIKey, testAPIKey},
		{"TelnyxFromNumber", cfg.TelnyxFromNumber, testFromNumber},
		{"TelnyxConnectionID", cfg.TelnyxConnectionID, testConnectionID},
		{"CheckLimit", cfg.CheckLimit, 90 * time.Second},
		{"CombineDisconnected", cfg.CombineDisconnected, true},
		{"APIKey", cfg.APIKey, testCheckAPIKey},
		{"Database", cfg.Database, testDatabase},
		{"Tunnel", cfg.Tunnel, ""},
		{"WebhookURL", cfg.WebhookURL(), testPublicURL + "/telnyx/events"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !bytes.Equal(cfg.TelnyxPublicKey, wantKey) {
		t.Errorf("TelnyxPublicKey = %x, want %x", []byte(cfg.TelnyxPublicKey), wantKey)
	}
}

func TestLoadDefaults(t *testing.T) {
	setAll(t)
	for _, name := range optionalSettings {
		t.Setenv(name, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load with optional settings unset: %v", err)
	}
	if cfg.CheckLimit != 120*time.Second {
		t.Errorf("CheckLimit = %v, want the TC-18 default 120s", cfg.CheckLimit)
	}
	if cfg.CombineDisconnected {
		t.Errorf("CombineDisconnected = true, want the R-23 default false")
	}
	if cfg.APIKey != "" {
		t.Errorf("APIKey set with CHECKNUMBER_API_KEY unset, want empty (AD-6)")
	}
}

// TestLoadAPIKey: CHECKNUMBER_API_KEY is optional, and its value is in no
// error and no log line (C-4, AD-6).
func TestLoadAPIKey(t *testing.T) {
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	t.Run("set", func(t *testing.T) {
		setAll(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.APIKey != testCheckAPIKey {
			t.Errorf("APIKey does not hold the value of CHECKNUMBER_API_KEY")
		}
	})
	t.Run("unset", func(t *testing.T) {
		setAll(t)
		t.Setenv("CHECKNUMBER_API_KEY", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load with CHECKNUMBER_API_KEY unset: %v", err)
		}
		if cfg.APIKey != "" {
			t.Errorf("APIKey not empty with CHECKNUMBER_API_KEY unset")
		}
	})
	t.Run("other setting bad", func(t *testing.T) {
		values := setAll(t)
		for _, name := range requiredSettings {
			t.Setenv(name, "")
		}
		_, err := Load()
		if err == nil {
			t.Fatal("Load with required settings unset: no error")
		}
		if strings.Contains(err.Error(), "CHECKNUMBER_API_KEY") {
			t.Errorf("error %q names CHECKNUMBER_API_KEY, which is set", err)
		}
		assertNoValue(t, err.Error(), values)
	})

	if strings.Contains(logged.String(), testCheckAPIKey) {
		t.Errorf("a log line contains the value of CHECKNUMBER_API_KEY")
	}
}

func TestLoadPublicURL(t *testing.T) {
	for _, address := range []string{testPublicURL, testPublicURL + ":443"} {
		t.Run(address, func(t *testing.T) {
			setAll(t)
			t.Setenv("CHECKNUMBER_PUBLIC_URL", address+"/")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got, want := cfg.WebhookURL(), address+"/telnyx/events"; got != want {
				t.Errorf("WebhookURL = %q, want %q", got, want)
			}
		})
	}
}

func TestLoadMissing(t *testing.T) {
	// Each required setting alone: the error names it and no other setting.
	for _, missing := range requiredSettings {
		t.Run(missing, func(t *testing.T) {
			values := setAll(t)
			t.Setenv(missing, "")
			_, err := Load()
			if err == nil {
				t.Fatalf("Load with %s unset: no error", missing)
			}
			msg := err.Error()
			if !strings.Contains(msg, missing+" (missing)") {
				t.Errorf("error %q does not name %s as missing", msg, missing)
			}
			for _, other := range append(append([]string{}, requiredSettings...), optionalSettings...) {
				if other != missing && strings.Contains(msg, other) {
					t.Errorf("error %q names %s, which is set", msg, other)
				}
			}
			assertNoValue(t, msg, values)
		})
	}

	// All of them at once: one error names every one.
	t.Run("all", func(t *testing.T) {
		for _, name := range append(append([]string{}, requiredSettings...), optionalSettings...) {
			t.Setenv(name, "")
		}
		_, err := Load()
		if err == nil {
			t.Fatal("Load with nothing set: no error")
		}
		for _, name := range requiredSettings {
			if !strings.Contains(err.Error(), name+" (missing)") {
				t.Errorf("error %q does not name %s", err, name)
			}
		}
	})
}

func TestLoadInvalid(t *testing.T) {
	shortKey := base64.StdEncoding.EncodeToString([]byte("short-key-bytes"))
	cases := []struct {
		name, value string
	}{
		{"CHECKNUMBER_PORT", "not-a-port"},
		{"CHECKNUMBER_PORT", "70000"},
		{"CHECKNUMBER_EVENTS_PORT", "0"},
		{"CHECKNUMBER_PUBLIC_URL", "http://plain-tunnel.example.test"},
		{"CHECKNUMBER_PUBLIC_URL", "https://"},
		{"CHECKNUMBER_PUBLIC_URL", "https://:443"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + ":0"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + ":70000"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + ":"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + "?token=private-test-value"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + "?"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + "#fragment"},
		{"CHECKNUMBER_PUBLIC_URL", testPublicURL + "#"},
		{"CHECKNUMBER_PUBLIC_URL", "https://test-user:test-password@tunnel-test.example.test"},
		{"CHECKNUMBER_RECORDINGS_DIR", filepath.Join(os.TempDir(), "checknumber-no-such-folder-zq")},
		{"TELNYX_PUBLIC_KEY", "@@not-base64@@"},
		{"TELNYX_PUBLIC_KEY", shortKey},
		{"CHECKNUMBER_CHECK_LIMIT", "-5s"},
		{"CHECKNUMBER_CHECK_LIMIT", "soon"},
		{"CHECKNUMBER_COMBINE_DISCONNECTED", "maybe"},
	}
	for _, c := range cases {
		t.Run(c.name+"="+c.value, func(t *testing.T) {
			values := setAll(t)
			t.Setenv(c.name, c.value)
			values[c.name] = c.value
			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted %s=%q", c.name, c.value)
			}
			if !strings.Contains(err.Error(), c.name+" (") {
				t.Errorf("error %q does not name %s", err, c.name)
			}
			assertNoValue(t, err.Error(), values)
		})
	}
	// A file is not a folder for recordings.
	t.Run("recordings dir is a file", func(t *testing.T) {
		values := setAll(t)
		file := filepath.Join(t.TempDir(), "not-a-folder")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatalf("write %s: %v", file, err)
		}
		t.Setenv("CHECKNUMBER_RECORDINGS_DIR", file)
		values["CHECKNUMBER_RECORDINGS_DIR"] = file
		_, err := Load()
		if err == nil || !strings.Contains(err.Error(), "CHECKNUMBER_RECORDINGS_DIR (") {
			t.Fatalf("Load error = %v, want CHECKNUMBER_RECORDINGS_DIR named", err)
		}
		assertNoValue(t, err.Error(), values)
	})
}

// writeSettingsFile writes content to a new owner-only file in a temp folder,
// points CHECKNUMBER_ENV_FILE at it and returns its path.
func writeSettingsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), testFileName)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Setenv("CHECKNUMBER_ENV_FILE", path)
	return path
}

// fileContent fills the test values into a settings file template.
func fileContent(template, publicKey string) string {
	return strings.NewReplacer("{api}", inFileAPIKey, "{pub}", publicKey,
		"{conn}", inFileConnectionID, "{from}", inFileFromNumber).Replace(template)
}

// TestLoadFromFile: the four Telnyx values come from the settings file alone,
// under either name and with either separator (R-25, TC-31).
func TestLoadFromFile(t *testing.T) {
	cases := []struct {
		name, template string
	}{
		{"long names with colon", longNameFile},
		{"long names with equals", "TELNYX_API_KEY={api}\nTELNYX_PUBLIC_KEY={pub}\nTELNYX_CONNECTION_ID={conn}\nTELNYX_FROM_NUMBER={from}\n"},
		{"short names with colon", "APIKEY:{api}\nPUBLICKEY:{pub}\nAPPLICATIONID:{conn}\nFROMNUMBER:{from}\n"},
		{"short names with equals", "APIKEY={api}\nPUBLICKEY={pub}\nAPPLICATIONID={conn}\nFROMNUMBER={from}\n"},
		// The skipped lines come last: one read as a setting would replace
		// the value above it.
		{"spaces, comments, blank lines and other names", "# test values only\n\n" +
			"  APIKEY : {api}  \n\tTELNYX_PUBLIC_KEY = {pub}\r\nAPPLICATIONID=  {conn}\nFROMNUMBER:{from}\n" +
			"#APIKEY:" + ignoredFileValue + "\n# TELNYX_PUBLIC_KEY = " + ignoredFileValue + "\n" +
			"OTHERNAME=" + ignoredFileValue + "\nAPPLICATIONID " + ignoredFileValue + "\n\nFROMNUMBERS:" + ignoredFileValue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setAll(t)
			for _, name := range telnyxSettings {
				t.Setenv(name, "")
			}
			publicKey := newTestKey(t)
			writeSettingsFile(t, fileContent(c.template, publicKey))

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.TelnyxAPIKey != inFileAPIKey {
				t.Errorf("TelnyxAPIKey is not the file's value")
			}
			if got := base64.StdEncoding.EncodeToString(cfg.TelnyxPublicKey); got != publicKey {
				t.Errorf("TelnyxPublicKey is not the file's value")
			}
			if cfg.TelnyxConnectionID != inFileConnectionID {
				t.Errorf("TelnyxConnectionID = %q, want %q", cfg.TelnyxConnectionID, inFileConnectionID)
			}
			if cfg.TelnyxFromNumber != inFileFromNumber {
				t.Errorf("TelnyxFromNumber = %q, want %q", cfg.TelnyxFromNumber, inFileFromNumber)
			}
		})
	}
}

// TestEnvWinsOverFile: with a value in both places the environment's is used;
// the file's counts only for a name the environment leaves unset (TC-31).
func TestEnvWinsOverFile(t *testing.T) {
	values := setAll(t)
	writeSettingsFile(t, fileContent(longNameFile, newTestKey(t)))

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelnyxAPIKey != testAPIKey {
		t.Errorf("TelnyxAPIKey is not the environment's value")
	}
	if got := base64.StdEncoding.EncodeToString(cfg.TelnyxPublicKey); got != values["TELNYX_PUBLIC_KEY"] {
		t.Errorf("TelnyxPublicKey is not the environment's value")
	}
	if cfg.TelnyxConnectionID != testConnectionID {
		t.Errorf("TelnyxConnectionID = %q, want the environment's %q", cfg.TelnyxConnectionID, testConnectionID)
	}
	if cfg.TelnyxFromNumber != testFromNumber {
		t.Errorf("TelnyxFromNumber = %q, want the environment's %q", cfg.TelnyxFromNumber, testFromNumber)
	}

	// The file was read: it supplies the one name the environment drops.
	t.Setenv("TELNYX_API_KEY", "")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with TELNYX_API_KEY only in the file: %v", err)
	}
	if cfg.TelnyxAPIKey != inFileAPIKey {
		t.Errorf("TelnyxAPIKey is not the file's value with the environment's unset")
	}
	if cfg.TelnyxFromNumber != testFromNumber {
		t.Errorf("TelnyxFromNumber = %q, want the environment's %q", cfg.TelnyxFromNumber, testFromNumber)
	}
}

// TestEnvFileRefused: a settings file that is missing, not a regular file or
// open to group or others stops Load, even with every setting in the
// environment. The error names the path and the cause and holds no value of
// the file (R-25, C-4).
func TestEnvFileRefused(t *testing.T) {
	const (
		whyMissingFile = "file not found"
		whyNotRegular  = "not a regular file"
		whyOpen        = "group or others have access; only the owner may"
	)
	publicKey := newTestKey(t)
	content := fileContent(longNameFile, publicKey)
	withMode := func(mode fs.FileMode) func(*testing.T, string) {
		return func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			// Chmod, because the umask may clear bits WriteFile is given.
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod %s: %v", path, err)
			}
		}
	}
	cases := []struct {
		name   string
		create func(t *testing.T, path string)
		why    string
		// isFile: the path holds the settings, so owner-only access cures it.
		isFile bool
	}{
		{"missing file", func(*testing.T, string) {}, whyMissingFile, false},
		{"directory", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatalf("mkdir %s: %v", path, err)
			}
		}, whyNotRegular, false},
		{"readable by group", withMode(0o640), whyOpen, true},
		{"readable by others", withMode(0o604), whyOpen, true},
		{"writable by group only", withMode(0o620), whyOpen, true},
		{"open to everyone", withMode(0o666), whyOpen, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setAll(t)
			path := filepath.Join(t.TempDir(), testFileName)
			c.create(t, path)
			t.Setenv("CHECKNUMBER_ENV_FILE", path)

			_, err := Load()
			if err == nil {
				t.Fatalf("Load accepted the settings file (%s)", c.name)
			}
			msg := err.Error()
			if want := "CHECKNUMBER_ENV_FILE (" + path + ": " + c.why + ")"; !strings.Contains(msg, want) {
				t.Errorf("error %q does not contain %q", msg, want)
			}
			// The file is the only thing wrong.
			for _, other := range append(append([]string{}, requiredSettings...), optionalSettings...) {
				if other != "CHECKNUMBER_ENV_FILE" && strings.Contains(msg, other) {
					t.Errorf("error %q names %s, which is set", msg, other)
				}
			}
			assertNoValue(t, msg, map[string]string{
				"TELNYX_API_KEY":       inFileAPIKey,
				"TELNYX_PUBLIC_KEY":    publicKey,
				"TELNYX_CONNECTION_ID": inFileConnectionID,
				"TELNYX_FROM_NUMBER":   inFileFromNumber,
			})
			if !c.isFile {
				return
			}

			// Owner-only, the same file is read: the access bits were the cause.
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatalf("chmod %s: %v", path, err)
			}
			t.Setenv("TELNYX_API_KEY", "")
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load with the file owner-only: %v", err)
			}
			if cfg.TelnyxAPIKey != inFileAPIKey {
				t.Errorf("TelnyxAPIKey is not the file's value with the file owner-only")
			}
		})
	}
}

// TestTunnelSetting: CHECKNUMBER_TUNNEL is empty or cloudflared, and only
// cloudflared makes the public address optional (R-26, TC-32).
func TestTunnelSetting(t *testing.T) {
	const otherMode = "another-tunnel-test"
	cases := []struct {
		name, tunnel, publicURL string
		// wantBad is the setting the error must name; empty when Load succeeds.
		wantBad       string
		wantPublicURL string
	}{
		{"cloudflared without a public address", tunnelCloudflared, "", "", ""},
		{"cloudflared with a public address", tunnelCloudflared, testPublicURL, "", testPublicURL},
		{"cloudflared with a bad public address", tunnelCloudflared, "http://plain-tunnel.example.test", "CHECKNUMBER_PUBLIC_URL (not ", ""},
		{"empty without a public address", "", "", "CHECKNUMBER_PUBLIC_URL (missing)", ""},
		{"empty with a public address", "", testPublicURL, "", testPublicURL},
		{"another value", otherMode, testPublicURL, "CHECKNUMBER_TUNNEL (", ""},
		{"cloudflared in capitals", "CLOUDFLARED", testPublicURL, "CHECKNUMBER_TUNNEL (", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values := setAll(t)
			t.Setenv("CHECKNUMBER_TUNNEL", c.tunnel)
			t.Setenv("CHECKNUMBER_PUBLIC_URL", c.publicURL)
			values["CHECKNUMBER_TUNNEL"] = c.tunnel
			values["CHECKNUMBER_PUBLIC_URL"] = c.publicURL

			cfg, err := Load()
			if c.wantBad != "" {
				if err == nil {
					t.Fatalf("Load accepted CHECKNUMBER_TUNNEL=%q with CHECKNUMBER_PUBLIC_URL=%q", c.tunnel, c.publicURL)
				}
				if !strings.Contains(err.Error(), c.wantBad) {
					t.Errorf("error %q does not contain %q", err, c.wantBad)
				}
				assertNoValue(t, err.Error(), values)
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Tunnel != c.tunnel {
				t.Errorf("Tunnel = %q, want %q", cfg.Tunnel, c.tunnel)
			}
			if cfg.PublicURL != c.wantPublicURL {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, c.wantPublicURL)
			}
		})
	}
}

// assertNoValue fails when msg contains any setting value (C-3, C-4).
func assertNoValue(t *testing.T, msg string, values map[string]string) {
	t.Helper()
	for name, v := range values {
		if v != "" && strings.Contains(msg, v) {
			t.Errorf("error %q contains the value of %s", msg, name)
		}
	}
}

// TestLogFileSetting: CHECKNUMBER_LOG_FILE is optional, its value is taken
// as it is, and no value of it stops Load, not even a path that cannot be a
// file: the app must still start (R-29, TC-44, AD-7).
func TestLogFileSetting(t *testing.T) {
	underFile := filepath.Join(t.TempDir(), "plain-file-test")
	if err := os.WriteFile(underFile, nil, 0o600); err != nil {
		t.Fatalf("write %s: %v", underFile, err)
	}
	// The folder does not exist: Load does not create or check it.
	logPath := filepath.Join(t.TempDir(), "logs-test", "checknumber.log")
	pathUnderFile := filepath.Join(underFile, "checknumber.log")
	cases := []struct {
		name, value, want string
		// notSet removes the variable instead of setting it to value.
		notSet bool
	}{
		{"not set", "", "", true},
		{"empty", "", "", false},
		{"only spaces", "   ", "", false},
		{"a path", logPath, logPath, false},
		{"a path under a file", pathUnderFile, pathUnderFile, false},
		{"a relative path", "relative-test.log", "relative-test.log", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setAll(t)
			// Setenv first, so the variable is restored after the test.
			t.Setenv("CHECKNUMBER_LOG_FILE", c.value)
			if c.notSet {
				if err := os.Unsetenv("CHECKNUMBER_LOG_FILE"); err != nil {
					t.Fatalf("unset CHECKNUMBER_LOG_FILE: %v", err)
				}
			}
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load with CHECKNUMBER_LOG_FILE=%q: %v", c.value, err)
			}
			if cfg.LogFile != c.want {
				t.Errorf("LogFile = %q, want %q", cfg.LogFile, c.want)
			}
		})
	}
}

func TestAddrsLocalOnly(t *testing.T) {
	setAll(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.LocalAddr(), "127.0.0.1:"+testPort; got != want {
		t.Errorf("LocalAddr = %q, want %q", got, want)
	}
	if got, want := cfg.EventsAddr(), "127.0.0.1:"+testEventsPort; got != want {
		t.Errorf("EventsAddr = %q, want %q", got, want)
	}
}

func TestRunningGuideListsSettings(t *testing.T) {
	raw, err := os.ReadFile(runningGuide)
	if err != nil {
		t.Fatalf("read %s: %v", runningGuide, err)
	}
	guide := string(raw)

	names := append(append(append([]string{}, requiredSettings...), optionalSettings...), fileShortNames...)
	for _, name := range names {
		if !strings.Contains(guide, "`"+name+"`") {
			t.Errorf("guide does not name setting %s", name)
		}
	}

	// The tunnel is pointed at the events port only (AD-6).
	tunnels := 0
	for _, line := range strings.Split(guide, "\n") {
		if !strings.Contains(line, tunnelCommand) {
			continue
		}
		tunnels++
		if !strings.Contains(line, eventsPortVar) || strings.Contains(line, localPortVar) {
			t.Errorf("tunnel line %q must use %s and never %s", line, eventsPortVar, localPortVar)
		}
	}
	if tunnels == 0 {
		t.Errorf("guide has no %q command", tunnelCommand)
	}

	// The M-1 list covers every result a test call can give.
	_, m1, found := strings.Cut(guide, m1Heading)
	if !found {
		t.Fatalf("guide has no %q section", m1Heading)
	}
	for _, code := range result.All() {
		if !strings.Contains(m1, result.Label(code)) {
			t.Errorf("M-1 test calls do not mention the result %q", result.Label(code))
		}
	}

	// No secret value (C-4): secrets are never assigned, and nothing looks
	// like a pasted key.
	for _, name := range secretSettings {
		if strings.Contains(guide, name+"=") || strings.Contains(guide, name+":") {
			t.Errorf("guide assigns a value to %s", name)
		}
	}
	if tok := longToken.FindString(guide); tok != "" {
		t.Errorf("guide contains a key-shaped value %q", tok)
	}
}
