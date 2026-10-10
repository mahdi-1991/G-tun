package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
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

type ClientConfig struct {
	ControlServerAddress string    `json:"control_server_address"`
	LocalListenPort      string    `json:"local_listen_port"`
	RemoteServerIP       string    `json:"remote_server_ip"`
	DataPort             string    `json:"data_port"`
	Token                string    `json:"token"`
	KcpConfig            KcpConfig `json:"kcp_config"`
}

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
}

type TransportConfig struct {
	Protocol string `json:"protocol"`
	Port     string `json:"port"`
}

// ==================== Utils & Wrappers ====================

var bufferPool = sync.Pool{
	New: func() any { b := make([]byte, 64*1024); return &b },
}

// relayConnections: شامل سیستم Sniffing و چاپ Hex Dump
func relayConnections(conn1, conn2 io.ReadWriteCloser) {
	defer conn1.Close()
	defer conn2.Close()
	errChan := make(chan error, 2)

	copyFunc := func(dst io.Writer, src io.Reader, direction string) {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr

		for {
			n, err := src.Read(buf)
			if n > 0 {
				// fmt.Printf("\n[DEBUG] %s | %d bytes\n", direction, n)
				// fmt.Print(hex.Dump(buf[:n]))

				_, werr := dst.Write(buf[:n])
				if werr != nil {
					errChan <- werr
					return
				}
			}
			if err != nil {
				errChan <- err
				return
			}
		}
	}

	go copyFunc(conn2, conn1, "Local App ➔ Tunnel")
	go copyFunc(conn1, conn2, "Tunnel ➔ Local App")

	<-errChan
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

func loadPinnedCert() ([]byte, error) {
	localCertPEM, err := os.ReadFile("cert.pem")
	if err != nil {
		return nil, fmt.Errorf("reading cert.pem: %w", err)
	}
	block, _ := pem.Decode(localCertPEM)
	if block == nil {
		return nil, errors.New("failed to decode cert.pem")
	}
	return block.Bytes, nil
}

func makePinnedTLSConfig(expectedCertDER []byte, nextProtos []string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         nextProtos,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || !bytes.Equal(rawCerts[0], expectedCertDER) {
				return errors.New("certificate mismatch: possible MITM attack")
			}
			return nil
		},
	}
}

// ==================== Client Core ====================

type TunnelClient struct {
	config         ClientConfig
	activeListener io.Closer
	mu             sync.Mutex
}

func NewTunnelClient(cfg ClientConfig) *TunnelClient {
	return &TunnelClient{config: cfg}
}

func (c *TunnelClient) stopTransport() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeListener != nil {
		c.activeListener.Close()
		c.activeListener = nil
	}
}

func (c *TunnelClient) setListener(l io.Closer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeListener = l
}

// ==================== CONNECTION TESTER ====================
// این متد برای تست اتوماتیک تانل پس از برقراری ارتباط فراخوانی می‌شود
func (c *TunnelClient) runConnectionTest(proto string) {
	// صبر می‌کنیم تا لیسنر کلاینت (TCP/UDP) کاملاً آماده شود
	time.Sleep(1 * time.Second)

	slog.Info("==================================================")
	slog.Info(fmt.Sprintf(" STARTING AUTOMATED TUNNEL TEST VIA [%s]", strings.ToUpper(proto)))
	slog.Info("==================================================")

	network := "tcp"
	if proto == "udp" {
		network = "udp"
	}

	// تبدیل 0.0.0.0 به 127.0.0.1 برای لوکال هاست
	host, port, err := net.SplitHostPort(c.config.LocalListenPort)
	if err != nil || host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	localAddr := net.JoinHostPort(host, port)

	var conn net.Conn
	if network == "tcp" {
		conn, err = net.DialTimeout("tcp", localAddr, 3*time.Second)
	} else {
		udpAddr, _ := net.ResolveUDPAddr("udp", localAddr)
		conn, err = net.DialUDP("udp", nil, udpAddr)
	}

	if err != nil {
		slog.Error("Tunnel Test Failed: Cannot connect to local listener", "err", err)
		return
	}
	defer conn.Close()

	// ارسال ۳ بسته تستی
	for i := 1; i <= 3; i++ {
		packetData := []byte(fmt.Sprintf("=== [G-TUN TEST PACKET %d] VIA %s ===", i, strings.ToUpper(proto)))
		_, err := conn.Write(packetData)
		if err != nil {
			// اگر سرور/Xray اتصال را قطع کرد
			slog.Warn("Test connection closed by remote (Expected if Xray rejects text data)", "packet", i)
			break
		}
		slog.Info("Tunnel Test Packet Sent Successfully", "packet", i)
		time.Sleep(300 * time.Millisecond)
	}

	slog.Info("==================================================")
	slog.Info(" AUTOMATED TUNNEL TEST COMPLETED ")
	slog.Info("==================================================")
}

