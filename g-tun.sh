#!/bin/bash

# ===================================================
#   G-Tun Management Script
#   Powered by Go & Systemd
# ===================================================


INSTALL_DIR="/usr/local/g-tun"
GO_BIN="/usr/local/go/bin/go"
SERVICE_NAME="g-tun"
SCREEN_NAME="g-tun_console"

# --- Colors ---
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# --- Core Functions ---

check_root() {
    if [ "$EUID" -ne 0 ]; then
        echo -e "${RED}Please run as root (sudo).${NC}"
        exit 1
    fi
}

install_deps() {
    echo -e "${YELLOW}[G-Tun] Installing System Dependencies...${NC}"
    apt update -q && apt install -y -q git curl wget tar lsof psmisc nano screen net-tools vnstat

    if [ ! -f "$GO_BIN" ]; then
        echo -e "${YELLOW}[G-Tun] Installing Go 1.23.1...${NC}"
        wget -q https://go.dev/dl/go1.23.1.linux-amd64.tar.gz
        rm -rf /usr/local/go && tar -C /usr/local -xzf go1.23.1.linux-amd64.tar.gz
        rm go1.23.1.linux-amd64.tar.gz
        export PATH=$PATH:/usr/local/go/bin
    fi

    # Generate TLS Certs if missing
    if [ ! -f "$INSTALL_DIR/server/cert.pem" ]; then
        echo -e "${YELLOW}[G-Tun] Generating TLS Certificates...${NC}"
        cd "$INSTALL_DIR/server" && "$GO_BIN" run generate_cert.go
    fi

    echo -e "${YELLOW}[G-Tun] Fixing Go Modules...${NC}"
    for dir in "$INSTALL_DIR/server" "$INSTALL_DIR/client"; do
        cd "$dir"
        "$GO_BIN" mod tidy 2>/dev/null
        "$GO_BIN" get github.com/gorilla/websocket
        "$GO_BIN" get github.com/xtaci/kcp-go/v5
        "$GO_BIN" get github.com/xtaci/smux
    done

    echo -e "${YELLOW}[G-Tun] Building Binaries...${NC}"
    
    # Build Server
    cd "$INSTALL_DIR/server"
    if "$GO_BIN" build -o g-tun-server server.go; then
        echo -e "${GREEN}✔ Server Built Successfully${NC}"
    else
        echo -e "${RED}✘ Server Build Failed!${NC}"; exit 1
    fi

    # Build Client
    cd "$INSTALL_DIR/client"
    if "$GO_BIN" build -o g-tun-client client.go; then
        echo -e "${GREEN}✔ Client Built Successfully${NC}"
    else
        echo -e "${RED}✘ Client Build Failed!${NC}"; exit 1
    fi

    chmod +x "$INSTALL_DIR/server/g-tun-server"
    chmod +x "$INSTALL_DIR/client/g-tun-client"
}

create_service() {
    local role=$1
    local exec_cmd=""
    local work_dir=""

    if [ "$role" == "client" ]; then
        work_dir="$INSTALL_DIR/client"
        exec_cmd="./g-tun-client"
    else
        work_dir="$INSTALL_DIR/server"
        exec_cmd="./g-tun-server"
    fi

    if [ ! -f "$work_dir/g-tun-$role" ]; then
         echo -e "${RED}Error: Binary not found. Run Install/Build first.${NC}"
         return
    fi

    echo -e "${YELLOW}[G-Tun] Creating Service for $role...${NC}"

    # Use 'sleep 10' to keep screen open on crash for debugging
    cat <<EOF > /etc/systemd/system/$SERVICE_NAME.service
[Unit]
Description=G-Tun Service ($role)
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$work_dir
ExecStart=/usr/bin/screen -DmS $SCREEN_NAME /bin/bash -c "$exec_cmd || sleep 10"
ExecStop=/usr/bin/screen -S $SCREEN_NAME -X quit
Restart=always
RestartSec=3s

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable $SERVICE_NAME
    echo -e "${GREEN}✔ Service Installed & Enabled.${NC}"
}

