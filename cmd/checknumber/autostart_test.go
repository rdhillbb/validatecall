package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The start-at-login scripts (R-26, TC-33) are tested on copies in a temporary
// folder laid out like the repository. The scripts find the repository from
// their own place, so what they build, start or name stays in that folder:
// the program is a fake, the port registry is a fake, HOME and the
// LaunchAgents folder are temporary, and install and remove always run with
// the no-load switch, so the real launchctl is never called.
const (
	scriptsSource   = "../../scripts"
	scriptsFolder   = "scripts"
	runScript       = "checknumber-run.sh"
	autostartScript = "checknumber-autostart.sh"
	startGuide      = "../../docs/RUNNING.md"

	commandPrint   = "print"
	commandInstall = "install"
	commandRemove  = "remove"

	// Independent of the scripts' own constants so a changed name, value or
	// path fails a test.
	programPath      = "bin/checknumber"
	wantLabel        = "com.checknumber.app"
	wantPlistName    = "com.checknumber.app.plist"
	wantShell        = "/bin/zsh"
	wantShellFlags   = "-ic"
	wantStartCommand = "exec "
	// The shell, its flags and the one command.
	wantArguments     = 3
	wantLogFile       = "Library/Logs/checknumber/checknumber.log"
	wantStartLogFile  = "Library/Logs/checknumber/checknumber-start.log"
	wantLogFolder     = "Library/Logs/checknumber"
	wantDatabase      = "checknumber"
	wantEnvFile       = "env.apikeys"
	wantRecordings    = "checknumber-recordings"
	registryPageName  = "checknumber"
	registryEventName = "checknumber-events"

	envPort          = "CHECKNUMBER_PORT"
	envEventsPort    = "CHECKNUMBER_EVENTS_PORT"
	envDatabase      = "PGDATABASE"
	envFile          = "CHECKNUMBER_ENV_FILE"
	envRecordingsDir = "CHECKNUMBER_RECORDINGS_DIR"
	envTunnel        = "CHECKNUMBER_TUNNEL"
	envLogFile       = "CHECKNUMBER_LOG_FILE"

	// The switches of the scripts.
	switchRegistry  = "PORT_REGISTRY"
	switchAgentsDir = "LAUNCH_AGENTS_DIR"
	switchNoLoad    = "CHECKNUMBER_NO_LOAD"
	noLoadOn        = "1"

	// What the fake registry answers. The values only pass through the start
	// script to the fake program; no listener is opened on them.
	fakePagePort   = "61011"
	fakeEventsPort = "61012"
	stalePort      = "61099"

	// Values already set before the start script runs; it must keep them.
	keptDatabase = "kept_database"
	keptTunnel   = "kept-tunnel-value"
	keptLogFile  = "kept-log-folder/kept.log"

	startedMarker = "fake-checknumber-started"
	builtMarker   = "fake-checknumber-built"

	fakeRegistry = `#!/bin/sh
[ "$1" = get ] || exit 2
case "$2" in
` + registryPageName + `) echo ` + fakePagePort + ` ;;
` + registryEventName + `) echo ` + fakeEventsPort + ` ;;
*) exit 3 ;;
esac
`
	// The module install builds in the fake repository: a program that only
	// prints builtMarker, in the place of the real one.
	fakeModule      = "module fakechecknumber\n"
	fakeProgramMain = "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"" + builtMarker + "\") }\n"

	startAtLoginHeading = "## Start at login"
	nextHeading         = "\n## "
	guideInstallCommand = "scripts/checknumber-autostart.sh install"
	guideRemoveCommand  = "scripts/checknumber-autostart.sh remove"
	guideLogFile        = "~/" + wantLogFile
	// The guide names the start log by its file name and its folder in one
	// phrase. Written as one path, "/Library/Logs/checknumber/checknumber-start"
	// is a 43-character run that the guide's key check (C-4, see keyShaped)
	// reads as a pasted key.
	startLogName      = "checknumber-start.log"
	guideStartLogFile = "`" + startLogName + "` in `~/" + wantLogFolder + "/`"
	// R-30: the size at which the log is set aside and how many older files
	// are kept, as the guide words them.
	guideSetAsideSize = "2 MB"
	guideKeptFiles    = "5 older"

	folderMode     fs.FileMode = 0o755
	executableMode fs.FileMode = 0o755
	plainMode      fs.FileMode = 0o644
	anyExecuteBit  fs.FileMode = 0o111

	// commandTimeout bounds one script or tool run; install compiles a small
	// program.
	commandTimeout = 2 * time.Minute
)

