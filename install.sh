#!/bin/bash
# G-Tun Setup and Installation Script
# Refactored for Modularity and Safe Execution

set -euo pipefail

readonly WORK_DIR="/root/G-tun-Project"
readonly GO_BIN="/usr/local/go/bin/go"
export PATH="/usr/local/go/bin:${PATH}"

check_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    echo "Error: Please run this script as root." >&2
    exit 1
  fi
}

install_dependencies() {
  echo "Checking and installing dependencies..."
  DEBIAN_FRONTEND=noninteractive apt-get update -y -q
  DEBIAN_FRONTEND=noninteractive apt-get install -y -q git curl wget tar systemd openssl nano
}

tune_system() {
  echo "Tuning system for maximum network performance (BBR & Buffers)..."
  cat <<EOF > /etc/sysctl.d/99-gtun.conf
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
EOF
  sysctl --system > /dev/null 2>&1
  echo "System tuned successfully!"
}

install_go() {
  if [[ ! -x "${GO_BIN}" ]] || ! "${GO_BIN}" version | grep -q "go1.23"; then
    echo "Installing Go 1.23.0..."
    wget -q https://go.dev/dl/go1.23.0.linux-amd64.tar.gz -O /tmp/go1.23.0.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go1.23.0.tar.gz
    rm /tmp/go1.23.0.tar.gz
  fi
}

fetch_source() {
  if [[ ! -d "${WORK_DIR}" ]]; then
    git clone https://github.com/mahdi-1991/G-tun.git "${WORK_DIR}"
  fi
  cd "${WORK_DIR}"
  git pull origin main || true
}

setup_server() {
  local control_port="$1"
  local proto_choice protocol xray_addr data_port secret_token

  echo "------------------------------------------"
  echo "Select Protocol:"
  echo "1) tcp    2) udp      3) ws       4) tcpmux"
  echo "5) wsmux  6) wss      7) wssmux   8) utcpmux (KCP)"
  echo "9) quic (HTTP/3 - Best for Anti-Censorship)"
  read -r -p "Enter choice [1-9] (default 9): " proto_choice
  
  case "${proto_choice}" in
    1) protocol="tcp" ;;
    2) protocol="udp" ;;
    3) protocol="ws" ;;
    4) protocol="tcpmux" ;;
    5) protocol="wsmux" ;;
    6) protocol="wss" ;;
    7) protocol="wssmux" ;;
    8) protocol="utcpmux" ;;
    *) protocol="quic" ;;
  esac

  read -r -p "Enter Xray/Destination Address (default 127.0.0.1:10085): " xray_addr
  xray_addr="${xray_addr:-127.0.0.1:10085}"

  read -r -p "Enter Data Port for Tunnel (default 8081): " data_port
  data_port="${data_port:-8081}"

  secret_token=""
  if [[ -f /etc/g-tun/server_config.json ]]; then
    secret_token=$(grep '"token"' /etc/g-tun/server_config.json | cut -d '"' -f 4 || true)
  fi

  if [[ -z "${secret_token}" ]]; then
    echo "Generating secure 64-character token..."
    if command -v openssl >/dev/null 2>&1; then
      secret_token=$(openssl rand -hex 32)
    else
      secret_token=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 64)
    fi
  fi

  echo "Checking TLS Certificates..."
  if [[ ! -s /etc/g-tun/cert.pem ]] || [[ ! -s /etc/g-tun/key.pem ]]; then
    echo "Generating new ECC Certificates for WSS & QUIC..."
    cd "${WORK_DIR}/server"
    "${GO_BIN}" run generate_cert.go
    cp cert.pem /etc/g-tun/cert.pem
    cp key.pem /etc/g-tun/key.pem
  else
    echo "Found existing certificates in /etc/g-tun/. Keeping them to prevent client disconnections."
  fi

  cat <<EOF > /etc/g-tun/server_config.json
{
    "control_port": "${control_port}",
    "data_port": "${data_port}",
    "protocol": "${protocol}",
    "xray_inbound_address": "${xray_addr}",
    "token": "${secret_token}",
    "tls_cert_path": "/etc/g-tun/cert.pem",
    "tls_key_path": "/etc/g-tun/key.pem",
    "kcp_config": {
        "NoDelay": 1, "Interval": 20, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 4096, "RcvWnd": 4096, "DataShards": 10, "ParityShards": 3
    }
}
EOF

  echo "Building Server Binary..."
  cd "${WORK_DIR}/server"
  "${GO_BIN}" get github.com/quic-go/quic-go@v0.48.2
  "${GO_BIN}" mod tidy
  "${GO_BIN}" build -o g-tun-server server.go
  mv g-tun-server /usr/local/bin/

  cat <<EOF > /etc/systemd/system/g-tun-server.service