configure() {
    echo -e "${BLUE}=== G-Tun Configuration ===${NC}"
    echo "1) Iran (Client)"
    echo "2) Foreign (Server)"
    read -p "Select Role: " role

    if [ "$role" == "1" ]; then
        # CLIENT
        read -p "Foreign IP: " ip
        read -p "Foreign Control Port [8880]: " cport; cport=${cport:-8880}
        read -p "Local Proxy Port [2054]: " lport; lport=${lport:-2054}

        cat <<EOF > "$INSTALL_DIR/client/client_config.json"
{
    "ControlServerAddress": "$ip:$cport",
    "LocalListenPort": "0.0.0.0:$lport",
    "RemoteServerIP": "$ip",
    "KcpConfig": { "NoDelay": 1, "Interval": 10, "Resend": 2, "NoCongestion": 1, "SndWnd": 1024, "RcvWnd": 1024, "DataShards": 10, "ParityShards": 3 }
}
EOF
        create_service "client"

    elif [ "$role" == "2" ]; then
        # SERVER
        read -p "Listen Control Port [8880]: " cport; cport=${cport:-8880}
        read -p "Target Xray Address [127.0.0.1:1080]: " target; target=${target:-127.0.0.1:1080}

        echo -e "${YELLOW}Clearing port $cport...${NC}"
        fuser -k -n tcp $cport 2> /dev/null

        cat <<EOF > "$INSTALL_DIR/server/server_config.json"
{
    "ControlPort": "$cport",
    "DataPorts": { "TCP": "9091", "UDP": "9092", "WS": "9093", "TCPMux": "9094", "WSMux": "9095", "WSS": "9096", "WSSMux": "9097", "UTCPMux": "9098" },
    "XrayInboundAddress": "$target",
    "TlsCertPath": "cert.pem", "TlsKeyPath": "key.pem",
    "KcpConfig": { "NoDelay": 1, "Interval": 10, "Resend": 2, "NoCongestion": 1, "SndWnd": 1024, "RcvWnd": 1024, "DataShards": 10, "ParityShards": 3 }
}
EOF
        create_service "server"
    fi
    echo -e "${GREEN}✔ Config Saved.${NC}"
}

enter_console() {
    echo -e "${YELLOW}Connecting to G-Tun Console...${NC}"
    sleep 2
    
    if screen -list | grep -q "$SCREEN_NAME"; then
        echo -e "${CYAN}-------------------------------------------------------${NC}"
        echo -e " >> Wait for 'Client Connected' -> Select Protocol"
        echo -e " >> To EXIT keeping tunnel alive: Press ${GREEN}Ctrl+A${NC} then ${GREEN}D${NC}"
        echo -e "${CYAN}-------------------------------------------------------${NC}"
        read -p "Press Enter..."
        screen -r $SCREEN_NAME
    else
        echo -e "${RED}Error: Service not running or crashed.${NC}"
        journalctl -u $SERVICE_NAME -n 10 --no-pager
    fi
}

start_tunnel() {
    systemctl stop $SERVICE_NAME
    killall -9 g-tun-server g-tun-client 2>/dev/null
    systemctl start $SERVICE_NAME
    echo -e "${GREEN}✔ Service Started.${NC}"
    
    # Auto-Console for Server
    if [ -f "$INSTALL_DIR/server/server_config.json" ]; then
        if grep -q "ControlPort" "$INSTALL_DIR/server/server_config.json"; then
             enter_console
        else
             echo -e "${BLUE}Client running in background.${NC}"
        fi
    fi
}

stop_tunnel() {
    systemctl stop $SERVICE_NAME
    screen -S $SCREEN_NAME -X quit 2>/dev/null
    echo -e "${RED}Tunnel Stopped.${NC}"
}

status_panel() {
    echo -e "\n${BLUE}=== G-Tun Status ===${NC}"
    systemctl is-active --quiet $SERVICE_NAME && echo -e "Service: ${GREEN}Active ●${NC}" || echo -e "Service: ${RED}Inactive ●${NC}"
    echo -e "--- Ports ---"
    netstat -tulpn | grep -E 'g-tun'
}

uninstall() {
    read -p "Are you sure? (y/n): " confirm
    if [[ "$confirm" == "y" ]]; then
        stop_tunnel
        systemctl disable $SERVICE_NAME
        rm /etc/systemd/system/$SERVICE_NAME.service
        systemctl daemon-reload
        rm -rf "$INSTALL_DIR"
        rm /usr/bin/g-tun
        echo -e "${RED}G-Tun Uninstalled Completely.${NC}"
        exit 0
    fi
}

# --- Main Menu ---
check_root

# If script is run with 'install' arg (internal use)
if [ "$1" == "install" ]; then
    install_deps
    exit 0
fi

while true; do
    echo -e "\n${CYAN}   G-Tun Manager v1.0   ${NC}"
    echo -e "${BLUE}========================${NC}"
    echo "1) Re-Install / Update / Fix Build"
    echo "2) Configure (Server/Client)"
    echo "3) Start Tunnel"
    echo "4) Stop Tunnel"
    echo "5) Status"
    echo "6) Uninstall"
    echo "0) Exit"
    echo -e "${BLUE}========================${NC}"
    read -p "Select: " opt
    case $opt in
        1) install_deps ;;
        2) configure ;;
        3) start_tunnel ;;
        4) stop_tunnel ;;
        5) status_panel ;;
        6) uninstall ;;
        0) exit 0 ;;
    esac
done