// passedSettings are what the start script must give the program (DD-1).
var passedSettings = []string{envPort, envEventsPort, envDatabase, envFile, envRecordingsDir, envTunnel, envLogFile}

// secretNames must not appear in a script or in the plist at all (C-3, C-4):
// the Telnyx keys under both of their names, the check API key and the
// database password.
var secretNames = []string{"TELNYX_API_KEY", "TELNYX_PUBLIC_KEY", "APIKEY", "PUBLICKEY", "CHECKNUMBER_API_KEY", "PGPASSWORD"}

var (
	// portShaped finds a written-out port: the registry issues ports of four
	// and five digits; three digits also catches a low port such as the HTTPS
	// one. The scripts and the plist hold no other number that long.
	portShaped = regexp.MustCompile(`[0-9]{3,}`)
	// keyShaped finds anything shaped like a pasted key: a run of 40 or more
	// base64 or key characters.
	keyShaped = regexp.MustCompile(`[A-Za-z0-9+/=_-]{40,}`)
)

// launchAgent is the part of the plist the tests read.
type launchAgent struct {
	Label             string
	ProgramArguments  []string
	RunAtLoad         bool
	KeepAlive         bool
	StandardOutPath   string
	StandardErrorPath string
}

func writeFile(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), folderMode); err != nil {
		t.Fatalf("create folder of %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	// WriteFile's mode is cut by the umask; an executable must stay one.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
}

// fakeRepo copies the two scripts, with their modes, into a new folder named
// name and returns its path as the scripts see it (symbolic links resolved).
func fakeRepo(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	for _, script := range []string{runScript, autostartScript} {
		source := filepath.Join(scriptsSource, script)
		info, err := os.Stat(source)
		if err != nil {
			t.Fatalf("stat %s: %v", source, err)
		}
		if info.Mode().Perm()&anyExecuteBit == 0 {
			t.Fatalf("%s is not executable (mode %v); the LaunchAgent starts it directly", source, info.Mode().Perm())
		}
		content, err := os.ReadFile(source)
		if err != nil {
			t.Fatalf("read %s: %v", source, err)
		}
		writeFile(t, filepath.Join(root, scriptsFolder, script), string(content), info.Mode().Perm())
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve %s: %v", root, err)
	}
	return resolved
}

// fakeProgram puts a program in the place of bin/checknumber that prints
// startedMarker and the seven settings it was given. It prints nothing else of
// its environment, so no value of the operator's shell reaches a test log.
func fakeProgram(t *testing.T, repo string) {
	t.Helper()
	var source strings.Builder
	source.WriteString("#!/bin/sh\necho " + startedMarker + "\n")
	for _, name := range passedSettings {
		source.WriteString(`printf '%s=%s\n' ` + name + ` "$` + name + `"` + "\n")
	}
	writeFile(t, filepath.Join(repo, programPath), source.String(), executableMode)
}

// scriptEnv is the whole environment of one script run: the test's PATH, a
// temporary HOME and the given NAME=value pairs. Nothing else of the test's
// own environment is passed on.
func scriptEnv(home string, pairs ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}, pairs...)
}

