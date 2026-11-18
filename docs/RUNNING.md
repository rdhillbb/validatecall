# Running checknumber (Stage 1, the operator's Mac)

This guide starts checknumber on the Mac (TS-6), reached by Telnyx through a
Cloudflare quick tunnel (TC-28), and lists the test calls for the manual
acceptance of milestone M-1 (TC-26).

There are two ways to run it. Steps 1 to 5 start it by hand, with the tunnel
started by hand in step 2. "Start at login" has macOS start it at every login;
the app then starts the tunnel itself.

Two listeners, both on `127.0.0.1` only (AD-6):

| Listener | Setting | What answers there | Reached from outside? |
|---|---|---|---|
| Page and API | `CHECKNUMBER_PORT` (registry name `checknumber`) | the test page `/` and the check API `/api/check`, `/api/history` | no, the Mac only |
| Events | `CHECKNUMBER_EVENTS_PORT` (registry name `checknumber-events`) | Telnyx call events at `/telnyx/events` | yes, through the tunnel |

The tunnel points at the events port only. Never point a tunnel at the page
and API port: anyone who reached it could place paid calls.

## Rules for the settings

- Secret values are never written into a file that git tracks, a script or a
  note (C-4). The Telnyx keys are read from one local settings file that git
  ignores and only its owner can read (R-25), or typed at a hidden prompt in
  the terminal that runs the app.
- Ports come from the port registry (C-2). Do not type a port number.
- Database settings come from `~/.zshrc` (C-3). Only the database name is set
  here, and the app does not start without it (D-7).
- The commands are for zsh, the Mac's default shell.

## 1. Get the ports

```bash
export CHECKNUMBER_PORT=$(get_port checknumber)
export CHECKNUMBER_EVENTS_PORT=$(get_port checknumber-events)
```

`get_port` is a shell function from `~/.zshrc`. If it is not loaded, use
`~/bin/port_registry.py get checknumber` and
`~/bin/port_registry.py get checknumber-events` instead.

## 2. Start the tunnel (events port only)

In a second terminal (run step 1 there too):

```bash
cloudflared tunnel --url http://127.0.0.1:$CHECKNUMBER_EVENTS_PORT
```

It prints an address ending in `trycloudflare.com`. Leave it running. The
address changes every time the tunnel starts (TC-13), so set it again after
every tunnel restart.

## 3. Set the settings

Back in the first terminal:

```bash
# Tunnel address printed in step 2, starting with https://
read -r 'CHECKNUMBER_PUBLIC_URL?Tunnel address: ' && export CHECKNUMBER_PUBLIC_URL

# Folder for the 10-second recordings, outside the repository (TC-22)
mkdir -p "$HOME/checknumber-recordings"
export CHECKNUMBER_RECORDINGS_DIR="$HOME/checknumber-recordings"

# Database: only the name; host, user and password come from ~/.zshrc
export PGDATABASE=checknumber
```

### The Telnyx settings, from the settings file (R-25)

The four Telnyx settings are read from one local file, so nobody types them
at start. On this Mac the file is `env.apikeys` in the repository folder; git
ignores it. From the repository folder:

```bash
chmod 600 env.apikeys
export CHECKNUMBER_ENV_FILE="$PWD/env.apikeys"
```

- One setting per line, written `NAME:VALUE` or `NAME=VALUE`. Spaces around
  the name and around the value are dropped.
- Blank lines, lines starting with `#` and lines with any other name are
  skipped.
- Each setting has two names; the file may use either (TC-31):

| Setting | Short name in the file |
|---|---|
| `TELNYX_API_KEY` | `APIKEY` |
| `TELNYX_PUBLIC_KEY` | `PUBLICKEY` |
| `TELNYX_CONNECTION_ID` | `APPLICATIONID` |
| `TELNYX_FROM_NUMBER` | `FROMNUMBER` |

- Only the owner may have access to the file (C-4). The app does not start
  when the file is missing, is not a regular file, or gives group or others
  any access; `chmod 600` above cures the last one. The message names the
  file's path and the cause, never a value from the file.
- An environment variable of the `TELNYX_` name wins over the file. The start
  script of "Start at login" removes every `TELNYX_` variable first, so the
  app started at login always reads the four from the file.

### The Telnyx settings, typed instead

Without a settings file, leave `CHECKNUMBER_ENV_FILE` unset and type the
four values (TC-27), fetched live from the Telnyx portal; typed, never saved:

