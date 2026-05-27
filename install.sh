#!/bin/bash
# G-Tun Setup and Installation Script
set -e

echo "=========================================="
echo "          G-Tun Setup Wizard              "
echo "=========================================="

if [ "$EUID" -ne 0 ]; then
  echo "Error: Please run this script as root."
  exit 1
fi

echo "Checking dependencies..."
apt-get update -y && apt-get install -y git curl wget tar systemd openssl

# 1. Install Go 1.23.0 safely
if [ ! -f "/usr/local/go/bin/go" ] || ! /usr/local/go/bin/go version | grep -q "go1.23"; then
    echo "Installing Go 1.23.0..."
    wget -q https://go.dev/dl/go1.23.0.linux-amd64.tar.gz -O /tmp/go1.23.0.tar.gz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/go1.23.0.tar.gz
    rm /tmp/go1.23.0.tar.gz
fi
export PATH=/usr/local/go/bin:$PATH

WORK_DIR="/root/G-tun-Project"
if [ ! -d "$WORK_DIR" ]; then
    git clone https://github.com/mahdi-1991/G-tun.git "$WORK_DIR"
fi
cd "$WORK_DIR"
git pull origin main || true

echo "------------------------------------------"
echo "Select setup type:"
echo "1) Server"
echo "2) Client"
read -p "Enter choice [1 or 2]: " SETUP_TYPE

read -p "Enter Control Port (default 8080): " CONTROL_PORT
CONTROL_PORT=${CONTROL_PORT:-8080}

if [ "$SETUP_TYPE" == "1" ]; then
    # ================== SERVER SETUP ==================
    echo "------------------------------------------"
    echo "Select Protocol:"
    echo "1) tcp    2) udp      3) ws       4) tcpmux"
    echo "5) wsmux  6) wss      7) wssmux   8) utcpmux (KCP)"
    read -p "Enter choice [1-8] (default 1): " PROTO_CHOICE
    
    case $PROTO_CHOICE in
        2) PROTOCOL="udp" ;;
        3) PROTOCOL="ws" ;;
        4) PROTOCOL="tcpmux" ;;
        5) PROTOCOL="wsmux" ;;
        6) PROTOCOL="wss" ;;
        7) PROTOCOL="wssmux" ;;
        8) PROTOCOL="utcpmux" ;;
        *) PROTOCOL="tcp" ;;
    esac

    read -p "Enter Xray/Destination Address (default 127.0.0.1:10085): " XRAY_ADDR
    XRAY_ADDR=${XRAY_ADDR:-127.0.0.1:10085}

    read -p "Enter Data Port for Tunnel (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}

    echo "Generating secure 64-character token..."
    if command -v openssl >/dev/null 2>&1; then
        SECRET_TOKEN=$(openssl rand -hex 32)
    else
        SECRET_TOKEN=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 64)
    fi

    mkdir -p /etc/g-tun
    cat <<EOF > /etc/g-tun/server_config.json
{
    "control_port": "$CONTROL_PORT",
    "data_port": "$DATA_PORT",
    "protocol": "$PROTOCOL",
    "xray_inbound_address": "$XRAY_ADDR",
    "token": "$SECRET_TOKEN",
    "tls_cert_path": "server.crt",
    "tls_key_path": "server.key",
    "kcp_config": {
        "NoDelay": 1, "Interval": 10, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 1024, "RcvWnd": 1024, "DataShards": 10, "ParityShards": 3
    }
}
EOF

    echo "Building Server Binary..."
    cd server
    
    /usr/local/go/bin/go mod tidy
    /usr/local/go/bin/go build -o g-tun-server server.go
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

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-server
    systemctl restart g-tun-server
    
    echo " "
    echo "=========================================================================="
    echo "                     SERVER INSTALLATION SUCCESSFUL                       "
    echo "=========================================================================="
    echo " Protocol Selected: $PROTOCOL "
    echo " Copy the 64-character token below and use it during client installation: "
    echo " "
    echo " TOKEN: $SECRET_TOKEN"
    echo " "
    echo "=========================================================================="
    echo " "

elif [ "$SETUP_TYPE" == "2" ]; then
    # ================== CLIENT SETUP ==================
    read -p "Enter Server IP Address: " SERVER_IP
    if [ -z "$SERVER_IP" ]; then
        echo "Error: Server IP cannot be empty."
        exit 1
    fi
    
    read -p "Enter Server Data Port (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}
    
    read -p "Enter Local Port to Listen on (default 1080): " LOCAL_PORT
    LOCAL_PORT=${LOCAL_PORT:-1080}

    echo "------------------------------------------"
    read -p "Paste the 64-character Token from Server: " SECRET_TOKEN
    if [ -z "$SECRET_TOKEN" ] || [ ${#SECRET_TOKEN} -lt 32 ]; then
        echo "Error: Invalid token. Token must be provided and securely long."
        exit 1
    fi

    mkdir -p /etc/g-tun
    cat <<EOF > /etc/g-tun/client_config.json
{
    "control_server_address": "$SERVER_IP:$CONTROL_PORT",
    "remote_server_ip": "$SERVER_IP",
    "data_port": "$DATA_PORT",
    "local_listen_port": "0.0.0.0:$LOCAL_PORT",
    "token": "$SECRET_TOKEN",
    "kcp_config": {
        "NoDelay": 1, "Interval": 10, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 1024, "RcvWnd": 1024, "DataShards": 10, "ParityShards": 3
    }
}
EOF

    echo "Building Client Binary..."
    cd client
    
    /usr/local/go/bin/go mod tidy
    /usr/local/go/bin/go build -o g-tun-client client.go
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

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-client
    systemctl restart g-tun-client
    echo " "
    echo "=========================================================================="
    echo " Client setup complete and running in background!"
    echo "=========================================================================="
fi