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
TLS_SHA256=""
RECOVERY_CODE=""
MODE="install"
PURGE="false"

usage() {
    echo "Usage: install-agent.sh --server HOST:PORT --code CODE --download-url URL [--architecture linux-amd64|linux-arm64] [--name NAME] [--protocols LIST] [--sha256 HASH] [--tls-sha256 HASH] [--recovery-code CODE] [--insecure]"
    echo "       install-agent.sh --upgrade --download-url URL [--sha256 HASH]"
    echo "       install-agent.sh --uninstall [--purge]"
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
        --tls-sha256) TLS_SHA256="${2:-}"; shift 2 ;;
        --recovery-code) RECOVERY_CODE="${2:-}"; shift 2 ;;
        --insecure) INSECURE="true"; shift ;;
        --upgrade) MODE="upgrade"; shift ;;
        --uninstall) MODE="uninstall"; shift ;;
        --purge) PURGE="true"; shift ;;
        --help|-h) usage; exit 0 ;;
        *) echo "Unknown option: $1" >&2; usage >&2; exit 2 ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this installer through sudo." >&2
    exit 1
fi
if [ "$MODE" = "uninstall" ]; then
    systemctl disable --now newnexus-agent.service >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/newnexus-agent.service /usr/local/bin/newnexus-agent /etc/newnexus/agent.env
    systemctl daemon-reload
    if [ "$PURGE" = "true" ]; then
        rm -rf /var/lib/newnexus-agent
        userdel newnexus-agent >/dev/null 2>&1 || true
        echo "NewNexus Agent uninstalled and state removed."
    else
        echo "NewNexus Agent uninstalled; /var/lib/newnexus-agent was preserved."
    fi
    exit 0
fi
if [ "$MODE" = "upgrade" ] && [ -f /etc/newnexus/agent.env ]; then
    # shellcheck disable=SC1091
    . /etc/newnexus/agent.env
    SERVER="${SERVER:-${NEWNEXUS_AGENT_SERVER:-}}"
    NAME="${NAME:-${NEWNEXUS_AGENT_NAME:-$(hostname)}}"
    PROTOCOLS="${PROTOCOLS:-${NEWNEXUS_AGENT_PROTOCOLS:-tcp,http}}"
    INSECURE="${NEWNEXUS_AGENT_INSECURE:-$INSECURE}"
    TLS_SHA256="${TLS_SHA256:-${NEWNEXUS_AGENT_TLS_SHA256:-}}"
    RECOVERY_CODE="${RECOVERY_CODE:-${NEWNEXUS_AGENT_RECOVERY_CODE:-}}"
fi
if [ -z "$DOWNLOAD_URL" ] || { [ "$MODE" = "install" ] && { [ -z "$SERVER" ] || [ -z "$CODE" ]; }; }; then
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
for value in "$SERVER" "$CODE" "$NAME" "$PROTOCOLS" "$DOWNLOAD_URL" "$TLS_SHA256" "$RECOVERY_CODE"; do
    case "$value" in
        *'"'*|*'\'*|*'
'*) echo "Installer argument contains unsupported characters." >&2; exit 2 ;;
    esac
done
command -v curl >/dev/null 2>&1 || { echo "curl is required." >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "systemd is required." >&2; exit 1; }

if [ -n "$TLS_SHA256" ] && [ "$INSECURE" != "true" ]; then
    command -v openssl >/dev/null 2>&1 || { echo "openssl is required for TLS fingerprint verification." >&2; exit 1; }
    TLS_HOST="${SERVER%:*}"
    ACTUAL_TLS_SHA256="$(printf '' | openssl s_client -connect "$SERVER" -servername "$TLS_HOST" 2>/dev/null | openssl x509 -noout -fingerprint -sha256 | sed 's/^.*=//;s/://g' | tr 'A-F' 'a-f')"
    NORMALIZED_TLS_SHA256="$(printf '%s' "$TLS_SHA256" | sed 's/://g' | tr 'A-F' 'a-f')"
    if [ -z "$ACTUAL_TLS_SHA256" ] || [ "$ACTUAL_TLS_SHA256" != "$NORMALIZED_TLS_SHA256" ]; then
        echo "Control-plane TLS certificate fingerprint verification failed." >&2
        exit 1
    fi
fi

TEMP_BINARY="$(mktemp /tmp/newnexus-agent.XXXXXX)"
trap 'rm -f "$TEMP_BINARY"' EXIT INT TERM
if [ "$INSECURE" = "true" ]; then
    curl -fL --retry 3 --retry-all-errors --proto '=https,http' --tlsv1.2 "$DOWNLOAD_URL" -o "$TEMP_BINARY"
else
    curl -fL --retry 3 --retry-all-errors --proto '=https' --tlsv1.2 "$DOWNLOAD_URL" -o "$TEMP_BINARY"
fi
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
if [ "$MODE" = "upgrade" ] && [ -f /etc/newnexus/agent.env ]; then
    systemctl restart newnexus-agent.service
else
cat > /etc/newnexus/agent.env <<EOF
NEWNEXUS_AGENT_SERVER="$SERVER"
NEWNEXUS_AGENT_ENROLL_CODE="$CODE"
NEWNEXUS_AGENT_NAME="$NAME"
NEWNEXUS_AGENT_PROTOCOLS="$PROTOCOLS"
NEWNEXUS_AGENT_STATE="/var/lib/newnexus-agent/state.json"
NEWNEXUS_AGENT_INSECURE="$INSECURE"
NEWNEXUS_AGENT_TLS_SHA256="$TLS_SHA256"
NEWNEXUS_AGENT_RECOVERY_CODE="$RECOVERY_CODE"
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

fi

systemctl daemon-reload
systemctl enable --now newnexus-agent.service
ATTEMPT=0
while [ "$ATTEMPT" -lt 10 ]; do
    if systemctl is-active --quiet newnexus-agent.service; then
        echo "NewNexus Agent installed and started successfully."
        exit 0
    fi
    ATTEMPT=$((ATTEMPT + 1))
    sleep 1
done
if ! systemctl is-active --quiet newnexus-agent.service; then
    systemctl status newnexus-agent.service --no-pager >&2 || true
    exit 1
fi
