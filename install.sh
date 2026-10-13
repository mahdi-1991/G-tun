#!/usr/bin/env bash
# G-Tun installer and updater.
#
#   install.sh            interactive installation of a server or a client
#   install.sh --update   rebuild and restart the installed role, keeping its configuration
set -euo pipefail

readonly REPO_URL="https://github.com/mahdi-1991/G-tun.git"
readonly WORK_DIR="/root/G-tun-Project"
readonly CONF_DIR="/etc/g-tun"
readonly BIN_DIR="/usr/local/bin"
readonly GO_VERSION="1.23.4"
readonly GO_ROOT="/usr/local/go"
readonly UNIT_DIR="/etc/systemd/system"

export PATH="${GO_ROOT}/bin:${PATH}"

die() {
  echo "Error: $*" >&2
  exit 1
}

info() {
  echo "==> $*"
}

require_root() {
  [[ ${EUID} -eq 0 ]] || die "please run this script as root."
}

# ---------------------------------------------------------------- validation

valid_port() {
  [[ $1 =~ ^[0-9]+$ ]] && (( $1 >= 1 && $1 <= 65535 ))
}

# Accepts a host name, an IPv4 address or a bracketed or bare IPv6 address.
valid_host() {
  local pattern='^[][A-Za-z0-9._:-]+$'
  [[ $1 =~ ${pattern} ]]
}

valid_hostport() {
  local host="${1%:*}" port="${1##*:}"
  [[ -n ${host} && ${1} == *:* ]] && valid_host "${host}" && valid_port "${port}"
}

# prompt VAR "Question" [default] [validator]
prompt() {
  local var=$1 question=$2 default=${3:-} validator=${4:-} answer
  while true; do
    if [[ -n ${default} ]]; then
      read -r -p "${question} (default ${default}): " answer
      answer=${answer:-${default}}
    else
      read -r -p "${question}: " answer
    fi
    if [[ -z ${validator} ]] || ${validator} "${answer}"; then
      printf -v "${var}" '%s' "${answer}"
      return
    fi
    echo "Invalid value, please try again."
  done
}

# json_value FILE KEY prints the string value of KEY in a flat JSON file.
json_value() {
  [[ -f $1 ]] || return 0
  sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" | head -n 1
}

# ---------------------------------------------------------------- system

install_dependencies() {
  info "Installing dependencies"
  apt-get update -y
  apt-get install -y git curl wget tar openssl nano ca-certificates
}

tune_system() {
  info "Tuning the network stack (BBR and larger buffers)"
  modprobe tcp_bbr 2>/dev/null || true
  cat > /etc/sysctl.d/99-gtun.conf <<'SYSCTL'
fs.file-max = 1048576
net.core.rmem_max = 67108864
net.core.wmem_max = 67108864
net.core.rmem_default = 65536
net.core.wmem_default = 65536
net.ipv4.tcp_rmem = 4096 87380 67108864
net.ipv4.tcp_wmem = 4096 65536 67108864
net.ipv4.tcp_mtu_probing = 1
net.ipv4.tcp_fastopen = 3
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
SYSCTL
  sysctl --system > /dev/null 2>&1 || echo "Warning: some sysctl settings could not be applied." >&2
}

install_go() {
  if [[ -x ${GO_ROOT}/bin/go ]] && "${GO_ROOT}/bin/go" version | grep -qE 'go1\.(2[3-9]|[3-9][0-9])'; then
    return
  fi
  local arch
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture: $(uname -m)" ;;
  esac
  info "Installing Go ${GO_VERSION} (${arch})"
  local tarball
  tarball="$(mktemp)"
  wget -q "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -O "${tarball}"
  rm -rf "${GO_ROOT}"
  tar -C /usr/local -xzf "${tarball}"
  rm -f "${tarball}"
}

