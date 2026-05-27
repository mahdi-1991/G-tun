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

# Detect if the current machine is Server or Client
if [ -f "/etc/systemd/system/g-tun-server.service" ]; then
    ROLE="Server"
    SVC="g-tun-server"
    CONF="/etc/g-tun/server_config.json"
elif [ -f "/etc/systemd/system/g-tun-client.service" ]; then
    ROLE="Client"
    SVC="g-tun-client"
    CONF="/etc/g-tun/client_config.json"
else
    echo -e "${RED}Error: G-Tun is not installed. Please run install.sh first.${NC}"
    exit 1
fi

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
    echo " 0) Exit Panel"
    echo -e "${CYAN}==========================================${NC}"
    read -p " Enter your choice [0-7]: " CHOICE

    case $CHOICE in
        1) 
            systemctl start $SVC
            echo -e "${GREEN}Service Started.${NC}"
            sleep 1; show_menu 
            ;;
        2) 
            systemctl stop $SVC
            echo -e "${RED}Service Stopped.${NC}"
            sleep 1; show_menu 
            ;;
        3) 
            systemctl restart $SVC
            echo -e "${YELLOW}Service Restarted.${NC}"
            sleep 1; show_menu 
            ;;
        4) 
            echo -e "${YELLOW}Fetching logs... Press Ctrl+C to stop.${NC}"
            journalctl -u $SVC -f 
            ;;
        5) 
            echo -e "\n${CYAN}--- Current Configuration ---${NC}"
            cat $CONF
            echo ""
            read -p "Press Enter to return..." key
            show_menu 
            ;;
        6) 
            TOKEN=$(grep '"token"' $CONF | cut -d '"' -f 4)
            echo -e "\n${YELLOW}--- Your Secret Token ---${NC}\n$TOKEN\n"
            read -p "Press Enter to return..." key
            show_menu 
            ;;
        7) 
            nano $CONF
            systemctl restart $SVC
            echo -e "${GREEN}Config saved and service restarted.${NC}"
            sleep 1; show_menu
            ;;
        0) 
            clear; exit 0 
            ;;
        *) 
            echo -e "${RED}Invalid choice!${NC}"
            sleep 1; show_menu 
            ;;
    esac
}

show_menu