#!/bin/bash

REPO_URL="https://github.com/mahdi-1991/G-tun.git"
INSTALL_DIR="/usr/local/g-tun"
BIN_LINK="/usr/bin/g-tun"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo -e "${GREEN}Welcome to G-Tun Installer...${NC}"

if ! command -v git &> /dev/null; then
    echo -e "${YELLOW}Installing Git...${NC}"
    apt update -q && apt install -y -q git
fi
if [ -d "$INSTALL_DIR" ]; then
    echo -e "${YELLOW}Cleaning up old/broken installation...${NC}"
    rm -rf "$INSTALL_DIR"
fi

echo -e "${GREEN}Cloning repository...${NC}"
git clone "$REPO_URL" "$INSTALL_DIR"

if [ ! -f "$INSTALL_DIR/g-tun.sh" ]; then
    echo -e "${RED}Error: Clone failed! Repository not found or empty.${NC}"
    echo -e "Check this URL manually: $REPO_URL"
    exit 1
fi

chmod +x "$INSTALL_DIR/g-tun.sh"

rm -f "$BIN_LINK"
ln -s "$INSTALL_DIR/g-tun.sh" "$BIN_LINK"

echo -e "${GREEN}G-Tun core installed successfully!${NC}"
echo -e "${YELLOW}Installing dependencies...${NC}"

g-tun install

echo -e "\n${GREEN}Installation Finished!${NC}"
echo -e "Type ${RED}g-tun${NC} to open the menu."

g-tun