fetch_source() {
  info "Fetching the source code"
  if [[ -d ${WORK_DIR}/.git ]]; then
    git -C "${WORK_DIR}" fetch --quiet origin main
    git -C "${WORK_DIR}" reset --quiet --hard origin/main
  else
    rm -rf "${WORK_DIR}"
    git clone --quiet "${REPO_URL}" "${WORK_DIR}"
  fi
}

# build_binary NAME builds ./cmd/NAME and installs it atomically.
build_binary() {
  local name=$1 version tmp
  info "Building ${name}"
  version="$(git -C "${WORK_DIR}" describe --always --dirty 2>/dev/null || echo dev)"
  tmp="$(mktemp)"
  (
    cd "${WORK_DIR}"
    # -mod=mod lets Go record missing go.sum entries, verified against the checksum database.
    GOFLAGS=-mod=mod go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "${tmp}" "./cmd/${name}"
  )
  install -m 0755 "${tmp}" "${BIN_DIR}/${name}"
  rm -f "${tmp}"
}

# write_service ROLE writes and reloads the systemd unit for server or client.
write_service() {
  local role=$1
  cat > "${UNIT_DIR}/g-tun-${role}.service" <<UNIT
[Unit]
Description=G-Tun ${role^} Service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=${CONF_DIR}
ExecStart=${BIN_DIR}/g-tun-${role} -config ${CONF_DIR}/${role}_config.json
Restart=always
RestartSec=3
LimitNOFILE=1048576
Environment=GOGC=400
Environment=GOMEMLIMIT=768MiB
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
}

install_panel() {
  install -m 0755 "${WORK_DIR}/g-tun.sh" /usr/bin/g-tun
}

# ---------------------------------------------------------------- roles

# check_tls13 HOST:PORT verifies that a web site completes a TLS 1.3 handshake,
# which the stealth protocol requires of its handshake site.
check_tls13() {
  local host="${1%:*}"
  timeout 15 openssl s_client -tls1_3 -connect "$1" -servername "${host}" < /dev/null 2> /dev/null \
    | grep -q "TLSv1.3"
}