func (c *TunnelClient) startForwarder(proto, port string) {
	c.stopTransport()
	slog.Info("Starting protocol", "protocol", proto, "port", port)

	// اجرای تست اتصال در پس‌زمینه (همزمان با راه‌اندازی پروتکل)
	// go c.runConnectionTest(proto)

	switch proto {
	case "tcp":
		go c.forwardTCP(port)
	case "udp":
		go c.forwardUDP(port)
	case "ws":
		go c.forwardWS(port)
	case "wss":
		go c.forwardWSS(port)
	case "tcpmux":
		go c.forwardTCPMux(port)
	case "wsmux":
		go c.forwardWSMux(port)
	case "wssmux":
		go c.forwardWSSMux(port)
	case "utcpmux":
		go c.forwardKCPMux(port)
	case "quic":
		go c.forwardQUIC(port)
	default:
		slog.Warn("Unknown protocol received", "protocol", proto)
	}
}

// ==================== Protocol Implementations ====================

func (c *TunnelClient) forwardTCP(dataPort string) {
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("TCP local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally", "addr", c.config.LocalListenPort)

	remoteDataAddr := net.JoinHostPort(c.config.RemoteServerIP, dataPort)
	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil {
				slog.Error("TCP dial error", "err", err)
				lconn.Close()
				return
			}
			setKeepAlive(rconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}

func (c *TunnelClient) forwardUDP(dataPort string) {
	localAddr, err := net.ResolveUDPAddr("udp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("Resolve UDP error", "err", err)
		return
	}
	localConn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		slog.Error("UDP local listen error", "err", err)
		return
	}
	c.setListener(localConn)
	slog.Info("Listening locally (UDP)", "addr", c.config.LocalListenPort)

	remoteDataAddr := net.JoinHostPort(c.config.RemoteServerIP, dataPort)
	type udpSession struct {
		conn     *net.UDPConn
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
			for addr, sess := range sessions {
				if now.Sub(sess.lastSeen) > UDPIdleTimeout {
					sess.conn.Close()
					delete(sessions, addr)
					slog.Debug("UDP session expired", "client", addr)
				}
			}
			mapMutex.Unlock()
		}
	}()

	buf := make([]byte, MaxUDPSize)
	for {
		n, clientAddr, err := localConn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		mapMutex.Lock()
		sess, ok := sessions[clientAddr.String()]
		if !ok {
			udpServerAddr, _ := net.ResolveUDPAddr("udp", remoteDataAddr)
			remoteConn, err := net.DialUDP("udp", nil, udpServerAddr)
			if err != nil {
				slog.Error("UDP dial error", "err", err)
				mapMutex.Unlock()
				continue
			}
			sess = &udpSession{conn: remoteConn, lastSeen: time.Now()}
			sessions[clientAddr.String()] = sess

			go func(rconn *net.UDPConn, cAddr *net.UDPAddr, addrStr string) {
				remoteBufPtr := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(remoteBufPtr)
				for {
					m, err := rconn.Read(*remoteBufPtr)
					if err != nil {
						mapMutex.Lock()
						delete(sessions, addrStr)
						mapMutex.Unlock()
						rconn.Close()
						return
					}

					fmt.Printf("\n[DEBUG] Tunnel ➔ Local App | %d bytes\n", m)
					fmt.Print(hex.Dump((*remoteBufPtr)[:m]))

					localConn.WriteToUDP((*remoteBufPtr)[:m], cAddr)
				}
			}(remoteConn, clientAddr, clientAddr.String())
		} else {
			sess.lastSeen = time.Now()
		}
		currentConn := sess.conn
		mapMutex.Unlock()

		currentConn.SetDeadline(time.Now().Add(UDPIdleTimeout))

		fmt.Printf("\n[DEBUG] Local App ➔ Tunnel | %d bytes\n", n)
		fmt.Print(hex.Dump(buf[:n]))

		currentConn.Write(buf[:n])
	}
}

