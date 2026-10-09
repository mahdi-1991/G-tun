#!/bin/bash
# G-Tun Full Control Panel
# Refactored for Clean Code and ShellCheck compliance

readonly RED='\033[0;31m'
readonly GREEN='\033[0;32m'
readonly YELLOW='\033[0;33m'
readonly CYAN='\033[0;36m'
readonly NC='\033[0m'
readonly WORK_DIR="/root/G-tun-Project"
readonly GO_BIN="/usr/local/go/bin/go"

# Global states
ROLE=""
SVC=""
CONF=""

check_root() {
  if [[ "${EUID}" -ne 0 ]]; then
    echo -e "${RED}Error: Please run as root.${NC}" >&2
    exit 1
  fi
}

init_env() {
  if [[ -f "/etc/systemd/system/g-tun-server.service" ]]; then
    ROLE="Server"
    SVC="g-tun-server"
    CONF="/etc/g-tun/server_config.json"
  elif [[ -f "/etc/systemd/system/g-tun-client.service" ]]; then
    ROLE="Client"
    SVC="g-tun-client"
    CONF="/etc/g-tun/client_config.json"
  else
    echo -e "${RED}Error: G-Tun is not installed.${NC}" >&2
    exit 1
  fi
}

update_gtun() {
  echo -e "${YELLOW}Updating G-Tun from GitHub...${NC}"
  cd "${WORK_DIR}" || { echo -e "${RED}Work directory not found.${NC}"; sleep 2; return; }
  
  if ! git pull origin main; then
    echo -e "${RED}Failed to pull from GitHub.${NC}"
    sleep 2
    return
  fi
  
  echo -e "${YELLOW}Rebuilding binaries...${NC}"
  if [[ "${ROLE}" == "Server" ]]; then
    cd server || exit 1
    "${GO_BIN}" get github.com/quic-go/quic-go@v0.48.2
    "${GO_BIN}" mod tidy
    if ! "${GO_BIN}" build -o g-tun-server server.go; then
       echo -e "${RED}Build failed!${NC}"; sleep 2; return;
    fi
    mv g-tun-server /usr/local/bin/
  else
    cd client || exit 1
    "${GO_BIN}" get github.com/quic-go/quic-go@v0.48.2
    "${GO_BIN}" mod tidy
    if ! "${GO_BIN}" build -o g-tun-client client.go; then
       echo -e "${RED}Build failed!${NC}"; sleep 2; return;
    fi
    mv g-tun-client /usr/local/bin/
  fi
  
  systemctl restart "${SVC}"
  echo -e "${GREEN}Update completed successfully! Service restarted.${NC}"
  sleep 2
}

uninstall_gtun() {
  local confirm
  echo -e "${RED}WARNING: You are about to completely uninstall G-Tun!${NC}"
  read -r -p "Are you sure? (y/n): " confirm
  
  if [[ "${confirm,,}" == "y" ]]; then
    echo -e "${YELLOW}Stopping and disabling services...${NC}"
    systemctl stop "${SVC}" 2>/dev/null || true
    systemctl disable "${SVC}" 2>/dev/null || true
    rm -f "/etc/systemd/system/${SVC}.service"
    systemctl daemon-reload
    
    echo -e "${YELLOW}Removing binaries and configurations...${NC}"
    rm -f /usr/local/bin/g-tun-server /usr/local/bin/g-tun-client
    rm -rf /etc/g-tun
    rm -rf "${WORK_DIR}"
    
    echo -e "${GREEN}G-Tun has been completely uninstalled.${NC}"
    rm -f /usr/bin/g-tun
    exit 0
  else
    echo -e "${GREEN}Uninstall cancelled.${NC}"
    sleep 1
  fi
}

show_menu() {
  while true; do
    clear
    echo -e "${CYAN}==========================================${NC}"
    echo -e "${GREEN}        G-TUN FULL CONTROL PANEL        ${NC}"
    echo -e "${CYAN}==========================================${NC}"
    
    if systemctl is-active --quiet "${SVC}"; then
      echo -e " Mode:   [${YELLOW}${ROLE}${NC}]"
      echo -e " Status: [${GREEN}RUNNING${NC}]"
    else
      echo -e " Mode:   [${YELLOW}${ROLE}${NC}]"
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
    
    local choice
    read -r -p " Enter your choice [0-9]: " choice

    case "${choice}" in
      1) systemctl start "${SVC}"; echo -e "${GREEN}Service Started.${NC}"; sleep 1 ;;
      2) systemctl stop "${SVC}"; echo -e "${RED}Service Stopped.${NC}"; sleep 1 ;;
      3) systemctl restart "${SVC}"; echo -e "${YELLOW}Service Restarted.${NC}"; sleep 1 ;;
      4) echo -e "${YELLOW}Fetching logs... Press Ctrl+C to stop.${NC}"; journalctl -u "${SVC}" -f ;;
      5) echo -e "\n${CYAN}--- Current Configuration ---${NC}"; cat "${CONF}"; echo ""; read -r -p "Press Enter to return..." ;;
      6) 
         local token
         token=$(grep '"token"' "${CONF}" | cut -d '"' -f 4)
         echo -e "\n${YELLOW}--- Your Secret Token ---${NC}\n${token}\n"
         read -r -p "Press Enter to return..." 
         ;;
      7) nano "${CONF}"; systemctl restart "${SVC}"; echo -e "${GREEN}Config saved and service restarted.${NC}"; sleep 1 ;;
      8) update_gtun ;;
      9) uninstall_gtun ;;
      0) clear; exit 0 ;;
      *) echo -e "${RED}Invalid choice!${NC}"; sleep 1 ;;
    esac
  done
}

main() {
  check_root
  init_env
  show_menu
}

main "$@"