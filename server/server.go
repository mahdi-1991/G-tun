package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/quic-go/quic-go"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

const (
	UDPIdleTimeout = 3 * time.Minute
	MaxUDPSize     = 65536
)

// ==================== Config Structs ====================

type KcpConfig struct {
	NoDelay      int `json:"NoDelay"`
	Interval     int `json:"Interval"`
	Resend       int `json:"Resend"`
	NoCongestion int `json:"NoCongestion"`
	SndWnd       int `json:"SndWnd"`
	RcvWnd       int `json:"RcvWnd"`
	DataShards   int `json:"DataShards"`
	ParityShards int `json:"ParityShards"`
}

type ServerConfig struct {
	ControlPort        string    `json:"control_port"`
	DataPort           string    `json:"data_port"`
	Protocol           string    `json:"protocol"`
	XrayInboundAddress string    `json:"xray_inbound_address"`
	Token              string    `json:"token"`
	TlsCertPath        string    `json:"tls_cert_path"`
	TlsKeyPath         string    `json:"tls_key_path"`
	KcpConfig          KcpConfig `json:"kcp_config"`
}

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
}

// ==================== Utils & Wrappers ====================

var bufferPool = sync.Pool{
	New: func() any { b := make([]byte, 64*1024); return &b },
}

func relayConnections(conn1, conn2 io.ReadWriteCloser) {
	defer conn1.Close()
	defer conn2.Close()
	errChan := make(chan error, 2)

	copyFunc := func(dst io.Writer, src io.Reader) {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		_, err := io.CopyBuffer(dst, src, *bufPtr)
		errChan <- err
	}

	go copyFunc(conn1, conn2)
	go copyFunc(conn2, conn1)
	<-errChan // Ensures both connections drop immediately when one is lost
}

type wsConnWrapper struct {
	*websocket.Conn
	reader io.Reader
}

func (w *wsConnWrapper) Read(b []byte) (int, error) {
	for {
		if w.reader == nil {
			msgType, r, err := w.NextReader()
			if err != nil {
				return 0, err
			}
			if msgType != websocket.BinaryMessage {
				continue
			}
			w.reader = r
		}
		n, err := w.reader.Read(b)
		if err == io.EOF {
			w.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (w *wsConnWrapper) Write(b []byte) (int, error) {
	if err := w.WriteMessage(websocket.BinaryMessage, b); err != nil {
		return 0, err
	}
	return len(b), nil
}

func getSmuxConfig() *smux.Config {
	conf := smux.DefaultConfig()
	conf.KeepAliveInterval = 10 * time.Second
	conf.KeepAliveTimeout = 30 * time.Second
	conf.MaxFrameSize = 32 * 1024
	conf.MaxReceiveBuffer = 4194304
	conf.MaxStreamBuffer = 65536
	return conf
}

func setKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(15 * time.Second)
	}
}

// ==================== Server Core ====================

type TunnelServer struct {
	config ServerConfig
}

func NewTunnelServer(cfg ServerConfig) *TunnelServer {
	return &TunnelServer{config: cfg}
}

func (s *TunnelServer) handleXrayStream(clientConn io.ReadWriteCloser) {
	defer clientConn.Close()
	xrayConn, err := net.Dial("tcp", s.config.XrayInboundAddress)
	if err != nil {
		slog.Error("Failed to dial Xray", "err", err)
		return
	}
	setKeepAlive(xrayConn)
	relayConnections(clientConn, xrayConn)
}

func (s *TunnelServer) startTCP() {
	listener, err := net.Listen("tcp", ":"+s.config.DataPort)
	if err != nil {
		slog.Error("TCP Listen Error", "err", err)
		return
	}
	slog.Info("TCP Listener started", "port", s.config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			setKeepAlive(conn)
			go s.handleXrayStream(conn)
		}
	}
}