setup_server() {
  local control_port="" data_port protocol target token choice handshake_site="" default_port=8081

  echo "------------------------------------------"
  echo "Select Protocol:"
  echo "1) tcp    2) udp      3) ws       4) tcpmux"
  echo "5) wsmux  6) wss      7) wssmux   8) utcpmux (KCP)"
  echo "9) quic"
  echo "10) stealth  - ShadowTLS v3 + multiplexing. Real TLS handshake with a real site;"
  echo "               the best choice under heavy filtering and active probing."
  read -r -p "Enter choice [1-10] (default 10): " choice
  case ${choice:-10} in
    1) protocol=tcp ;;
    2) protocol=udp ;;
    3) protocol=ws ;;
    4) protocol=tcpmux ;;
    5) protocol=wsmux ;;
    6) protocol=wss ;;
    7) protocol=wssmux ;;
    8) protocol=utcpmux ;;
    9) protocol=quic ;;
    10) protocol=stealth ;;
    *) die "invalid choice." ;;
  esac

  if [[ ${protocol} == stealth ]]; then
    default_port=443
    echo "------------------------------------------"
    echo "Stealth forwards unauthenticated visitors to a real web site, so the site must"
    echo "support TLS 1.3, be reachable from THIS server, and be reachable (not filtered)"
    echo "from where your clients are. Popular, large sites work best."
    while true; do
      prompt handshake_site "Handshake site host:port" www.microsoft.com:443 valid_hostport
      if check_tls13 "${handshake_site}"; then
        break
      fi
      echo "${handshake_site} did not complete a TLS 1.3 handshake from this server; pick another."
    done
  else
    prompt control_port "Enter Control Port" 8080 valid_port
  fi

  prompt target "Enter Xray/Destination Address" 127.0.0.1:10085 valid_hostport
  prompt data_port "Enter Data Port for Tunnel" "${default_port}" valid_port

  token="$(json_value "${CONF_DIR}/server_config.json" token)"
  if [[ -z ${token} ]]; then
    info "Generating a 64-character token"
    token="$(openssl rand -hex 32)"
  fi

  mkdir -p "${CONF_DIR}"
  chmod 700 "${CONF_DIR}"

  if [[ ${protocol} != stealth ]]; then
    if [[ ! -s ${CONF_DIR}/cert.pem || ! -s ${CONF_DIR}/key.pem ]]; then
      info "Generating a TLS certificate"
      (cd "${WORK_DIR}" && GOFLAGS=-mod=mod go run ./cmd/g-tun-gencert -cert "${CONF_DIR}/cert.pem" -key "${CONF_DIR}/key.pem")
    else
      info "Keeping the existing certificate so clients stay connected"
    fi
    chmod 600 "${CONF_DIR}/key.pem"
  fi

  (
    umask 077
    {
      echo "{"
      [[ -n ${control_port} ]] && echo "    \"control_port\": \"${control_port}\","
      echo "    \"data_port\": \"${data_port}\","
      echo "    \"protocol\": \"${protocol}\","
      echo "    \"xray_inbound_address\": \"${target}\","
      echo "    \"token\": \"${token}\","
      [[ -n ${handshake_site} ]] && echo "    \"stealth_handshake_server\": \"${handshake_site}\","
      echo "    \"tls_cert_path\": \"${CONF_DIR}/cert.pem\","
      echo "    \"tls_key_path\": \"${CONF_DIR}/key.pem\","
      echo "    \"kcp_config\": {"
      echo "        \"NoDelay\": 1, \"Interval\": 20, \"Resend\": 2, \"NoCongestion\": 1,"
      echo "        \"SndWnd\": 4096, \"RcvWnd\": 4096, \"DataShards\": 10, \"ParityShards\": 3"
      echo "    }"
      echo "}"
    } > "${CONF_DIR}/server_config.json"
  )

  build_binary g-tun-server
  "${BIN_DIR}/g-tun-server" -config "${CONF_DIR}/server_config.json" -check
  write_service server
  systemctl enable g-tun-server
  systemctl restart g-tun-server
  install_panel

  if [[ ${protocol} == stealth ]]; then
    cat <<DONE

==========================================================================
                     SERVER INSTALLATION SUCCESSFUL
==========================================================================
 Protocol: stealth (ShadowTLS v3)
 Open this port in your firewall: ${data_port}/tcp   (there is no control port)

 Enter these values during client installation:

 TOKEN:          ${token}
 DATA PORT:      ${data_port}
 HANDSHAKE SNI:  ${handshake_site%:*}

 No certificate is needed. Manage the tunnel at any time by typing: g-tun
==========================================================================
DONE
    return
  fi

  cat <<DONE

==========================================================================
                     SERVER INSTALLATION SUCCESSFUL
==========================================================================
 Protocol: ${protocol}
 Open these ports in your firewall: control ${control_port}/tcp, data ${data_port}

 Use this token during client installation:

 TOKEN: ${token}

 --------------------------------------------------------------------------
 Paste this certificate (including the BEGIN and END lines) into the client
 installer. The client trusts only this exact certificate.

$(cat "${CONF_DIR}/cert.pem")

 Manage the tunnel at any time by typing: g-tun
==========================================================================
DONE
}