// startEnv is scriptEnv with a new fake port registry.
func startEnv(t *testing.T, home string, pairs ...string) []string {
	t.Helper()
	registry := filepath.Join(t.TempDir(), "port-registry")
	writeFile(t, registry, fakeRegistry, executableMode)
	return scriptEnv(home, append([]string{switchRegistry + "=" + registry}, pairs...)...)
}

func runCommand(t *testing.T, env []string, name string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// mustRun is runCommand for a run that has to succeed; it returns the output.
func mustRun(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	stdout, stderr, err := runCommand(t, env, name, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\noutput: %s\nerrors: %s", name, args, err, stdout, stderr)
	}
	return stdout
}

// givenSettings reads the NAME=value lines of the fake program; ok is false
// when the fake program did not run.
func givenSettings(output string) (settings map[string]string, ok bool) {
	settings = map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		if line == startedMarker {
			ok = true
			continue
		}
		if name, value, found := strings.Cut(line, "="); found {
			settings[name] = value
		}
	}
	return settings, ok
}

func TestRunScript(t *testing.T) {
	t.Run("ports from the registry and the five defaults", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()
		fakeProgram(t, repo)

		output := mustRun(t, startEnv(t, home), filepath.Join(repo, scriptsFolder, runScript))
		t.Logf("environment printed by the fake program:\n%s", output)

		got, started := givenSettings(output)
		if !started {
			t.Fatalf("the start script did not start the program; output: %q", output)
		}
		recordings := filepath.Join(home, wantRecordings)
		want := map[string]string{
			envPort:          fakePagePort,
			envEventsPort:    fakeEventsPort,
			envDatabase:      wantDatabase,
			envFile:          filepath.Join(repo, wantEnvFile),
			envRecordingsDir: recordings,
			envTunnel:        tunnelSetting,
			envLogFile:       filepath.Join(home, wantLogFile),
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("settings given to the program = %v, want %v", got, want)
		}
		if info, err := os.Stat(recordings); err != nil || !info.IsDir() {
			t.Errorf("the default recordings folder %s was not created: %v", recordings, err)
		}
	})

	t.Run("a value already set is kept, a port is not", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()
		fakeProgram(t, repo)
		keptEnvFile := filepath.Join(t.TempDir(), "kept-settings-file")
		keptRecordings := filepath.Join(t.TempDir(), "kept-recordings")
		env := startEnv(t, home,
			envDatabase+"="+keptDatabase,
			envFile+"="+keptEnvFile,
			envRecordingsDir+"="+keptRecordings,
			envTunnel+"="+keptTunnel,
			envLogFile+"="+keptLogFile,
			envPort+"="+stalePort,
			envEventsPort+"="+stalePort,
		)

		output := mustRun(t, env, filepath.Join(repo, scriptsFolder, runScript))

		got, started := givenSettings(output)
		if !started {
			t.Fatalf("the start script did not start the program; output: %q", output)
		}
		want := map[string]string{
			envPort:          fakePagePort,
			envEventsPort:    fakeEventsPort,
			envDatabase:      keptDatabase,
			envFile:          keptEnvFile,
			envRecordingsDir: keptRecordings,
			envTunnel:        keptTunnel,
			envLogFile:       keptLogFile,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("settings given to the program = %v, want %v", got, want)
		}
		if _, err := os.Stat(filepath.Join(home, wantRecordings)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the default recordings folder was created although another folder was set: %v", err)
		}
	})

	t.Run("explicitly empty settings are kept", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()
		fakeProgram(t, repo)
		env := startEnv(t, home,
			envDatabase+"=",
			envFile+"=",
			envRecordingsDir+"=",
			envTunnel+"=",
			envLogFile+"=",
		)

		output := mustRun(t, env, filepath.Join(repo, scriptsFolder, runScript))
		got, started := givenSettings(output)
		if !started {
			t.Fatalf("the start script did not start the program; output: %q", output)
		}
		want := map[string]string{
			envPort:          fakePagePort,
			envEventsPort:    fakeEventsPort,
			envDatabase:      "",
			envFile:          "",
			envRecordingsDir: "",
			envTunnel:        "",
			envLogFile:       "",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("settings given to the program = %v, want %v", got, want)
		}
		if _, err := os.Stat(filepath.Join(home, wantRecordings)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the default recordings folder was created although the setting was explicitly empty: %v", err)
		}
	})

	t.Run("Telnyx variables of the shell are not passed on", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()
		// This fake program prints only how many TELNYX_ variables it was
		// given, never a name or a value.
		program := "#!/bin/sh\necho " + startedMarker + "\nenv | awk '/^TELNYX_/{n++} END{print \"telnyx-count=\" n+0}'\n"
		writeFile(t, filepath.Join(repo, programPath), program, executableMode)
		env := startEnv(t, home,
			"TELNYX_API_KEY=shell-key",
			"TELNYX_PUBLIC_KEY=shell-public-key",
			"TELNYX_CONNECTION_ID=shell-application",
			"TELNYX_FROM_NUMBER=shell-number",
			"TELNYX_TO_NUMBER=shell-other",
		)

		output := mustRun(t, env, filepath.Join(repo, scriptsFolder, runScript))

		got, started := givenSettings(output)
		if !started {
			t.Fatalf("the start script did not start the program; output: %q", output)
		}
		if got["telnyx-count"] != "0" {
			t.Errorf("TELNYX_ variables given to the program = %q, want 0: the Telnyx settings must come from the settings file only", got["telnyx-count"])
		}
	})

	t.Run("stops with a message when the program is missing", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()

		stdout, stderr, err := runCommand(t, startEnv(t, home), filepath.Join(repo, scriptsFolder, runScript))

		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("start script without a program: error = %v, want a failing exit status", err)
		}
		if program := filepath.Join(repo, programPath); !strings.Contains(stderr, program) || !strings.Contains(stderr, "missing") {
			t.Errorf("message = %q, want it to say that %s is missing", stderr, program)
		}
		if stdout != "" {
			t.Errorf("output = %q, want none", stdout)
		}
	})

	t.Run("stops with a message when the registry is missing", func(t *testing.T) {
		repo, home := fakeRepo(t, "repo"), t.TempDir()
		fakeProgram(t, repo)
		registry := filepath.Join(t.TempDir(), "no-registry-here")

		stdout, stderr, err := runCommand(t, scriptEnv(home, switchRegistry+"="+registry), filepath.Join(repo, scriptsFolder, runScript))

		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("start script without a registry: error = %v, want a failing exit status", err)
		}
		if !strings.Contains(stderr, registry) || !strings.Contains(stderr, "missing") {
			t.Errorf("message = %q, want it to say that %s is missing", stderr, registry)
		}
		if _, started := givenSettings(stdout); started {
			t.Errorf("the program was started without a port registry; output: %q", stdout)
		}
	})
}

