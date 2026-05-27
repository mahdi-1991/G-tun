// Package main implements the G-Tun client.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// Config holds the client configuration parameters.
type Config struct {
	ServerIP    string `json:"server_ip"`
	ControlPort string `json:"control_port"`
	DataPort    string `json:"data_port"`
	LocalPort   string `json:"local_port"`
	Token       string `json:"token"`
}

func main() {
	configPath := flag.String("config", "client_config.json", "Path to config file")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Start the robust control loop
	startControlLoop(cfg)
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

func startControlLoop(cfg Config) {
	var currentCancel context.CancelFunc

	for {
		targetAddr := fmt.Sprintf("%s:%s", cfg.ServerIP, cfg.ControlPort)
		log.Printf("Attempting to connect to control server at %s...", targetAddr)

		conn, err := net.Dial("tcp", targetAddr)
		if err != nil {
			log.Printf("Connection failed, retrying in 5 seconds... (%v)", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// 1. Send Authentication Token
		_, err = conn.Write([]byte(cfg.Token + "\n"))
		if err != nil {
			log.Printf("Failed to send token: %v", err)
			conn.Close()
			time.Sleep(3 * time.Second)
			continue
		}

		// 2. Read Server Commands
		reader := bufio.NewReader(conn)
		for {
			msg, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("Control connection lost: %v", err)
				break // Break inner loop to trigger reconnect
			}

			msg = strings.TrimSpace(msg)
			if strings.HasPrefix(msg, "start_transport") {
				parts := strings.Split(msg, " ")
				if len(parts) >= 2 {
					protocol := parts[1]
					log.Printf("Received start command for protocol: %s", protocol)

					// Cancel the previous listener if it exists
					if currentCancel != nil {
						currentCancel()
					}

					// Create a new context for the new local listener
					ctx, cancel := context.WithCancel(context.Background())
					currentCancel = cancel

					// Start local listener in a new goroutine
					go startLocalListener(ctx, cfg)
				}
			}
		}

		// Clean up before retrying
		conn.Close()
		if currentCancel != nil {
			currentCancel()
		}
		time.Sleep(3 * time.Second)
	}
}

func startLocalListener(ctx context.Context, cfg Config) {
	addr := ":" + cfg.LocalPort
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("Failed to start local listener on %s: %v", addr, err)
		return
	}
	log.Printf("Local listener started on %s. Ready to proxy traffic.", addr)

	// Goroutine to cleanly shutdown the listener when context is canceled
	go func() {
		<-ctx.Done()
		log.Printf("Shutting down local listener on %s", addr)
		listener.Close()
	}()

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			// If context is canceled, exit the loop gracefully
			if ctx.Err() != nil {
				return
			}
			log.Printf("Failed to accept local connection: %v", err)
			continue
		}

		go handleProxyConnection(clientConn, cfg)
	}
}

func handleProxyConnection(localConn net.Conn, cfg Config) {
	serverAddr := fmt.Sprintf("%s:%s", cfg.ServerIP, cfg.DataPort)
	serverConn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		log.Printf("Failed to connect to remote data server at %s: %v", serverAddr, err)
		localConn.Close()
		return
	}

	// Relay data bidirectionally
	relay(localConn, serverConn)
}

// relay bidirectionally copies data between two connections safely.
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

	<-errc
}