func (c *TunnelClient) forwardWS(dataPort string) {
	c.serveWebSocket("ws", dataPort, nil)
}

func (c *TunnelClient) forwardWSS(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		slog.Error("WSS cert error", "err", err)
		return
	}
	dialer := &websocket.Dialer{
		TLSClientConfig: makePinnedTLSConfig(expectedCertDER, nil),
	}
	c.serveWebSocket("wss", dataPort, dialer)
}

func (c *TunnelClient) serveWebSocket(scheme, port string, dialer *websocket.Dialer) {
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("WS local listen error", "err", err)
		return
	}
	c.setListener(listener)
	u := url.URL{Scheme: scheme, Host: net.JoinHostPort(c.config.RemoteServerIP, port), Path: "/" + scheme}
	slog.Info("Listening locally", "addr", c.config.LocalListenPort)

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			wsConn, _, err := dialer.Dial(u.String(), nil)
			if err != nil {
				slog.Error("WS dial error", "err", err)
				lconn.Close()
				return
			}
			relayConnections(lconn, &wsConnWrapper{Conn: wsConn})
		}(localConn)
	}
}

func (c *TunnelClient) handleMux(listener net.Listener, dialFunc func() (*smux.Session, error)) {
	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				if sess != nil {
					sess.Close()
				}
				newSess, err := dialFunc()
				if err != nil {
					slog.Error("Mux session error", "err", err)
					lconn.Close()
					sessMu.Unlock()
					return
				}
				sess = newSess
			}
			currentSess := sess
			sessMu.Unlock()

			stream, err := currentSess.OpenStream()
			if err != nil {
				lconn.Close()
				sessMu.Lock()
				if sess == currentSess {
					sess = nil
				}
				sessMu.Unlock()
				return
			}
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func (c *TunnelClient) forwardTCPMux(dataPort string) {
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("TCPMux local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally (TCPMux)", "addr", c.config.LocalListenPort)

	remoteAddr := net.JoinHostPort(c.config.RemoteServerIP, dataPort)
	c.handleMux(listener, func() (*smux.Session, error) {
		baseConn, err := net.Dial("tcp", remoteAddr)
		if err != nil {
			return nil, err
		}
		setKeepAlive(baseConn)
		return smux.Client(baseConn, getSmuxConfig())
	})
}

func (c *TunnelClient) forwardWSMux(dataPort string) {
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("WSMux local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally (WSMux)", "addr", c.config.LocalListenPort)

	u := url.URL{Scheme: "ws", Host: net.JoinHostPort(c.config.RemoteServerIP, dataPort), Path: "/wsmux"}
	c.handleMux(listener, func() (*smux.Session, error) {
		ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
		if err != nil {
			return nil, err
		}
		return smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
	})
}

func (c *TunnelClient) forwardWSSMux(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		slog.Error("WSSMux cert error", "err", err)
		return
	}
	dialer := &websocket.Dialer{TLSClientConfig: makePinnedTLSConfig(expectedCertDER, nil)}
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("WSSMux local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally (WSSMux)", "addr", c.config.LocalListenPort)

	u := url.URL{Scheme: "wss", Host: net.JoinHostPort(c.config.RemoteServerIP, dataPort), Path: "/wssmux"}
	c.handleMux(listener, func() (*smux.Session, error) {
		ws, _, err := dialer.Dial(u.String(), nil)
		if err != nil {
			return nil, err
		}
		return smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
	})
}

