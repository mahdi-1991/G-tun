#!/bin/bash
set -e

echo "=========================================="
echo "          G-Tun Setup Wizard              "
echo "=========================================="

if [ "$EUID" -ne 0 ]; then
  echo "Error: Please run this script as root."
  exit 1
fi

# 1. Install dependencies and Clone Repo
if ! command -v git &> /dev/null || ! command -v go &> /dev/null; then
    echo "Installing Git and Golang..."
    apt-get update -y && apt-get install -y git golang
fi

WORK_DIR="/root/G-tun-Project"
if [ ! -d "$WORK_DIR" ]; then
    echo "Cloning repository..."
    git clone https://github.com/mahdi-1991/G-tun.git "$WORK_DIR"
fi
cd "$WORK_DIR"
git pull origin main || true

# 2. Setup Configuration
echo "Select setup type:"
echo "1) Server"
echo "2) Client"
read -p "Enter choice [1 or 2]: " SETUP_TYPE

read -p "Enter Control Port (default 8080): " CONTROL_PORT
CONTROL_PORT=${CONTROL_PORT:-8080}

read -p "Enter Secret Token for Authentication (default: mahdi): " SECRET_TOKEN
SECRET_TOKEN=${SECRET_TOKEN:-mahdi}

echo "Select Protocol:"
echo "1. tcp    2. udp      3. ws       4. tcpmux"
echo "5. wsmux  6. wss      7. wssmux   8. utcpmux (KCP)"
read -p "Enter Protocol Name (default tcp): " PROTOCOL
PROTOCOL=${PROTOCOL:-tcp}

mkdir -p /etc/g-tun

if [ "$SETUP_TYPE" == "1" ]; then
    read -p "Enter Xray/Destination Address (default 127.0.0.1:10085): " XRAY_ADDR
    XRAY_ADDR=${XRAY_ADDR:-127.0.0.1:10085}

    read -p "Enter Data Port for Tunnel (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}

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

    echo "Building Server..."
    cd server && go mod tidy && go build -o g-tun-server server.go
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

    systemctl daemon-reload && systemctl enable g-tun-server && systemctl restart g-tun-server
    echo "Server setup complete!"

elif [ "$SETUP_TYPE" == "2" ]; then
    read -p "Enter Server IP Address: " SERVER_IP
    read -p "Enter Server Data Port (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}
    read -p "Enter Local Port to Listen on (default 1080): " LOCAL_PORT
    LOCAL_PORT=${LOCAL_PORT:-1080}

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

    echo "Building Client..."
    cd client && go mod tidy && go build -o g-tun-client client.go
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

    systemctl daemon-reload && systemctl enable g-tun-client && systemctl restart g-tun-client
    echo "Client setup complete!"
fi