```bash
read -rs 'TELNYX_API_KEY?Telnyx API key: ' && export TELNYX_API_KEY && echo
read -rs 'TELNYX_PUBLIC_KEY?Telnyx public key (base64): ' && export TELNYX_PUBLIC_KEY && echo
read -r 'TELNYX_FROM_NUMBER?Telnyx number to call from (+1...): ' && export TELNYX_FROM_NUMBER
read -r 'TELNYX_CONNECTION_ID?Telnyx Call Control application id: ' && export TELNYX_CONNECTION_ID
```

### All settings

| Setting | Required | What it is |
|---|---|---|
| `CHECKNUMBER_PORT` | yes | port of the page and API listener, from the registry |
| `CHECKNUMBER_EVENTS_PORT` | yes | port of the events listener, from the registry |
| `CHECKNUMBER_PUBLIC_URL` | yes, unless `CHECKNUMBER_TUNNEL` is `cloudflared` | the tunnel's `https://` address; Telnyx is sent `<address>/telnyx/events` on each call |
| `CHECKNUMBER_RECORDINGS_DIR` | yes | existing folder for recordings |
| `CHECKNUMBER_ENV_FILE` | no | path of the settings file that holds the four Telnyx settings (R-25, TC-31) |
| `TELNYX_API_KEY` | yes, here or in the settings file | Telnyx API key (secret) |
| `TELNYX_PUBLIC_KEY` | yes, here or in the settings file | Telnyx public key for event signatures, base64 (secret by house rule) |
| `TELNYX_FROM_NUMBER` | yes, here or in the settings file | a Telnyx number on the operator's account (C-7) |
| `TELNYX_CONNECTION_ID` | yes, here or in the settings file | id of the Telnyx Call Control application |
| `CHECKNUMBER_TUNNEL` | no | empty or `cloudflared`; any other value is refused. With `cloudflared` the app starts the tunnel itself (the `cloudflared` program must be on the PATH), reads the address from it and stops when the tunnel stops, so `CHECKNUMBER_PUBLIC_URL` is not needed (TC-32); the start script of "Start at login" sets this value. Leave it empty when the tunnel is started by hand as in step 2 |
| `CHECKNUMBER_CHECK_LIMIT` | no | time limit of one check, for example `90s`; default `120s` (TC-18) |
| `CHECKNUMBER_COMBINE_DISCONNECTED` | no | `true` reports "Disconnected / Unallocated" instead of the two separate results (R-23); default `false` |
| `CHECKNUMBER_API_KEY` | no | shared key the fax system sends with each check API request in M-2 (AD-6, secret); when set, `/api/check` and `/api/history` answer 401 without `Authorization: Bearer <key>`; the test page never needs it |
| `PGDATABASE` | yes | database name, `checknumber`; without it the database driver would use the database named after the user, so the app refuses to start (D-7) |
| `CHECKNUMBER_LOG_FILE` | no | path of the app's log file. When set, every log line (start-up, Telnyx events, errors, one line per check) goes into that file instead of standard error; a missing folder is created. At 2 MB the file is set aside as `.1` (older ones move up to `.5`, the oldest is deleted) and a new one is started (R-30). If the file cannot be opened, the app says so on standard error, logs there and still starts. Not set or empty: log lines go to standard error, as in a start by hand (TC-44) |

Only when the fax system calls the API (M-2), set the shared key the same
way as the Telnyx keys: fetched live, typed at a hidden prompt, never saved.

```bash
read -rs 'CHECKNUMBER_API_KEY?Check API key: ' && export CHECKNUMBER_API_KEY && echo
```

If a setting is missing or wrong, the app stops at once and names every bad
setting. It never prints a value; of a refused settings file it prints the
path and the cause.

The database `checknumber` must exist. If it does not:

```bash
psql -d postgres -c "CREATE DATABASE checknumber"
```

The app creates its table itself.

## 4. Run

From the repository folder:

```bash
go run ./cmd/checknumber
```

At start-up any check that was still in progress when the app last stopped is
marked "Unknown", reason "app restarted", so the next request for that number
dials again (TC-20, R-21). Stop the app with Ctrl-C.

## 5. Open the test page

```bash
open "http://127.0.0.1:$CHECKNUMBER_PORT/"
```

Type one number, press Check, and wait for the result (up to the check
limit).

## Start at login

The app can start by itself when the operator logs in, and start again
whenever it stops (R-26). macOS does this with a LaunchAgent. It starts at
login, not when the Mac boots: before login the settings in `~/.zshrc` and the
user's files are not available (TC-33). After a restart of the Mac, log in;
nothing has to be typed.

Before installing:

- the settings file is in place with `chmod 600`, as in step 3;
- the database exists, as in step 3;
- the `go` and `cloudflared` programs are on the PATH that `~/.zshrc` sets.