// lintPlist writes plist into a file, has plutil check it and returns the
// file's path.
func lintPlist(t *testing.T, plist string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), wantPlistName)
	writeFile(t, path, plist, plainMode)
	mustRun(t, os.Environ(), "plutil", "-lint", path)
	return path
}

func readAgent(t *testing.T, plistPath string) launchAgent {
	t.Helper()
	converted := mustRun(t, os.Environ(), "plutil", "-convert", "json", "-o", "-", plistPath)
	var agent launchAgent
	if err := json.Unmarshal([]byte(converted), &agent); err != nil {
		t.Fatalf("read the plist as JSON: %v\n%s", err, converted)
	}
	return agent
}

func TestAutostartPrint(t *testing.T) {
	tests := []struct {
		name   string
		folder string
		// plain: the folder name needs no quoting, so the command is known
		// letter for letter.
		plain bool
	}{
		{name: "plain path", folder: "repo", plain: true},
		{name: "path with a space and an ampersand", folder: "check number & co"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo, home := fakeRepo(t, tc.folder), t.TempDir()

			plist := mustRun(t, scriptEnv(home), filepath.Join(repo, scriptsFolder, autostartScript), commandPrint)
			agent := readAgent(t, lintPlist(t, plist))

			if agent.Label != wantLabel {
				t.Errorf("Label = %q, want %q", agent.Label, wantLabel)
			}
			if !agent.RunAtLoad {
				t.Error("RunAtLoad is not true: the app would not start at login")
			}
			if !agent.KeepAlive {
				t.Error("KeepAlive is not true: the app would not start again when it stops")
			}
			// The app's own log has one writer, the app (TC-45): the login
			// service writes into the start log.
			startLog := filepath.Join(home, wantStartLogFile)
			if agent.StandardOutPath != startLog || agent.StandardErrorPath != startLog {
				t.Errorf("output path = %q, error path = %q, want both %q", agent.StandardOutPath, agent.StandardErrorPath, startLog)
			}
			if len(agent.ProgramArguments) != wantArguments {
				t.Fatalf("ProgramArguments = %q, want the shell, its flags and one command", agent.ProgramArguments)
			}
			if agent.ProgramArguments[0] != wantShell || agent.ProgramArguments[1] != wantShellFlags {
				t.Errorf("ProgramArguments start with %q %q, want %q %q", agent.ProgramArguments[0], agent.ProgramArguments[1], wantShell, wantShellFlags)
			}
			command := agent.ProgramArguments[2]
			if want := wantStartCommand + filepath.Join(repo, scriptsFolder, runScript); tc.plain && command != want {
				t.Errorf("command = %q, want %q", command, want)
			}

			// The three arguments, run as launchd runs them, start this
			// repository's start script (and through it the fake program).
			fakeProgram(t, repo)
			output := mustRun(t, startEnv(t, home), agent.ProgramArguments[0], agent.ProgramArguments[1:]...)
			got, started := givenSettings(output)
			if !started {
				t.Fatalf("the plist's command did not start the program; output: %q", output)
			}
			if want := filepath.Join(repo, wantEnvFile); got[envFile] != want {
				t.Errorf("%s = %q, want %q: another start script ran", envFile, got[envFile], want)
			}
		})
	}
}

