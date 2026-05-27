cat << 'EOF' > /usr/bin/g-tun
#!/bin/bash
# G-Tun Full Control Panel

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m' 

if [ "$EUID" -ne 0 ]; then
  echo -e "${RED}Please run as root.${NC}"
  exit 1
fi

if [ -f "/etc/systemd/system/g-tun-server.service" ]; then
    ROLE="Server"
    SVC="g-tun-server"
    CONF="/etc/g-tun/server_config.json"
elif [ -f "/etc/systemd/system/g-tun-client.service" ]; then
    ROLE="Client"
    SVC="g-tun-client"
    CONF="/etc/g-tun/client_config.json"
else
    echo -e "${RED}Error: G-Tun is not installed.${NC}"
    exit 1
fi

update_gtun() {
    echo -e "${YELLOW}Updating G-Tun from GitHub...${NC}"
    cd /root/G-tun-Project
    git pull origin main || echo -e "${RED}Failed to pull from GitHub.${NC}"
    
    echo -e "${YELLOW}Rebuilding binaries...${NC}"
    if [ "$ROLE" == "Server" ]; then
        cd server
        /usr/local/go/bin/go mod tidy
        /usr/local/go/bin/go build -o g-tun-server server.go
        mv g-tun-server /usr/local/bin/
    else
        cd client
        /usr/local/go/bin/go mod tidy
        /usr/local/go/bin/go build -o g-tun-client client.go
        mv g-tun-client /usr/local/bin/
    fi
    
    systemctl restart $SVC
    echo -e "${GREEN}Update completed successfully! Service restarted.${NC}"
    sleep 2
    show_menu
}

uninstall_gtun() {
    echo -e "${RED}WARNING: You are about to completely uninstall G-Tun!${NC}"
    read -p "Are you sure? (y/n): " CONFIRM
    if [[ "$CONFIRM" == "y" || "$CONFIRM" == "Y" ]]; then
        echo -e "${YELLOW}Stopping and disabling services...${NC}"
        systemctl stop $SVC 2>/dev/null
        systemctl disable $SVC 2>/dev/null
        rm -f /etc/systemd/system/$SVC.service
        systemctl daemon-reload
        
        echo -e "${YELLOW}Removing binaries and configurations...${NC}"
        rm -f /usr/local/bin/g-tun-server /usr/local/bin/g-tun-client
        rm -rf /etc/g-tun
        rm -rf /root/G-tun-Project
        
        echo -e "${GREEN}G-Tun has been completely uninstalled.${NC}"
        rm -f /usr/bin/g-tun
        exit 0
    else
        echo -e "${GREEN}Uninstall cancelled.${NC}"
        sleep 1
        show_menu
    fi
}

show_menu() {
    clear
    echo -e "${CYAN}==========================================${NC}"
    echo -e "${GREEN}        G-TUN FULL CONTROL PANEL        ${NC}"
    echo -e "${CYAN}==========================================${NC}"
    
    STATUS=$(systemctl is-active $SVC || true)
    if [ "$STATUS" == "active" ]; then
        echo -e " Mode:   [${YELLOW}$ROLE${NC}]"
        echo -e " Status: [${GREEN}RUNNING${NC}]"
    else
        echo -e " Mode:   [${YELLOW}$ROLE${NC}]"
        echo -e " Status: [${RED}STOPPED${NC}]"
    fi
    echo -e "${CYAN}------------------------------------------${NC}"
    echo " 1) Start G-Tun Service"
    echo " 2) Stop G-Tun Service"
    echo " 3) Restart G-Tun Service"
    echo " 4) View Live Logs (Press Ctrl+C to exit)"
    echo " 5) View Full Configuration"
    echo " 6) Show Secret Token"
    echo " 7) Edit Configuration (Requires Restart)"
    echo " 8) Update G-Tun to Latest Version"
    echo " 9) Uninstall G-Tun Completely"
    echo " 0) Exit Panel"
    echo -e "${CYAN}==========================================${NC}"
    read -p " Enter your choice [0-9]: " CHOICE

    case $CHOICE in
        1) systemctl start $SVC; echo -e "${GREEN}Service Started.${NC}"; sleep 1; show_menu ;;
        2) systemctl stop $SVC; echo -e "${RED}Service Stopped.${NC}"; sleep 1; show_menu ;;
        3) systemctl restart $SVC; echo -e "${YELLOW}Service Restarted.${NC}"; sleep 1; show_menu ;;
        4) echo -e "${YELLOW}Fetching logs... Press Ctrl+C to stop.${NC}"; journalctl -u $SVC -f ;;
        5) echo -e "\n${CYAN}--- Current Configuration ---${NC}"; cat $CONF; echo ""; read -p "Press Enter to return..." key; show_menu ;;
        6) TOKEN=$(grep '"token"' $CONF | cut -d '"' -f 4); echo -e "\n${YELLOW}--- Your Secret Token ---${NC}\n$TOKEN\n"; read -p "Press Enter to return..." key; show_menu ;;
        7) nano $CONF; systemctl restart $SVC; echo -e "${GREEN}Config saved and service restarted.${NC}"; sleep 1; show_menu ;;
        8) update_gtun ;;
        9) uninstall_gtun ;;
        0) clear; exit 0 ;;
        *) echo -e "${RED}Invalid choice!${NC}"; sleep 1; show_menu ;;
    esac
}

show_menu
EOF

chmod +x /usr/bin/g-tun
echo "Control menu updated with Update and Uninstall options! Type 'g-tun' to open it."