Install, from the repository folder:

```bash
scripts/checknumber-autostart.sh install
```

It builds the program `bin/checknumber`, writes the LaunchAgent
`~/Library/LaunchAgents/com.checknumber.app.plist` and loads it. The app
starts at once and at every login. After the code changes, run the same
command again: it builds the program again and restarts the app.

macOS starts `scripts/checknumber-run.sh` through `zsh -ic`, so `~/.zshrc`
supplies the database connection and the PATH. The start script takes both
ports from the port registry and sets five settings. A value that `~/.zshrc`
already exports is kept, including an empty value. An explicitly empty
`CHECKNUMBER_TUNNEL` disables the automatic tunnel; supply
`CHECKNUMBER_PUBLIC_URL` as in the manual steps in that case.

| Setting | Set by the start script to |
|---|---|
| `PGDATABASE` | `checknumber` |
| `CHECKNUMBER_ENV_FILE` | `env.apikeys` in the repository folder |
| `CHECKNUMBER_RECORDINGS_DIR` | `~/checknumber-recordings`, created when it is missing |
| `CHECKNUMBER_TUNNEL` | `cloudflared`: the app starts the tunnel itself, so step 2 and `CHECKNUMBER_PUBLIC_URL` are not needed |
| `CHECKNUMBER_LOG_FILE` | `~/Library/Logs/checknumber/checknumber.log`, the app's log |

It also removes every `TELNYX_` variable, so the four Telnyx settings come
from the settings file only (R-31).

No port, key or password is written in the scripts or in the LaunchAgent
(C-2, C-3, C-4).

There are two log files:

- The app's log, `~/Library/Logs/checknumber/checknumber.log`: every line the
  app writes (start-up, Telnyx events, errors, one line per check). When it
  reaches 2 MB the app sets it aside as `checknumber.log.1` and starts a new
  one; the 5 older files `.1` to `.5` are kept and the oldest is deleted
  (R-30).
- The start log, `checknumber-start.log` in `~/Library/Logs/checknumber/`:
  what the app and the start script write to standard error. That is what comes
  before the app opens its own log (a missing program, a refused setting), a
  crash, a warning when setting the app's log aside fails, and, when the
  app's log cannot be opened, every line of the app. It is not set aside.

```bash
tail -f ~/Library/Logs/checknumber/checknumber.log
```

When the app cannot start (a setting is missing, the program is not built),
the start log names the cause and macOS tries again, at most once every 10
seconds.

While it is installed, do not also start the app by hand (step 4): both would
ask for the same two ports. Open the test page as in step 5, after step 1 in
that terminal.

Remove it, from the repository folder:

```bash
scripts/checknumber-autostart.sh remove
```

It unloads the LaunchAgent, which stops the app, and deletes
`~/Library/LaunchAgents/com.checknumber.app.plist`. The program, the logs and
the recordings stay.

`scripts/checknumber-autostart.sh print` shows the LaunchAgent without
installing anything.

## M-1 test calls

The operator's manual acceptance of M-1. Automated tests never place real
calls (C-5); these are the only real ones. The known test numbers are still to
be supplied (product description, open question 12). Write down each result.

| # | Number to call | Expected result |
|---|---|---|
| 1 | a known fax number | Fax |
| 2 | a number with an answering message | Voice message, with a recording of at most 10 seconds and its text with a Copy button |
| 3 | a phone a person answers (for example the operator's own mobile) | Person answered; no recording exists for the call |
| 4 | a disconnected number | Disconnected |
| 5 | a number that rings and nobody answers | No answer, after about 5 rings |
| 6 | a line that answers with silence | Picked up |
| 7 | a busy line | Busy |
| 8 | a number that was never assigned | Unallocated |
| 9 | a number that plays a phone-company message before any answer | Voice message with text, or Unknown with reason "phone company message, not recorded" (R-22) |
| 10 | the number from row 1 again, at once | the saved result from row 1, with its date; no new call |
| 11 | "123", an empty box, and a +44 number | "123" and the empty box: a refusal message; no call, nothing saved (R-24). The +44 number: International, saved; no call (R-28) |

The result "International" is for a number of another country; such a number is never called.

After the calls:

- If rows 4 and 8 cannot be told apart, restart the app with
  `export CHECKNUMBER_COMBINE_DISCONNECTED=true`; both then read
  "Disconnected / Unallocated" (R-23).
- Any call that reads Unknown shows its reason; it is dialled again on the next
  request (R-21).
- Each result shows the number dialled and the date and time of the call in
  US Eastern (R-10).