func TestAutostartInstallRemove(t *testing.T) {
	repo, home := fakeRepo(t, "repo"), t.TempDir()
	writeFile(t, filepath.Join(repo, "go.mod"), fakeModule, plainMode)
	writeFile(t, filepath.Join(repo, "cmd", "checknumber", "main.go"), fakeProgramMain, plainMode)
	agentsDir := filepath.Join(t.TempDir(), "LaunchAgents")

	// A fake launchctl in front of the real one on the PATH: it only writes
	// down that it was called.
	fakeBin := t.TempDir()
	launchctlCalls := filepath.Join(t.TempDir(), "launchctl-calls")
	fakeLaunchctl := filepath.Join(fakeBin, "launchctl")
	writeFile(t, fakeLaunchctl, "#!/bin/sh\necho \"$@\" >> '"+launchctlCalls+"'\n", executableMode)

	// HOME is temporary, so the build cache is named: without it the fake
	// program would be compiled from nothing.
	buildCache := strings.TrimSpace(mustRun(t, os.Environ(), "go", "env", "GOCACHE"))
	env := []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"GOCACHE=" + buildCache,
		switchAgentsDir + "=" + agentsDir,
		switchNoLoad + "=" + noLoadOn,
	}
	// A call of launchctl by the script would reach the fake, so a missing
	// call record means no call.
	if found := strings.TrimSpace(mustRun(t, env, wantShell, "-c", "command -v launchctl")); found != fakeLaunchctl {
		t.Fatalf("launchctl on the scripts' PATH is %q, want the fake %q", found, fakeLaunchctl)
	}
	script := filepath.Join(repo, scriptsFolder, autostartScript)
	plistPath := filepath.Join(agentsDir, wantPlistName)

	mustRun(t, env, script, commandInstall)

	if built := strings.TrimSpace(mustRun(t, scriptEnv(home), filepath.Join(repo, programPath))); built != builtMarker {
		t.Errorf("the built program printed %q, want %q", built, builtMarker)
	}
	written, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("install wrote no plist: %v", err)
	}
	if printed := mustRun(t, env, script, commandPrint); string(written) != printed {
		t.Errorf("installed plist = %q, want what print gives: %q", written, printed)
	}
	if agent := readAgent(t, lintPlist(t, string(written))); agent.Label != wantLabel {
		t.Errorf("installed plist has Label %q, want %q", agent.Label, wantLabel)
	}
	if info, err := os.Stat(filepath.Join(home, wantLogFolder)); err != nil || !info.IsDir() {
		t.Errorf("install did not create the log folder: %v", err)
	}
	if calls, err := os.ReadFile(launchctlCalls); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("install called launchctl with the no-load switch on: %q (%v)", calls, err)
	}

	mustRun(t, env, script, commandRemove)

	if _, err := os.Stat(plistPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("remove left the plist %s: %v", plistPath, err)
	}
	if calls, err := os.ReadFile(launchctlCalls); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("remove called launchctl with the no-load switch on: %q (%v)", calls, err)
	}
}

