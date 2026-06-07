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
apt-get update -y && apt-get install -y git curl wget tar systemd openssl nano

# ================== OS TUNING ==================
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
tune_system
# ===============================================

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

mkdir -p /etc/g-tun

if [ "$SETUP_TYPE" == "1" ]; then
    echo "------------------------------------------"
    echo "Select Protocol:"
    echo "1) tcp    2) udp      3) ws       4) tcpmux"
    echo "5) wsmux  6) wss      7) wssmux   8) utcpmux (KCP)"
    echo "9) quic (HTTP/3 - Best for Anti-Censorship)"
    read -p "Enter choice [1-9] (default 9): " PROTO_CHOICE
    
    case $PROTO_CHOICE in
        1) PROTOCOL="tcp" ;;
        2) PROTOCOL="udp" ;;
        3) PROTOCOL="ws" ;;
        4) PROTOCOL="tcpmux" ;;
        5) PROTOCOL="wsmux" ;;
        6) PROTOCOL="wss" ;;
        7) PROTOCOL="wssmux" ;;
        8) PROTOCOL="utcpmux" ;;
        *) PROTOCOL="quic" ;;
    esac

    read -p "Enter Xray/Destination Address (default 127.0.0.1:10085): " XRAY_ADDR
    XRAY_ADDR=${XRAY_ADDR:-127.0.0.1:10085}

    read -p "Enter Data Port for Tunnel (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}

    if [ -f /etc/g-tun/server_config.json ]; then
        SECRET_TOKEN=$(cat /etc/g-tun/server_config.json | grep '"token"' | cut -d '"' -f 4)
    fi

    if [ -z "$SECRET_TOKEN" ]; then
        echo "Generating secure 64-character token..."
        if command -v openssl >/dev/null 2>&1; then
            SECRET_TOKEN=$(openssl rand -hex 32)
        else
            SECRET_TOKEN=$(tr -dc 'A-Za-z0-9' < /dev/urandom | head -c 64)
        fi
    fi

    echo "Checking TLS Certificates..."
    if [ ! -s /etc/g-tun/cert.pem ] || [ ! -s /etc/g-tun/key.pem ]; then
        echo "Generating new ECC Certificates for WSS & QUIC..."
        cd /root/G-tun-Project/server
        /usr/local/go/bin/go run generate_cert.go
        cp cert.pem /etc/g-tun/cert.pem
        cp key.pem /etc/g-tun/key.pem
    else
        echo "Found existing certificates in /etc/g-tun/. Keeping them to prevent client disconnections."
    fi

    cat <<EOF > /etc/g-tun/server_config.json
{
    "control_port": "$CONTROL_PORT",
    "data_port": "$DATA_PORT",
    "protocol": "$PROTOCOL",
    "xray_inbound_address": "$XRAY_ADDR",
    "token": "$SECRET_TOKEN",
    "tls_cert_path": "/etc/g-tun/cert.pem",
    "tls_key_path": "/etc/g-tun/key.pem",
    "kcp_config": {
        "NoDelay": 1, "Interval": 20, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 4096, "RcvWnd": 4096, "DataShards": 10, "ParityShards": 3
    }
}
EOF

    echo "Building Server Binary..."
    cd /root/G-tun-Project/server
    
    /usr/local/go/bin/go get github.com/quic-go/quic-go
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
Environment="GOGC=400"
Environment="GOMEMLIMIT=768MiB"

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-server
    systemctl restart g-tun-server
    
    cp /root/G-tun-Project/g-tun.sh /usr/bin/g-tun
    chmod +x /usr/bin/g-tun
    
    echo " "
    echo "=========================================================================="
    echo "                     SERVER INSTALLATION SUCCESSFUL                       "
    echo "=========================================================================="
    echo " Protocol Selected: $PROTOCOL "
    echo " Copy the 64-character token below and use it during client installation: "
    echo " "
    echo " TOKEN: $SECRET_TOKEN"
    echo " "
    echo " ------------------------------------------------------------------------ "
    echo " IMPORTANT: Copy the Certificate below (including BEGIN and END lines)    "
    echo " You MUST paste this during the Client installation!                      "
    echo " "
    cat /etc/g-tun/cert.pem
    echo " "
    echo " You can manage the tunnel anytime by typing: g-tun"
    echo "=========================================================================="
    echo " "

elif [ "$SETUP_TYPE" == "2" ]; then
    read -p "Enter Server IP Address: " SERVER_IP
    if [ -z "$SERVER_IP" ]; then
        echo "Error: Server IP cannot be empty."
        exit 1
    fi
    
    read -p "Enter Server Data Port (default 8081): " DATA_PORT
    DATA_PORT=${DATA_PORT:-8081}
    
    read -p "Enter Local Port to Listen on (default 1080): " LOCAL_PORT
    LOCAL_PORT=${LOCAL_PORT:-1080}

    if [ -f /etc/g-tun/client_config.json ]; then
        EXISTING_TOKEN=$(cat /etc/g-tun/client_config.json | grep '"token"' | cut -d '"' -f 4)
    fi

    echo "------------------------------------------"
    if [ -n "$EXISTING_TOKEN" ]; then
        read -p "Paste the 64-character Token from Server (Press Enter to keep existing): " SECRET_TOKEN
        SECRET_TOKEN=${SECRET_TOKEN:-$EXISTING_TOKEN}
    else
        read -p "Paste the 64-character Token from Server: " SECRET_TOKEN
    fi

    if [ ! -s /etc/g-tun/cert.pem ]; then
        echo "------------------------------------------"
        echo "MITM Protection requires the Server's Certificate (cert.pem)."
        echo "Press ENTER to open the editor. Paste the certificate you copied from the server,"
        echo "then press Ctrl+O, Enter, and Ctrl+X to save and exit."
        read -p "Press ENTER to continue..."
        nano /etc/g-tun/cert.pem
    else
        echo "------------------------------------------"
        echo "Found existing cert.pem in /etc/g-tun/. Skipping certificate prompt."
    fi

    cat <<EOF > /etc/g-tun/client_config.json
{
    "control_server_address": "$SERVER_IP:$CONTROL_PORT",
    "remote_server_ip": "$SERVER_IP",
    "data_port": "$DATA_PORT",
    "local_listen_port": "0.0.0.0:$LOCAL_PORT",
    "token": "$SECRET_TOKEN",
    "kcp_config": {
        "NoDelay": 1, "Interval": 20, "Resend": 2, "NoCongestion": 1,
        "SndWnd": 4096, "RcvWnd": 4096, "DataShards": 10, "ParityShards": 3
    }
}
EOF

    echo "Building Client Binary..."
    cd /root/G-tun-Project/client
    
    /usr/local/go/bin/go get github.com/quic-go/quic-go
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
Environment="GOGC=400"
Environment="GOMEMLIMIT=768MiB"

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable g-tun-client
    systemctl restart g-tun-client
    
    cp /root/G-tun-Project/g-tun.sh /usr/bin/g-tun
    chmod +x /usr/bin/g-tun
    
    echo "=========================================================================="
    echo " Client setup complete and running in background!"
    echo " You can manage the tunnel anytime by typing: g-tun"
    echo "=========================================================================="
fi