setup_client() {
  local server_ip control_port local_port token existing stealth="" data_port="" sni=""

  prompt server_ip "Enter Server IP Address" "" valid_host
  read -r -p "Does the server use the stealth protocol (option 10)? [Y/n]: " stealth
  if [[ ${stealth:-y} == [yY]* ]]; then
    stealth=yes
    prompt data_port "Enter the server's Data Port" 443 valid_port
    prompt sni "Enter the HANDSHAKE SNI printed by the server" "" valid_host
  else
    stealth=""
    prompt control_port "Enter the server's Control Port" 8080 valid_port
  fi
  prompt local_port "Enter Local Port to Listen on" 1080 valid_port

  existing="$(json_value "${CONF_DIR}/client_config.json" token)"
  echo "------------------------------------------"
  if [[ -n ${existing} ]]; then
    read -r -p "Paste the Token from the server (Enter keeps the existing one): " token
    token=${token:-${existing}}
  else
    read -r -p "Paste the Token from the server: " token
  fi
  [[ ${#token} -ge 16 ]] || die "the token must be at least 16 characters."

  mkdir -p "${CONF_DIR}"
  chmod 700 "${CONF_DIR}"

  if [[ -n ${stealth} ]]; then
    : # Stealth needs no certificate: it verifies the handshake site's real one.
  elif [[ ! -s ${CONF_DIR}/cert.pem ]]; then
    echo "------------------------------------------"
    echo "The server's certificate is needed to protect against man-in-the-middle attacks."
    echo "The editor opens next: paste the certificate printed by the server installer,"
    echo "then press Ctrl+O, Enter, Ctrl+X to save and exit."
    read -r -p "Press ENTER to continue..." _
    nano "${CONF_DIR}/cert.pem"
  fi
  if [[ -z ${stealth} ]]; then
    openssl x509 -in "${CONF_DIR}/cert.pem" -noout 2> /dev/null \
      || die "${CONF_DIR}/cert.pem is not a valid certificate."
  fi

  (
    umask 077
    if [[ -n ${stealth} ]]; then
      cat > "${CONF_DIR}/client_config.json" <<JSON
{
    "protocol": "stealth",
    "data_port": "${data_port}",
    "stealth_sni": "${sni}",
    "remote_server_ip": "${server_ip}",
    "local_listen_port": "0.0.0.0:${local_port}",
    "token": "${token}"
}
JSON
    else
      cat > "${CONF_DIR}/client_config.json" <<JSON
{
    "control_server_address": "${server_ip}:${control_port}",
    "remote_server_ip": "${server_ip}",
    "local_listen_port": "0.0.0.0:${local_port}",
    "token": "${token}"
}
JSON
    fi
  )

  build_binary g-tun-client
  "${BIN_DIR}/g-tun-client" -config "${CONF_DIR}/client_config.json" -check
  write_service client
  systemctl enable g-tun-client
  systemctl restart g-tun-client
  install_panel

  cat <<DONE
==========================================================================
 Client setup complete and running in the background.
 Applications can now connect to port ${local_port} on this machine.
 Manage the tunnel at any time by typing: g-tun
==========================================================================
DONE
}

update_installed() {
  local role
  if [[ -f ${UNIT_DIR}/g-tun-server.service ]]; then
    role=server
  elif [[ -f ${UNIT_DIR}/g-tun-client.service ]]; then
    role=client
  else
    die "G-Tun is not installed; run the installer without --update."
  fi
  install_go
  fetch_source
  build_binary "g-tun-${role}"
  "${BIN_DIR}/g-tun-${role}" -config "${CONF_DIR}/${role}_config.json" -check
  write_service "${role}"
  systemctl restart "g-tun-${role}"
  install_panel
  info "Updated and restarted g-tun-${role}. Remember to update the other side too."
}

main() {
  require_root
  echo "=========================================="
  echo "          G-Tun Setup Wizard              "
  echo "=========================================="

  if [[ ${1:-} == --update ]]; then
    update_installed
    return
  fi

  install_dependencies
  tune_system
  install_go
  fetch_source

  echo "------------------------------------------"
  echo "Select setup type:"
  echo "1) Server"
  echo "2) Client"
  local setup_type
  read -r -p "Enter choice [1 or 2]: " setup_type
  case ${setup_type} in
    1) setup_server ;;
    2) setup_client ;;
    *) die "invalid choice." ;;
  esac
}

main "$@"