func TestAutostartNoSecretsNoPorts(t *testing.T) {
	repo, home := fakeRepo(t, "repo"), t.TempDir()
	plist := mustRun(t, scriptEnv(home), filepath.Join(repo, scriptsFolder, autostartScript), commandPrint)
	// The two temporary folders are not part of what is checked; their names
	// hold digits. Nor is the start log's path, which TestAutostartPrint pins
	// letter for letter: its folders and the name checknumber-start run to
	// more than 40 key characters.
	texts := map[string]string{
		"the plist": strings.NewReplacer(filepath.Join(home, wantStartLogFile), "<start log>", repo, "<repo>", home, "<home>").Replace(plist),
	}
	for _, script := range []string{runScript, autostartScript} {
		content, err := os.ReadFile(filepath.Join(scriptsSource, script))
		if err != nil {
			t.Fatalf("read %s: %v", script, err)
		}
		texts[script] = string(content)
	}

	for name, text := range texts {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty; nothing was checked", name)
		}
		if port := portShaped.FindString(text); port != "" {
			t.Errorf("%s holds a number shaped like a port: %q", name, port)
		}
		if key := keyShaped.FindString(text); key != "" {
			t.Errorf("%s holds a key-shaped value: %q", name, key)
		}
		for _, secret := range secretNames {
			if strings.Contains(text, secret) {
				t.Errorf("%s names the secret setting %s", name, secret)
			}
		}
	}
}

func TestRunningGuideStartAtLogin(t *testing.T) {
	raw, err := os.ReadFile(startGuide)
	if err != nil {
		t.Fatalf("read %s: %v", startGuide, err)
	}
	_, section, found := strings.Cut(string(raw), startAtLoginHeading+"\n")
	if !found {
		t.Fatalf("guide has no %q section", startAtLoginHeading)
	}
	section, _, _ = strings.Cut(section, nextHeading)

	// The file name and folder the guide names are exactly the start log the
	// plist writes to (TestAutostartPrint).
	if got := wantLogFolder + "/" + startLogName; got != wantStartLogFile {
		t.Fatalf("the guide's start log %q is not the plist's %q", got, wantStartLogFile)
	}
	for _, want := range []string{guideInstallCommand, guideRemoveCommand, guideLogFile, guideStartLogFile, guideSetAsideSize, guideKeptFiles} {
		if !strings.Contains(section, want) {
			t.Errorf("the %q section does not name %q", startAtLoginHeading, want)
		}
	}
	// The guide as a whole holds nothing key-shaped (C-4); the section must
	// not be the first to break that.
	if key := keyShaped.FindString(section); key != "" {
		t.Errorf("the %q section holds a key-shaped value %q", startAtLoginHeading, key)
	}
}