func (s *TunnelServer) startUDP() {
	addr, _ := net.ResolveUDPAddr("udp", ":"+s.config.DataPort)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		slog.Error("UDP Listen Error", "err", err)
		return
	}
	slog.Info("UDP Listener started", "port", s.config.DataPort)

	type udpSession struct {
		conn     net.Conn
		lastSeen time.Time
	}
	sessions := make(map[string]*udpSession)
	var mapMutex sync.Mutex

	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			mapMutex.Lock()
			now := time.Now()
			for addrStr, sess := range sessions {
				if now.Sub(sess.lastSeen) > UDPIdleTimeout {
					sess.conn.Close()
					delete(sessions, addrStr)
					slog.Debug("UDP session expired", "client", addrStr)
				}
			}
			mapMutex.Unlock()
		}
	}()

	buf := make([]byte, MaxUDPSize)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		mapMutex.Lock()
		sess, ok := sessions[remoteAddr.String()]
		if !ok {
			xrayConn, err := net.Dial("tcp", s.config.XrayInboundAddress)
			if err != nil {
				slog.Error("Failed to dial Xray via UDP", "err", err)
				mapMutex.Unlock()
				continue
			}
			sess = &udpSession{conn: xrayConn, lastSeen: time.Now()}
			sessions[remoteAddr.String()] = sess

			go func(uConn *net.UDPConn, cAddr *net.UDPAddr, tConn net.Conn, addrStr string) {
				tBufPtr := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(tBufPtr)
				for {
					m, err := tConn.Read(*tBufPtr)
					if err != nil {
						mapMutex.Lock()
						delete(sessions, addrStr)
						mapMutex.Unlock()
						tConn.Close()
						return
					}
					uConn.WriteToUDP((*tBufPtr)[:m], cAddr)
				}
			}(conn, remoteAddr, xrayConn, remoteAddr.String())
		} else {
			sess.lastSeen = time.Now()
		}
		cConn := sess.conn
		mapMutex.Unlock()

		cConn.SetDeadline(time.Now().Add(UDPIdleTimeout))
		cConn.Write(buf[:n])
	}
}

// WebSocket Setup
var wsUpgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func (s *TunnelServer) wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err == nil {
		s.handleXrayStream(&wsConnWrapper{Conn: conn})
	}
}

func (s *TunnelServer) wsmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, getSmuxConfig())
	if err != nil {
		ws.Close()
		return
	}
	defer session.Close()
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}
		go s.handleXrayStream(stream)
	}
}

func (s *TunnelServer) startWebSockets(tlsEnabled bool) {
	mux := http.NewServeMux()
	var protoName, pathName string
	if tlsEnabled {
		protoName, pathName = "WSS", "/wss"
	} else {
		protoName, pathName = "WS", "/ws"
	}

	mux.HandleFunc(pathName, s.wsHandler)
	mux.HandleFunc(pathName+"mux", s.wsmuxHandler)

	srv := &http.Server{Addr: ":" + s.config.DataPort, Handler: mux}
	slog.Info(fmt.Sprintf("%s Listener started", protoName), "port", s.config.DataPort)

	if tlsEnabled {
		srv.ListenAndServeTLS(s.config.TlsCertPath, s.config.TlsKeyPath)
	} else {
		srv.ListenAndServe()
	}
}

// Mux & Protocol Handlers
func (s *TunnelServer) startTCPMux() {
	listener, err := net.Listen("tcp", ":"+s.config.DataPort)
	if err != nil {
		slog.Error("TCPMux Listen Error", "err", err)
		return
	}
	slog.Info("TCPMux Listener started", "port", s.config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			setKeepAlive(conn)
			go func(c net.Conn) {
				session, err := smux.Server(c, getSmuxConfig())
				if err != nil {
					c.Close()
					return
				}
				defer session.Close()
				for {
					stream, err := session.AcceptStream()
					if err != nil {
						break
					}
					go s.handleXrayStream(stream)
				}
			}(conn)
		}
	}
}

