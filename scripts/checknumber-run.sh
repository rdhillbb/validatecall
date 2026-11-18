#!/bin/zsh
# Start script of checknumber (R-26, TC-33). The LaunchAgent runs it through
# `zsh -ic`, so ~/.zshrc has already exported the database connection (C-3)
# and the PATH on which the app finds the tunnel program. This file holds no
# port, no key and no password (C-2, C-4): the ports come from the registry
# and the settings file is only named here, never read.
set -eu

REPO="${0:A:h:h}"
PROGRAM="$REPO/bin/checknumber"
# get_port of ~/.zshrc is a shell function and does not reach a script, so the
# registry program behind it is called directly (PORTS.md).
REGISTRY="${PORT_REGISTRY:-$HOME/bin/port_registry.py}"
PAGE_PORT_NAME="checknumber"
EVENTS_PORT_NAME="checknumber-events"
DATABASE_NAME="checknumber"
ENV_FILE_NAME="env.apikeys"
RECORDINGS_FOLDER="checknumber-recordings"
# The app's own log (TC-44); the app creates the folder and sets the file
# aside itself (R-30).
LOG_DIR="$HOME/Library/Logs/checknumber"
LOG_FILE="$LOG_DIR/checknumber.log"
# TC-32: with this value the app starts the tunnel itself, so no public
# address has to be known before the start.
TUNNEL_MODE="cloudflared"

fail() {
  print -u2 -r -- "checknumber-run: $1"
  exit 1
}

[[ -x "$PROGRAM" ]] || fail "the program $PROGRAM is missing; scripts/checknumber-autostart.sh install builds it"
[[ -x "$REGISTRY" ]] || fail "the port registry $REGISTRY is missing or cannot be run"

# The ports are always the registry's, also when the variables are already set
# (C-2).
CHECKNUMBER_PORT="$("$REGISTRY" get "$PAGE_PORT_NAME")" || fail "the port registry $REGISTRY gave no port for $PAGE_PORT_NAME"
CHECKNUMBER_EVENTS_PORT="$("$REGISTRY" get "$EVENTS_PORT_NAME")" || fail "the port registry $REGISTRY gave no port for $EVENTS_PORT_NAME"
export CHECKNUMBER_PORT CHECKNUMBER_EVENTS_PORT

# A value that is already set is kept, so ~/.zshrc can override any of the
# five, including an empty value (an empty tunnel mode disables the tunnel,
# an empty log file sends the app's lines to the start log).
export PGDATABASE="${PGDATABASE-$DATABASE_NAME}"
export CHECKNUMBER_ENV_FILE="${CHECKNUMBER_ENV_FILE-$REPO/$ENV_FILE_NAME}"
export CHECKNUMBER_TUNNEL="${CHECKNUMBER_TUNNEL-$TUNNEL_MODE}"
export CHECKNUMBER_LOG_FILE="${CHECKNUMBER_LOG_FILE-$LOG_FILE}"
# The app refuses a recordings folder that does not exist; only the default
# one is created here.
if [[ ! -v CHECKNUMBER_RECORDINGS_DIR ]]; then
  export CHECKNUMBER_RECORDINGS_DIR="$HOME/$RECORDINGS_FOLDER"
  mkdir -p "$CHECKNUMBER_RECORDINGS_DIR" || fail "cannot create the recordings folder $CHECKNUMBER_RECORDINGS_DIR"
fi

# The four Telnyx settings come from the settings file only (operator: "use
# env.apikeys"). ~/.zshrc may export TELNYX_ variables of another project, and
# a TELNYX_ variable wins over the file, so every one of them is removed here.
# unset -m fails when no name matches, which set -e would make fatal.
unset -m 'TELNYX_*' || true

exec "$PROGRAM"
