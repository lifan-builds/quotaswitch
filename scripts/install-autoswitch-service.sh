#!/usr/bin/env bash
set -euo pipefail

AGY_BIN="${AGY_AUTH_BIN:-$(command -v agy-auth || true)}"
if [[ -z "${AGY_BIN}" ]]; then
  echo "error: agy-auth binary not found on PATH (build and install agy-auth first)" >&2
  exit 1
fi

INTERVAL="${AGY_AUTOSWITCH_INTERVAL:-30s}"
PROVIDER="${AGY_AUTOSWITCH_PROVIDER:-all}"
LABEL="${AGY_AUTOSWITCH_LABEL:-com.agy-auth.autoswitch}"
LOG_DIR="${HOME}/Library/Application Support/agy-auth"
PLIST_PATH="${HOME}/Library/LaunchAgents/${LABEL}.plist"

mkdir -p "${LOG_DIR}" "${HOME}/Library/LaunchAgents"

cat > "${PLIST_PATH}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${AGY_BIN}</string>
    <string>rotate</string>
    <string>--provider=${PROVIDER}</string>
    <string>--watch</string>
    <string>--interval=${INTERVAL}</string>
    <string>--relaunch</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    <key>HOME</key>
    <string>${HOME}</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>${LOG_DIR}/autoswitch.log</string>
  <key>StandardErrorPath</key>
  <string>${LOG_DIR}/autoswitch.err.log</string>
</dict>
</plist>
EOF

launchctl bootout "gui/$(id -u)/${LABEL}" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "${PLIST_PATH}"
launchctl kickstart -k "gui/$(id -u)/${LABEL}"

echo "Installed and started ${LABEL} (${PLIST_PATH})"
echo "Logs: ${LOG_DIR}/autoswitch.log"