[Unit]
Description=G-Tun Server Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/etc/g-tun
ExecStart=/usr/local/bin/g-tun-server
Restart=always
RestartSec=3
LimitNOFILE=1048576
Environment="GOGC=400"
Environment="GOMEMLIMIT=768MiB"

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable --now g-tun-server
  
  cp "${WORK_DIR}/g-tun.sh" /usr/bin/g-tun
  chmod +x /usr/bin/g-tun
  
  echo -e "\n=========================================================================="
  echo "                     SERVER INSTALLATION SUCCESSFUL                       "
  echo "=========================================================================="
  echo " Protocol Selected: ${protocol} "
  echo " Copy the 64-character token below and use it during client installation: "
  echo -e "\n TOKEN: ${secret_token}\n"
  echo " ------------------------------------------------------------------------ "
  echo " IMPORTANT: Copy the Certificate below (including BEGIN and END lines)    "
  echo " You MUST paste this during the Client installation!                      "
  echo -e "\n$(cat /etc/g-tun/cert.pem)\n"
  echo " You can manage the tunnel anytime by typing: g-tun"
  echo "=========================================================================="
}

setup_client() {
  local control_port="$1"
  local server_ip data_port local_port existing_token secret_token

  read -r -p "Enter Server IP Address: " server_ip
  if [[ -z "${server_ip}" ]]; then
    echo "Error: Server IP cannot be empty." >&2
    exit 1
  fi
  
  read -r -p "Enter Server Data Port (default 8081): " data_port
  data_port="${data_port:-8081}"
  
  read -r -p "Enter Local Port to Listen on (default 1080): " local_port
  local_port="${local_port:-1080}"

  existing_token=""
  if [[ -f /etc/g-tun/client_config.json ]]; then
    existing_token=$(grep '"token"' /etc/g-tun/client_config.json | cut -d '"' -f 4 || true)
  fi

  echo "------------------------------------------"
  if [[ -n "${existing_token}" ]]; then
    read -r -p "Paste the 64-character Token from Server (Press Enter to keep existing): " secret_token
    secret_token="${secret_token:-$existing_token}"
  else
    read -r -p "Paste the 64-character Token from Server: " secret_token
  fi

  if [[ ! -s /etc/g-tun/cert.pem ]]; then
    echo "------------------------------------------"
    echo "MITM Protection requires the Server's Certificate (cert.pem)."
    echo "Press ENTER to open the editor. Paste the certificate you copied from the server,"
    echo "then press Ctrl+O, Enter, and Ctrl+X to save and exit."
    read -r -p "Press ENTER to continue..."
    nano /etc/g-tun/cert.pem
  else
    echo "------------------------------------------"
    echo "Found existing cert.pem in /etc/g-tun/. Skipping certificate prompt."
  fi

  cat <<EOF > /etc/g-tun/client_config.json
{
    "control_server_address": "${server_ip}:${control_port}",
    "remote_server_ip": "${server_ip}",
    "data_port": "${data_port}",
    "local_listen_port": "0.0.0.0:${local_port}",
    "token": "${secret_token}",
    "kcp_config": {
        "NoDelay": 1, "Interval": 20, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 4096, "RcvWnd": 4096, "DataShards": 10, "ParityShards": 3
    }
}
EOF

  echo "Building Client Binary..."
  cd "${WORK_DIR}/client"
  "${GO_BIN}" get github.com/quic-go/quic-go@v0.48.2
  "${GO_BIN}" mod tidy
  "${GO_BIN}" build -o g-tun-client client.go
  mv g-tun-client /usr/local/bin/

  cat <<EOF > /etc/systemd/system/g-tun-client.service
[Unit]
Description=G-Tun Client Service
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=/etc/g-tun
ExecStart=/usr/local/bin/g-tun-client
Restart=always
RestartSec=3
LimitNOFILE=1048576
Environment="GOGC=400"
Environment="GOMEMLIMIT=768MiB"

[Install]
WantedBy=multi-user.target
EOF

  systemctl daemon-reload
  systemctl enable --now g-tun-client
  
  cp "${WORK_DIR}/g-tun.sh" /usr/bin/g-tun
  chmod +x /usr/bin/g-tun
  
  echo "=========================================================================="
  echo " Client setup complete and running in background!"
  echo " You can manage the tunnel anytime by typing: g-tun"
  echo "=========================================================================="
}

main() {
  echo "=========================================="
  echo "          G-Tun Setup Wizard              "
  echo "=========================================="

  check_root
  install_dependencies
  tune_system
  install_go
  fetch_source

  echo "------------------------------------------"
  echo "Select setup type:"
  echo "1) Server"
  echo "2) Client"
  
  local setup_type control_port
  read -r -p "Enter choice [1 or 2]: " setup_type

  read -r -p "Enter Control Port (default 8080): " control_port
  control_port="${control_port:-8080}"

  mkdir -p /etc/g-tun

  if [[ "${setup_type}" == "1" ]]; then
    setup_server "${control_port}"
  elif [[ "${setup_type}" == "2" ]]; then
    setup_client "${control_port}"
  else
    echo "Invalid choice. Exiting." >&2
    exit 1
  fi
}

main "$@"