func (c *TunnelClient) forwardKCPMux(dataPort string) {
	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("KCPMux local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally (KCPMux)", "addr", c.config.LocalListenPort)

	remoteAddr := net.JoinHostPort(c.config.RemoteServerIP, dataPort)
	kc := c.config.KcpConfig
	c.handleMux(listener, func() (*smux.Session, error) {
		baseConn, err := kcp.DialWithOptions(remoteAddr, nil, kc.DataShards, kc.ParityShards)
		if err != nil {
			return nil, err
		}
		baseConn.SetNoDelay(kc.NoDelay, kc.Interval, kc.Resend, kc.NoCongestion)
		baseConn.SetWindowSize(kc.SndWnd, kc.RcvWnd)
		return smux.Client(baseConn, getSmuxConfig())
	})
}

func (c *TunnelClient) forwardQUIC(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		slog.Error("QUIC cert error", "err", err)
		return
	}
	tlsConf := makePinnedTLSConfig(expectedCertDER, []string{"gtun-quic"})

	listener, err := net.Listen("tcp", c.config.LocalListenPort)
	if err != nil {
		slog.Error("QUIC local listen error", "err", err)
		return
	}
	c.setListener(listener)
	slog.Info("Listening locally (QUIC)", "addr", c.config.LocalListenPort)

	var qConn quic.Connection
	var connMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			connMu.Lock()
			if qConn == nil || qConn.Context().Err() != nil {
				if qConn != nil {
					qConn.CloseWithError(0, "reconnecting")
				}
				newConn, err := quic.DialAddr(
					context.Background(),
					net.JoinHostPort(c.config.RemoteServerIP, dataPort),
					tlsConf,
					&quic.Config{
						KeepAlivePeriod:    10 * time.Second,
						MaxIdleTimeout:     5 * time.Minute,
						MaxIncomingStreams: 10000,
					},
				)
				if err != nil {
					slog.Error("QUIC dial error", "err", err)
					lconn.Close()
					connMu.Unlock()
					return
				}
				qConn = newConn
			}
			currentConn := qConn
			connMu.Unlock()

			stream, err := currentConn.OpenStreamSync(context.Background())
			if err != nil {
				lconn.Close()
				connMu.Lock()
				if qConn == currentConn {
					qConn = nil
				}
				connMu.Unlock()
				return
			}
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func (c *TunnelClient) Run() {
	backoff := 3 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		slog.Info("Connecting to control server", "address", c.config.ControlServerAddress)
		conn, err := net.DialTimeout("tcp", c.config.ControlServerAddress, 10*time.Second)
		if err != nil {
			slog.Warn("Connection failed, retrying...", "err", err, "wait", backoff)
			time.Sleep(backoff)
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = 3 * time.Second
		setKeepAlive(conn)
		conn.SetDeadline(time.Now().Add(15 * time.Second))

		reader := bufio.NewReader(conn)
		challengeStr, err := reader.ReadString('\n')
		if err != nil {
			slog.Error("Error reading challenge", "err", err)
			conn.Close()
			continue
		}

		mac := hmac.New(sha256.New, []byte(c.config.Token))
		mac.Write([]byte(strings.TrimSpace(challengeStr)))
		responseHex := hex.EncodeToString(mac.Sum(nil))

		if _, err := conn.Write([]byte(responseHex + "\n")); err != nil {
			slog.Error("Error sending auth response", "err", err)
			conn.Close()
			continue
		}

		msgStr, err := reader.ReadString('\n')
		if err != nil {
			slog.Error("Error reading server command", "err", err)
			conn.Close()
			continue
		}

		conn.SetDeadline(time.Time{})

		var msg Message
		if err := json.Unmarshal([]byte(strings.TrimSpace(msgStr)), &msg); err != nil {
			slog.Error("Error parsing server message", "err", err)
			conn.Close()
			continue
		}

		if msg.Command == "start_transport" {
			var tCfg TransportConfig
			if err := json.Unmarshal([]byte(msg.Payload), &tCfg); err == nil {
				c.startForwarder(tCfg.Protocol, tCfg.Port)
			}
		}

		reader.ReadString('\n')
		conn.Close()
		slog.Warn("Control connection lost, reconnecting...")
	}
}

func main() {
	file, err := os.Open("client_config.json")
	if err != nil {
		slog.Error("Failed to open client_config.json", "err", err)
		os.Exit(1)
	}
	defer file.Close()

	var cfg ClientConfig
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		slog.Error("Failed to parse config", "err", err)
		os.Exit(1)
	}

	client := NewTunnelClient(cfg)
	client.Run()
}
