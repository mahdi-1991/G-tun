package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

type KcpConfig struct {
	NoDelay      int
	Interval     int
	Resend       int
	NoCongestion int
	SndWnd       int
	RcvWnd       int
	DataShards   int
	ParityShards int
}

type ClientConfig struct {
	ControlServerAddress string    `json:"control_server_address"`
	LocalListenPort      string    `json:"local_listen_port"`
	RemoteServerIP       string    `json:"remote_server_ip"`
	DataPort             string    `json:"data_port"`
	Token                string    `json:"token"`
	KcpConfig            KcpConfig `json:"kcp_config"`
}

var config ClientConfig
var bufferPool = sync.Pool{
	New: func() interface{} { b := make([]byte, 64*1024); return &b },
}

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
}

type TransportConfig struct {
	Protocol string `json:"protocol"`
	Port     string `json:"port"`
}

type wsConnWrapper struct {
	*websocket.Conn
	r io.Reader
}

func (c *wsConnWrapper) Read(b []byte) (int, error) {
	if c.r == nil {
		_, r, err := c.NextReader()
		if err != nil { return 0, err }
		c.r = r
	}
	n, err := c.r.Read(b)
	if err == io.EOF { c.r = nil; err = nil }
	return n, err
}

func (c *wsConnWrapper) Write(b []byte) (int, error) {
	err := c.WriteMessage(websocket.BinaryMessage, b)
	if err != nil { return 0, err }
	return len(b), nil
}

func logInfo(message string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
}

func loadClientConfiguration() {
	file, err := os.Open("client_config.json")
	if err != nil { os.Exit(1) }
	defer file.Close()
	json.NewDecoder(file).Decode(&config)
}

func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

func startTcpDataForwarder(ctx context.Context, dataPort string) {
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	go func() { <-ctx.Done(); listener.Close() }()
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil { return }
			defer rconn.Close()
			go relayConnections(rconn, lconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}

func startUdpDataForwarder(ctx context.Context, dataPort string) {
	localAddr, _ := net.ResolveUDPAddr("udp", config.LocalListenPort)
	localConn, _ := net.ListenUDP("udp", localAddr)
	go func() { <-ctx.Done(); localConn.Close() }()
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	sessions := make(map[string]*net.UDPConn)
	var mu sync.Mutex
	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := localConn.ReadFromUDP(buf)
		if err != nil { return }
		mu.Lock()
		remoteConn, ok := sessions[clientAddr.String()]
		if !ok {
			udpServerAddr, _ := net.ResolveUDPAddr("udp", remoteDataAddr)
			remoteConn, _ = net.DialUDP("udp", nil, udpServerAddr)
			sessions[clientAddr.String()] = remoteConn
			go func(lconn *net.UDPConn, rconn *net.UDPConn, cAddr *net.UDPAddr) {
				remoteBufPtr := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(remoteBufPtr)
				for {
					m, err := rconn.Read(*remoteBufPtr)
					if err != nil {
						mu.Lock()
						delete(sessions, cAddr.String())
						mu.Unlock()
						rconn.Close()
						return
					}
					lconn.WriteToUDP((*remoteBufPtr)[:m], cAddr)
				}
			}(localConn, remoteConn, clientAddr)
		}
		mu.Unlock()
		remoteConn.Write(buf[:n])
	}
}

func relayWs(localConn net.Conn, wsConn *websocket.Conn) {
	errChan := make(chan error, 2)
	go func() {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		for {
			n, err := localConn.Read(buf)
			if err != nil { errChan <- err; return }
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil { errChan <- err; return }
		}
	}()
	go func() {
		for {
			mt, message, err := wsConn.ReadMessage()
			if err != nil { errChan <- err; return }
			if mt == websocket.BinaryMessage {
				if _, err := localConn.Write(message); err != nil { errChan <- err; return }
			}
		}
	}()
	<-errChan
}

func startWsDataForwarder(ctx context.Context, dataPort string) {
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	go func() { <-ctx.Done(); listener.Close() }()
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/ws"}
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil { return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func handleLocalMuxConnection(lconn net.Conn, session *smux.Session) {
	defer lconn.Close()
	stream, err := session.OpenStream()
	if err != nil { return }
	defer stream.Close()
	go relayConnections(stream, lconn)
	relayConnections(lconn, stream)
}

func startTcpMuxDataForwarder(ctx context.Context, dataPort string) {
	baseConn, err := net.Dial("tcp", config.RemoteServerIP+":"+dataPort)
	if err != nil { return }
	session, err := smux.Client(baseConn, nil)
	if err != nil { return }
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	go func() { <-ctx.Done(); listener.Close(); session.Close() }()
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startWsMuxDataForwarder(ctx context.Context, dataPort string) {
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wsmux"}
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil { return }
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { return }
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	go func() { <-ctx.Done(); listener.Close(); session.Close() }()
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startWssDataForwarder(ctx context.Context, dataPort string) {
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	go func() { <-ctx.Done(); listener.Close() }()
	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wss"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := dialer.Dial(u.String(), nil)
			if err != nil { return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func startWssMuxDataForwarder(ctx context.Context, dataPort string) {
	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wssmux"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	ws, _, err := dialer.Dial(u.String(), nil)
	if err != nil { return }
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { return }
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	go func() { <-ctx.Done(); listener.Close(); session.Close() }()
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startUtcpMuxDataForwarder(ctx context.Context, dataPort string) {
	kcpConf := config.KcpConfig
	baseConn, err := kcp.DialWithOptions(config.RemoteServerIP+":"+dataPort, nil, kcpConf.DataShards, kcpConf.ParityShards)
	if err != nil { return }
	baseConn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
	baseConn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
	session, err := smux.Client(baseConn, nil)
	if err != nil { return }
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	go func() { <-ctx.Done(); listener.Close(); session.Close() }()
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func main() {
	loadClientConfiguration()
	var currentCancel context.CancelFunc

	for {
		logInfo("Connecting to control server...")
		conn, err := net.Dial("tcp", config.ControlServerAddress)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}

		conn.Write([]byte(config.Token + "\n"))
		reader := bufio.NewReader(conn)
		
		msgStr, err := reader.ReadString('\n')
		if err != nil { conn.Close(); continue }
		
		var msg Message
		if err := json.Unmarshal([]byte(msgStr), &msg); err != nil {
			conn.Close(); continue
		}

		if msg.Command == "start_transport" {
			if currentCancel != nil { currentCancel() }
			ctx, cancel := context.WithCancel(context.Background())
			currentCancel = cancel

			var configData TransportConfig
			json.Unmarshal([]byte(msg.Payload), &configData)
			
			logInfo("Starting protocol: " + configData.Protocol)

			switch configData.Protocol {
			case "tcp": go startTcpDataForwarder(ctx, configData.Port)
			case "udp": go startUdpDataForwarder(ctx, configData.Port)
			case "ws": go startWsDataForwarder(ctx, configData.Port)
			case "tcpmux": go startTcpMuxDataForwarder(ctx, configData.Port)
			case "wsmux": go startWsMuxDataForwarder(ctx, configData.Port)
			case "wss": go startWssDataForwarder(ctx, configData.Port)
			case "wssmux": go startWssMuxDataForwarder(ctx, configData.Port)
			case "utcpmux": go startUtcpMuxDataForwarder(ctx, configData.Port)
			}
		}

		reader.ReadString('\n') // Block until server disconnects
		conn.Close()
	}
}