#!/bin/sh
# Installs the portmesh agent as a boot-time service: usage `install.sh <name> <token>`, run as root via sudo.
set -eu

BASE_URL="@BASE_URL@"
AGENT_ADDR="@AGENT_ADDR@"
FINGERPRINT="@FINGERPRINT@"
NAME="${1:?usage: install.sh <name> <token>}"
TOKEN="${2:?usage: install.sh <name> <token>}"
LABEL=io.portmesh.agent

[ "$(id -u)" -eq 0 ] || { echo "run with sudo" >&2; exit 1; }
RUN_USER="${SUDO_USER:-root}"

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

mkdir -p /usr/local/bin /etc/portmesh
curl -fsSL "$BASE_URL/dl/portmesh-$OS-$ARCH" -o /usr/local/bin/portmesh.new
chmod 755 /usr/local/bin/portmesh.new
mv -f /usr/local/bin/portmesh.new /usr/local/bin/portmesh

(umask 077; cat > /etc/portmesh/agent.json <<EOF
{"name": "$NAME", "token": "$TOKEN", "server": "$AGENT_ADDR", "fingerprint": "$FINGERPRINT"}
EOF
)
chown "$RUN_USER" /etc/portmesh/agent.json

case "$OS" in
darwin)
  LOG=/var/log/portmesh-agent.log
  touch "$LOG"; chown "$RUN_USER" "$LOG"
  PLIST=/Library/LaunchDaemons/$LABEL.plist
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array><string>/usr/local/bin/portmesh</string><string>agent</string><string>-config</string><string>/etc/portmesh/agent.json</string></array>
  <key>UserName</key><string>$RUN_USER</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$LOG</string>
  <key>StandardErrorPath</key><string>$LOG</string>
</dict>
</plist>
EOF
  chown root:wheel "$PLIST"; chmod 644 "$PLIST"
  launchctl bootout "system/$LABEL" 2>/dev/null || true
  # bootout returns before the job is gone, so bootstrap can fail briefly
  for _ in 1 2 3 4 5; do
    launchctl bootstrap system "$PLIST" 2>/dev/null && break
    sleep 1
  done
  launchctl print "system/$LABEL" >/dev/null
  echo "installed; logs in $LOG"
  ;;
linux)
  cat > /etc/systemd/system/portmesh-agent.service <<EOF
[Unit]
Description=portmesh agent
Wants=network-online.target
After=network-online.target docker.service

[Service]
User=$RUN_USER
ExecStart=/usr/local/bin/portmesh agent -config /etc/portmesh/agent.json
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable portmesh-agent >/dev/null 2>&1
  systemctl restart portmesh-agent
  echo "installed; logs: journalctl -u portmesh-agent"
  ;;
*)
  echo "unsupported OS $OS" >&2; exit 1 ;;
esac
