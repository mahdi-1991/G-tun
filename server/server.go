// Package main implements the G-Tun server.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
)

// Config holds the server configuration parameters.
type Config struct {
	ControlPort string `json:"control_port"`
	DataPort    string `json:"data_port"`
	XrayPort    string `json:"xray_port"`
	Protocol    string `json:"protocol"`
	Token       string `json:"token"`
}

func main() {
	configPath := flag.String("config", "server_config.json", "Path to config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Start data listener in a separate goroutine
	go startDataServer(cfg)

	// Start control listener on the main thread
	startControlServer(cfg)
}

func loadConfig(path string) (Config, error) {
	var cfg Config
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&cfg)
	return cfg, err
}

func startControlServer(cfg Config) {
	addr := ":" + cfg.ControlPort
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to start control server on %s: %v", addr, err)
	}
	log.Printf("Control server listening on %s", addr)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept control connection: %v", err)
			continue
		}
		go handleControlConnection(conn, cfg)
	}
}

func handleControlConnection(conn net.Conn, cfg Config) {
	defer conn.Close()
	remoteAddr := conn.RemoteAddr().String()
	log.Printf("New control connection attempt from %s", remoteAddr)

	reader := bufio.NewReader(conn)
	
	// Authentication Phase
	clientToken, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("Failed to read token from %s: %v", remoteAddr, err)
		return
	}

	clientToken = strings.TrimSpace(clientToken)
	if clientToken != cfg.Token {
		log.Printf("Unauthorized access attempt from %s", remoteAddr)
		return
	}
	log.Printf("Client %s authenticated successfully", remoteAddr)

	// Send Transport Command
	cmd := fmt.Sprintf("start_transport %s\n", cfg.Protocol)
	if _, err := conn.Write([]byte(cmd)); err != nil {
		log.Printf("Failed to send transport command to %s: %v", remoteAddr, err)
		return
	}

	// Block and wait for client disconnect
	if _, err := reader.ReadString('\n'); err != nil {
		log.Printf("Control connection with %s closed.", remoteAddr)
	}
}

func startDataServer(cfg Config) {
	addr := ":" + cfg.DataPort
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to start data server on %s: %v", addr, err)
	}
	log.Printf("Data server listening on %s for protocol: %s", addr, cfg.Protocol)

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			log.Printf("Failed to accept data connection: %v", err)
			continue
		}
		go handleDataConnection(clientConn, cfg)
	}
}

func handleDataConnection(clientConn net.Conn, cfg Config) {
	// Connect to the local Xray destination
	xrayAddr := "127.0.0.1:" + cfg.XrayPort
	xrayConn, err := net.Dial("tcp", xrayAddr)
	if err != nil {
		log.Printf("Failed to connect to Xray on %s: %v", xrayAddr, err)
		clientConn.Close()
		return
	}

	// Relay data bidirectionally
	relay(clientConn, xrayConn)
}

// relay bidirectionally copies data between two connections.
// It ensures that both connections are closed when one terminates, preventing goroutine leaks.
func relay(left, right net.Conn) {
	defer left.Close()
	defer right.Close()

	errc := make(chan error, 2)

	go func() {
		_, err := io.Copy(right, left)
		errc <- err
	}()

	go func() {
		_, err := io.Copy(left, right)
		errc <- err
	}()

	<-errc // Wait for the first copy operation to finish (EOF or Error)
}