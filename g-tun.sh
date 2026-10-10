#!/usr/bin/env bash
# G-Tun control panel.
set -uo pipefail

readonly RED=$'\033[0;31m' GREEN=$'\033[0;32m' YELLOW=$'\033[0;33m' CYAN=$'\033[0;36m' NC=$'\033[0m'
readonly CONF_DIR="/etc/g-tun"
readonly WORK_DIR="/root/G-tun-Project"
readonly UNIT_DIR="/etc/systemd/system"

if [[ ${EUID} -ne 0 ]]; then
  echo "${RED}Please run as root.${NC}"
  exit 1
fi

if [[ -f ${UNIT_DIR}/g-tun-server.service ]]; then
  ROLE=server
elif [[ -f ${UNIT_DIR}/g-tun-client.service ]]; then
  ROLE=client
else
  echo "${RED}Error: G-Tun is not installed.${NC}"
  exit 1
fi
readonly ROLE
readonly SERVICE="g-tun-${ROLE}"
readonly CONFIG="${CONF_DIR}/${ROLE}_config.json"
readonly BINARY="/usr/local/bin/g-tun-${ROLE}"

pause() {
  read -r -p "Press Enter to return..." _
}

show_status() {
  if [[ $(systemctl is-active "${SERVICE}") == active ]]; then
    echo " Status: [${GREEN}RUNNING${NC}]"
  else
    echo " Status: [${RED}STOPPED${NC}]"
  fi
}

show_token() {
  local token
  token="$(sed -n 's/.*"token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${CONFIG}" | head -n 1)"
  echo -e "\n${YELLOW}--- Your Secret Token ---${NC}\n${token}\n"
}

edit_config() {
  "${EDITOR:-nano}" "${CONFIG}"
  if "${BINARY}" -config "${CONFIG}" -check > /dev/null; then
    systemctl restart "${SERVICE}"
    echo "${GREEN}Config saved and service restarted.${NC}"
  else
    echo "${RED}The configuration is invalid; the service was NOT restarted:${NC}"
    "${BINARY}" -config "${CONFIG}" -check
  fi
}

update_gtun() {
  echo "${YELLOW}Updating G-Tun from GitHub...${NC}"
  if bash "${WORK_DIR}/install.sh" --update; then
    echo "${GREEN}Update completed successfully.${NC}"
  else
    echo "${RED}Update failed.${NC}"
  fi
}

uninstall_gtun() {
  echo "${RED}WARNING: You are about to completely uninstall G-Tun!${NC}"
  local answer
  read -r -p "Are you sure? (y/n): " answer
  if [[ ${answer} != [yY] ]]; then
    echo "${GREEN}Uninstall cancelled.${NC}"
    return
  fi
  systemctl disable --now "${SERVICE}" 2> /dev/null
  rm -f "${UNIT_DIR}/${SERVICE}.service" "${BINARY}" /etc/sysctl.d/99-gtun.conf
  systemctl daemon-reload
  rm -rf "${CONF_DIR}" "${WORK_DIR}" /usr/bin/g-tun
  echo "${GREEN}G-Tun has been completely uninstalled.${NC}"
  exit 0
}

show_menu() {
  clear
  echo "${CYAN}==========================================${NC}"
  echo "${GREEN}        G-TUN FULL CONTROL PANEL          ${NC}"
  echo "${CYAN}==========================================${NC}"
  echo " Mode:   [${YELLOW}${ROLE^}${NC}]"
  show_status
  echo "${CYAN}------------------------------------------${NC}"
  echo " 1) Start G-Tun Service"
  echo " 2) Stop G-Tun Service"
  echo " 3) Restart G-Tun Service"
  echo " 4) View Live Logs (Press Ctrl+C to exit)"
  echo " 5) View Full Configuration"
  echo " 6) Show Secret Token"
  echo " 7) Edit Configuration"
  echo " 8) Update G-Tun to Latest Version"
  echo " 9) Uninstall G-Tun Completely"
  echo " 0) Exit Panel"
  echo "${CYAN}==========================================${NC}"
}

main() {
  local choice
  while true; do
    show_menu
    read -r -p " Enter your choice [0-9]: " choice
    case ${choice} in
      1) systemctl start "${SERVICE}" && echo "${GREEN}Service started.${NC}"; sleep 1 ;;
      2) systemctl stop "${SERVICE}" && echo "${RED}Service stopped.${NC}"; sleep 1 ;;
      3) systemctl restart "${SERVICE}" && echo "${YELLOW}Service restarted.${NC}"; sleep 1 ;;
      4) journalctl -u "${SERVICE}" -f || true ;;
      5) echo -e "\n${CYAN}--- Current Configuration ---${NC}"; cat "${CONFIG}"; echo; pause ;;
      6) show_token; pause ;;
      7) edit_config; sleep 2 ;;
      8) update_gtun; pause ;;
      9) uninstall_gtun ;;
      0) clear; exit 0 ;;
      *) echo "${RED}Invalid choice!${NC}"; sleep 1 ;;
    esac
  done
}

main
