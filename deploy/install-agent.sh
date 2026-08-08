#!/bin/sh
set -eu

SERVER=""
CODE=""
NAME="$(hostname)"
PROTOCOLS="tcp,http"
DOWNLOAD_URL=""
EXPECTED_SHA256=""
ARCHITECTURE=""
INSECURE="false"

usage() {
    echo "Usage: install-agent.sh --server HOST:PORT --code CODE --download-url URL [--architecture linux-amd64|linux-arm64] [--name NAME] [--protocols LIST] [--sha256 HASH] [--insecure]"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --server) SERVER="${2:-}"; shift 2 ;;
        --code) CODE="${2:-}"; shift 2 ;;
        --name) NAME="${2:-}"; shift 2 ;;
        --protocols) PROTOCOLS="${2:-}"; shift 2 ;;
        --architecture) ARCHITECTURE="${2:-}"; shift 2 ;;
        --download-url) DOWNLOAD_URL="${2:-}"; shift 2 ;;
        --sha256) EXPECTED_SHA256="${2:-}"; shift 2 ;;
        --insecure) INSECURE="true"; shift ;;
        --help|-h) usage; exit 0 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this installer through sudo." >&2
    exit 1
fi
if [ -z "$SERVER" ] || [ -z "$CODE" ] || [ -z "$DOWNLOAD_URL" ]; then
    usage >&2
    exit 2
fi
case "$(uname -m)" in
    x86_64|amd64) DETECTED_ARCHITECTURE="amd64" ;;
    aarch64|arm64) DETECTED_ARCHITECTURE="arm64" ;;
    *) echo "This installer only supports Linux amd64 and arm64." >&2; exit 1 ;;
esac
case "$ARCHITECTURE" in
    "") EXPECTED_ARCHITECTURE="$DETECTED_ARCHITECTURE" ;;
    amd64|x86_64|linux-amd64) EXPECTED_ARCHITECTURE="amd64" ;;
    arm64|aarch64|linux-arm64) EXPECTED_ARCHITECTURE="arm64" ;;
    *) echo "Unsupported Agent architecture: $ARCHITECTURE" >&2; exit 2 ;;
esac
if [ "$DETECTED_ARCHITECTURE" != "$EXPECTED_ARCHITECTURE" ]; then
    echo "Agent package architecture $EXPECTED_ARCHITECTURE does not match this host ($DETECTED_ARCHITECTURE)." >&2
    exit 1
fi
for value in "$SERVER" "$CODE" "$NAME" "$PROTOCOLS" "$DOWNLOAD_URL"; do
    case "$value" in
        *'"'*|*'\'*|*'
'*) echo "Installer argument contains unsupported characters." >&2; exit 2 ;;
    esac
done
command -v curl >/dev/null 2>&1 || { echo "curl is required." >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemd is required." >&2; exit 1; }

TEMP_BINARY="$(mktemp /tmp/newnexus-agent.XXXXXX)"
trap 'rm -f "$TEMP_BINARY"' EXIT INT TERM
curl -fL --proto '=https,http' --tlsv1.2 "$DOWNLOAD_URL" -o "$TEMP_BINARY"
if [ -n "$EXPECTED_SHA256" ]; then
    command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required." >&2; exit 1; }
    ACTUAL_SHA256="$(sha256sum "$TEMP_BINARY" | awk '{print $1}')"
    if [ "$ACTUAL_SHA256" != "$EXPECTED_SHA256" ]; then
        echo "Agent checksum verification failed." >&2
        exit 1
    fi
fi

if ! id newnexus-agent >/dev/null 2>&1; then
    useradd --system --home-dir /var/lib/newnexus-agent --shell /usr/sbin/nologin newnexus-agent
fi
install -d -o newnexus-agent -g newnexus-agent -m 0750 /var/lib/newnexus-agent
install -d -o root -g root -m 0755 /etc/newnexus
install -o root -g root -m 0755 "$TEMP_BINARY" /usr/local/bin/newnexus-agent
if [ -f /etc/newnexus/agent.env ]; then
    cp /etc/newnexus/agent.env "/etc/newnexus/agent.env.bak.$(date +%s)"
fi
cat > /etc/newnexus/agent.env <<EOF
NEWNEXUS_AGENT_SERVER="$SERVER"
NEWNEXUS_AGENT_ENROLL_CODE="$CODE"
NEWNEXUS_AGENT_NAME="$NAME"
NEWNEXUS_AGENT_PROTOCOLS="$PROTOCOLS"
NEWNEXUS_AGENT_STATE="/var/lib/newnexus-agent/state.json"
NEWNEXUS_AGENT_INSECURE="$INSECURE"
EOF
chmod 0600 /etc/newnexus/agent.env

cat > /etc/systemd/system/newnexus-agent.service <<'EOF'
[Unit]
Description=NewNexus remote probe agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=newnexus-agent
Group=newnexus-agent
EnvironmentFile=/etc/newnexus/agent.env
ExecStart=/usr/local/bin/newnexus-agent
Restart=always
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/var/lib/newnexus-agent
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now newnexus-agent.service
sleep 1
if systemctl is-active --quiet newnexus-agent.service; then
    echo "NewNexus Agent installed and started successfully."
else
    systemctl status newnexus-agent.service --no-pager >&2 || true
    exit 1
fi
