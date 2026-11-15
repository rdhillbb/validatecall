#!/bin/zsh
# Prints, installs or removes the LaunchAgent that starts checknumber when the
# operator logs in and starts it again when it stops (R-26, TC-33). The agent
# runs the start script through `zsh -ic`, so ~/.zshrc supplies the database
# settings and the PATH; neither this file nor the plist holds a port, a key
# or a password (C-2, C-3, C-4).
set -eu

SCRIPT_NAME="${0:t}"
REPO="${0:A:h:h}"
RUN_SCRIPT="$REPO/scripts/checknumber-run.sh"
PROGRAM="$REPO/bin/checknumber"
PROGRAM_PACKAGE="./cmd/checknumber"
LABEL="com.checknumber.app"
AGENTS_DIR="${LAUNCH_AGENTS_DIR:-$HOME/Library/LaunchAgents}"
PLIST="$AGENTS_DIR/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/checknumber"
LOG_FILE="$LOG_DIR/checknumber.log"
# launchd's names for this user's login session and for the agent in it.
DOMAIN="gui/$UID"
SERVICE="$DOMAIN/$LABEL"
# CHECKNUMBER_NO_LOAD with this value writes or deletes the plist only;
# launchctl is then not called.
NO_LOAD_ON="1"
# How long an unload may take: the app itself takes up to 10 seconds to end
# its open calls (shutdownGrace in cmd/checknumber/main.go).
UNLOAD_WAIT_SECONDS=30

fail() {
  print -u2 -r -- "$SCRIPT_NAME: $1"
  exit 1
}

# xml_text VALUE prints VALUE as XML text; a folder name may hold & or <.
xml_text() {
  local text="$1"
  text=${text//'&'/'&amp;'}
  text=${text//'<'/'&lt;'}
  text=${text//'>'/'&gt;'}
  print -r -- "$text"
}

print_plist() {
  local start_command log_file
  # (q) quotes the path for the shell that reads the command, so a folder
  # name with a space still gives one word.
  start_command="$(xml_text "exec ${(q)RUN_SCRIPT}")"
  log_file="$(xml_text "$LOG_FILE")"
  cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/zsh</string>
		<string>-ic</string>
		<string>$start_command</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$log_file</string>
	<key>StandardErrorPath</key>
	<string>$log_file</string>
</dict>
</plist>
EOF
}

load_wanted() {
  [[ "${CHECKNUMBER_NO_LOAD:-}" != "$NO_LOAD_ON" ]]
}

agent_loaded() {
  launchctl print "$SERVICE" >/dev/null 2>&1
}

unload_agent() {
  agent_loaded || return 0
  # bootout can return, and can report an error, while the app is still
  # ending its open calls; so launchd is asked for the outcome and the exit
  # status of bootout is not used.
  launchctl bootout "$SERVICE" || true
  local waited=0
  while agent_loaded; do
    (( waited < UNLOAD_WAIT_SECONDS )) || fail "$LABEL is still loaded after $UNLOAD_WAIT_SECONDS seconds; not unloaded"
    sleep 1
    (( waited += 1 ))
  done
}

install_agent() {
  go build -C "$REPO" -o "$PROGRAM" "$PROGRAM_PACKAGE" || fail "could not build $PROGRAM from $PROGRAM_PACKAGE"
  # launchd creates the log file but not its folder.
  mkdir -p "$AGENTS_DIR" "$LOG_DIR" || fail "cannot create $AGENTS_DIR and $LOG_DIR"
  print_plist > "$PLIST" || fail "cannot write $PLIST"
  print -r -- "$SCRIPT_NAME: wrote $PLIST"
  if ! load_wanted; then
    print -r -- "$SCRIPT_NAME: not loaded, CHECKNUMBER_NO_LOAD is $NO_LOAD_ON"
    return 0
  fi
  # A loaded agent keeps its old definition and its running program until it
  # is unloaded.
  unload_agent
  launchctl bootstrap "$DOMAIN" "$PLIST" || fail "launchctl could not load $PLIST"
  print -r -- "$SCRIPT_NAME: loaded $LABEL; the log is $LOG_FILE"
}

remove_agent() {
  if load_wanted; then
    unload_agent
  fi
  rm -f "$PLIST" || fail "cannot delete $PLIST"
  print -r -- "$SCRIPT_NAME: removed $PLIST"
}

case "${1:-}" in
  print) print_plist ;;
  install) install_agent ;;
  remove) remove_agent ;;
  *) fail "usage: $SCRIPT_NAME print|install|remove" ;;
esac
