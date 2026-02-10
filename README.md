<div align="center">

# 🚀 G-Tun
### Advanced High-Performance Go Tunnel

![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8?style=for-the-badge&logo=go)
![Platform](https://img.shields.io/badge/Platform-Linux-linux?style=for-the-badge&logo=linux)
![License](https://img.shields.io/badge/License-MIT-green?style=for-the-badge)

<p align="center">
  <b>A lightweight, secure, and multi-protocol tunneling solution written in Golang.</b><br>
  Designed to bypass network restrictions by establishing a robust connection between a client and a server using 
  <b>TCP, UDP, WebSocket, KCP, and Multiplexing.</b>
</p>

</div>

---

## ✨ Features

| Feature | Description |
| :--- | :--- |
| 🛡️ **Multi-Protocol** | Supports **TCP**, **UDP**, **WebSocket (WS)**, **Secure WebSocket (WSS)**, and **KCP**. |
| ⚡ **Multiplexing** | Boosts performance using **Smux** (TCPMux, WSMux, KCPMux) to run multiple streams over a single connection. |
| 🔒 **Security** | Auto-generates **TLS Certificates** for encrypted WSS connections. |
| 🚀 **High Speed** | Optimized for low latency and high throughput on weak networks. |
| ⚙️ **Easy Management** | Includes a powerful **TUI (Text User Interface)** bash script for setup and monitoring. |
| 🔄 **Auto-Resume** | Integrated with **Systemd** & **Screen** for persistence after reboots or crashes. |

---

## 📥 Quick Installation

### 🖥️ Supported Operating Systems
<p align="left">
  <img src="https://img.shields.io/badge/Ubuntu-24.04-E95420?style=flat-square&logo=ubuntu&logoColor=white" alt="Ubuntu 24.04">
  <img src="https://img.shields.io/badge/Ubuntu-22.04-E95420?style=flat-square&logo=ubuntu&logoColor=white" alt="Ubuntu 22.04">
  <img src="https://img.shields.io/badge/Ubuntu-20.04-E95420?style=flat-square&logo=ubuntu&logoColor=white" alt="Ubuntu 20.04">
</p>

Run the following command on your **VPS**. 
This script works for both the **Foreign Server** and the **Iran Client**.

```bash
bash <(curl -Ls https://raw.githubusercontent.com/mahdi-1991/G-tun/main/install.sh)
