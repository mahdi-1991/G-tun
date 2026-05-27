#!/bin/bash
# G-Tun Setup and Installation Script
# Automatically generates a secure 64-character token on server installation.

set -e

echo "=========================================="
echo "          G-Tun Setup Wizard              "
echo "=========================================="

if [ "$EUID" -ne 0 ]; then
  echo "Error: Please run this script as root."
  exit 1
fi

echo "Select setup type:"
echo "1) Server"
echo "2) Client"
read -p "Enter choice [1 or 2]: " SETUP_TYPE

read -p "Enter Control Port (default 8080): " CONTROL_PORT
CONTROL_PORT=${CONTROL_PORT:-8080}

read -p "Select Protocol (tcp, udp, ws, wss) [default tcp]: " PROTOCOL
PROTOCOL=${PROTOCOL:-tcp}

mkdir -p /etc/g-tun

if [ "$SETUP_TYPE" == "1" ]; then
    # Server Setup
    read -p "Enter Xray/Destination Port (default 10085): " XRAY_PORT
    XRAY_PORT=${XRAY_PORT:-10085}

    read -p "Enter Data Port for Tunnel (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}

    # Generate a cryptographically secure 64-character hex token
    echo "Generating secure 64-character token..."
    if command -v openssl >/dev/null 2>&1; then
        SECRET_TOKEN=$(openssl rand -hex 32)
    else
        SECRET_TOKEN=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 64)
    fi

    cat <<EOF > /etc/g-tun/server_config.json
{
    "control_port": "$CONTROL_PORT",
    "data_port": "$DATA_PORT",
    "xray_port": "$XRAY_PORT",
    "protocol": "$PROTOCOL",
    "token": "$SECRET_TOKEN"
}
EOF

    echo "Building Server Binary..."
    cd server && go build -o g-tun-server server.go
    mv g-tun-server /usr/local/bin/

    cat <<EOF > /etc/systemd/system/g-tun-server.service
[Unit]
Description=G-Tun Server Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/g-tun-server -config /etc/g-tun/server_config.json
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-server
    systemctl restart g-tun-server
    
    # Visual layout to expose the generated token clearly to the admin
    echo " "
    echo "=========================================================================="
    echo "                     SERVER INSTALLATION SUCCESSFUL                       "
    echo "=========================================================================="
    echo " Copy the 64-character token below and use it during client installation: "
    echo " "
    echo " TOKEN: $SECRET_TOKEN"
    echo " "
    echo "=========================================================================="
    echo " "

elif [ "$SETUP_TYPE" == "2" ]; then
    # Client Setup
    read -p "Enter Server IP Address: " SERVER_IP
    if [ -z "$SERVER_IP" ]; then
        echo "Error: Server IP cannot be empty."
        exit 1
    fi
    
    read -p "Enter Server Data Port (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}

    read -p "Enter Local Port to Listen on (default 1080): " LOCAL_PORT
    LOCAL_PORT=${LOCAL_PORT:-1080}

    # Request the generated token from the user
    read -p "Paste the 64-character Token from Server: " SECRET_TOKEN
    if [ -z "$SECRET_TOKEN" ] || [ ${#SECRET_TOKEN} -lt 32 ]; then
        echo "Error: Invalid token. Token must be provided and securely long."
        exit 1
    fi

    cat <<EOF > /etc/g-tun/client_config.json
{
    "server_ip": "$SERVER_IP",
    "control_port": "$CONTROL_PORT",
    "data_port": "$DATA_PORT",
    "local_port": "$LOCAL_PORT",
    "token": "$SECRET_TOKEN"
}
EOF

    echo "Building Client Binary..."
    cd client && go build -o g-tun-client client.go
    mv g-tun-client /usr/local/bin/

    cat <<EOF > /etc/systemd/system/g-tun-client.service
[Unit]
Description=G-Tun Client Service
After=network.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/g-tun-client -config /etc/g-tun/client_config.json
Restart=always
RestartSec=3
LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-client
    systemctl restart g-tun-client
    echo "Client setup complete and running in background!"
fi