func (s *TunnelServer) startKCPMux() {
	kc := s.config.KcpConfig
	listener, err := kcp.ListenWithOptions(":"+s.config.DataPort, nil, kc.DataShards, kc.ParityShards)
	if err != nil {
		slog.Error("KCP Listen Error", "err", err)
		return
	}
	slog.Info("KCPMux Listener started", "port", s.config.DataPort)
	for {
		conn, err := listener.AcceptKCP()
		if err == nil {
			conn.SetNoDelay(kc.NoDelay, kc.Interval, kc.Resend, kc.NoCongestion)
			conn.SetWindowSize(kc.SndWnd, kc.RcvWnd)
			go func(c net.Conn) {
				session, err := smux.Server(c, getSmuxConfig())
				if err != nil {
					c.Close()
					return
				}
				defer session.Close()
				for {
					stream, err := session.AcceptStream()
					if err != nil {
						break
					}
					go s.handleXrayStream(stream)
				}
			}(conn)
		}
	}
}

func (s *TunnelServer) startQUIC() {
	cert, err := tls.LoadX509KeyPair(s.config.TlsCertPath, s.config.TlsKeyPath)
	if err != nil {
		slog.Error("QUIC TLS Load Error", "err", err)
		return
	}

	tlsConf := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"gtun-quic"},
	}

	quicConfig := &quic.Config{
		KeepAlivePeriod:    10 * time.Second,
		MaxIdleTimeout:     5 * time.Minute,
		MaxIncomingStreams: 10000,
	}

	listener, err := quic.ListenAddr(":"+s.config.DataPort, tlsConf, quicConfig)
	if err != nil {
		slog.Error("QUIC Listen Error", "err", err)
		return
	}
	slog.Info("QUIC (HTTP/3) Listener started", "port", s.config.DataPort)

	for {
		conn, err := listener.Accept(context.Background())
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func(c quic.Connection) {
			for {
				stream, err := c.AcceptStream(context.Background())
				if err != nil {
					break
				}
				go s.handleXrayStream(stream)
			}
		}(conn)
	}
}

// handleControlConnection securely authenticates via HMAC challenge
func (s *TunnelServer) handleControlConnection(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return
	}
	challengeHex := hex.EncodeToString(challenge)

	if _, err := conn.Write([]byte(challengeHex + "\n")); err != nil {
		return
	}

	reader := bufio.NewReader(conn)
	clientResponseHex, err := reader.ReadString('\n')
	if err != nil {
		return
	}

	mac := hmac.New(sha256.New, []byte(s.config.Token))
	mac.Write([]byte(challengeHex))
	expectedResponseHex := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(clientResponseHex)), []byte(expectedResponseHex)) != 1 {
		slog.Warn("Unauthorized access attempt", "ip", conn.RemoteAddr().String())
		return
	}

	slog.Info("Client authenticated successfully", "ip", conn.RemoteAddr().String())
	conn.SetDeadline(time.Time{})

	payload := fmt.Sprintf(`{"protocol":"%s","port":"%s"}`, s.config.Protocol, s.config.DataPort)
	msg := Message{Command: "start_transport", Payload: payload}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return
	}

	reader.ReadString('\n') // Block until client disconnects
	slog.Info("Client disconnected", "ip", conn.RemoteAddr().String())
}

func main() {
	file, err := os.Open("server_config.json")
	if err != nil {
		slog.Error("Failed to open server_config.json", "err", err)
		os.Exit(1)
	}
	defer file.Close()

	var cfg ServerConfig
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		slog.Error("Failed to parse config", "err", err)
		os.Exit(1)
	}

	server := NewTunnelServer(cfg)

	switch cfg.Protocol {
	case "tcp":
		go server.startTCP()
	case "udp":
		go server.startUDP()
	case "ws", "wsmux":
		go server.startWebSockets(false)
	case "wss", "wssmux":
		go server.startWebSockets(true)
	case "tcpmux":
		go server.startTCPMux()
	case "utcpmux":
		go server.startKCPMux()
	case "quic":
		go server.startQUIC()
	default:
		slog.Error("Unknown protocol configured", "protocol", cfg.Protocol)
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", ":"+cfg.ControlPort)
	if err != nil {
		slog.Error("Control port listen failed", "err", err)
		os.Exit(1)
	}

	slog.Info("Control listener ready", "port", cfg.ControlPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			setKeepAlive(conn)
			go server.handleControlConnection(conn)
		}
	}
}