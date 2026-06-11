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

// ==================== Config Structs ====================

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

var config ServerConfig

// bufferPool: استفاده مجدد از buffer برای کاهش فشار روی GC
var bufferPool = sync.Pool{
	New: func() interface{} { b := make([]byte, 64*1024); return &b },
}

// ==================== Message ====================

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
}

// ==================== WebSocket Wrapper ====================

// wsConnWrapper: تبدیل websocket.Conn به net.Conn-like برای استفاده با smux
type wsConnWrapper struct {
	*websocket.Conn
	r io.Reader
}

func (c *wsConnWrapper) Read(b []byte) (int, error) {
	for {
		if c.r != nil {
			n, err := c.r.Read(b)
			if err == io.EOF {
				// فریم فعلی تموم شد، فریم بعدی رو بگیر
				c.r = nil
				continue
			}
			return n, err
		}
		_, r, err := c.NextReader()
		if err != nil {
			return 0, err
		}
		c.r = r
	}
}

func (c *wsConnWrapper) Write(b []byte) (int, error) {
	err := c.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// ==================== Helpers ====================

func logInfo(message string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
}

func loadServerConfiguration() {
	file, err := os.Open("server_config.json")
	if err != nil {
		fmt.Println("Error loading server_config.json:", err)
		os.Exit(1)
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		fmt.Println("Error parsing server_config.json:", err)
		os.Exit(1)
	}
}

func getSmuxConfig() *smux.Config {
	smuxConfig := smux.DefaultConfig()
	smuxConfig.KeepAliveInterval = 10 * time.Second
	smuxConfig.KeepAliveTimeout = 30 * time.Second
	smuxConfig.MaxFrameSize = 32 * 1024
	smuxConfig.MaxReceiveBuffer = 4194304
	smuxConfig.MaxStreamBuffer = 65536
	return smuxConfig
}

func setKeepAlive(conn net.Conn) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(15 * time.Second)
	}
}

// relayConnections: کپی داده از src به dst با buffer pool
func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

// ==================== TCP ====================

func handleTcpDataConnection(clientConn net.Conn) {
	defer clientConn.Close()
	setKeepAlive(clientConn)

	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil {
		logInfo("Failed to dial Xray: " + err.Error())
		return
	}
	defer xrayConn.Close()
	setKeepAlive(xrayConn)

	go relayConnections(xrayConn, clientConn)
	relayConnections(clientConn, xrayConn)
}

func startTcpDataListener() {
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.DataPort)
	if err != nil {
		logInfo("TCP Listen Error: " + err.Error())
		return
	}
	logInfo("TCP Listener started on port " + config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			go handleTcpDataConnection(conn)
		}
	}
}

// ==================== UDP ====================

// udpSession: یه session UDP با زمان آخرین فعالیت برای تشخیص idle
type udpSession struct {
	conn     net.Conn
	lastSeen time.Time
}

// startUdpDataListener: هر کلاینت UDP یه اتصال TCP به Xray می‌گیره.
// FIX: اضافه شدن idle timeout برای جلوگیری از session leak.
func startUdpDataListener() {
	udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:"+config.DataPort)
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		logInfo("UDP Listen Error: " + err.Error())
		return
	}
	logInfo("UDP Listener started on port " + config.DataPort)

	sessions := make(map[string]*udpSession)
	var mapMutex sync.Mutex

	// goroutine پاک‌سازی session های idle (هر ۱ دقیقه یه‌بار)
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			now := time.Now()
			mapMutex.Lock()
			for addr, sess := range sessions {
				if now.Sub(sess.lastSeen) > 3*time.Minute {
					sess.conn.Close()
					delete(sessions, addr)
					logInfo("UDP session expired for " + addr)
				}
			}
			mapMutex.Unlock()
		}
	}()

	buf := make([]byte, 4096)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}

		mapMutex.Lock()
		sess, ok := sessions[remoteAddr.String()]
		if !ok {
			xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
			if err != nil {
				logInfo("Failed to dial Xray: " + err.Error())
				mapMutex.Unlock()
				continue
			}
			sess = &udpSession{conn: xrayConn, lastSeen: time.Now()}
			sessions[remoteAddr.String()] = sess

			go func(udpConn *net.UDPConn, clientAddr *net.UDPAddr, tcpConn net.Conn, addrStr string) {
				tcpBufPtr := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(tcpBufPtr)
				for {
					m, err := tcpConn.Read(*tcpBufPtr)
					if err != nil {
						mapMutex.Lock()
						delete(sessions, addrStr)
						mapMutex.Unlock()
						tcpConn.Close()
						return
					}
					udpConn.WriteToUDP((*tcpBufPtr)[:m], clientAddr)
				}
			}(conn, remoteAddr, xrayConn, remoteAddr.String())
		} else {
			sess.lastSeen = time.Now()
		}
		currentConn := sess.conn
		mapMutex.Unlock()

		currentConn.SetDeadline(time.Now().Add(3 * time.Minute))
		currentConn.Write(buf[:n])
	}
}

// ==================== Mux Stream Handler ====================

func handleMuxStream(stream io.ReadWriteCloser) {
	defer stream.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil {
		logInfo("Failed to dial Xray: " + err.Error())
		return
	}
	defer xrayConn.Close()
	setKeepAlive(xrayConn)

	go relayConnections(xrayConn, stream)
	relayConnections(stream, xrayConn)
}

// ==================== QUIC ====================

func startQuicDataListener() {
	cert, err := tls.LoadX509KeyPair(config.TlsCertPath, config.TlsKeyPath)
	if err != nil {
		logInfo("QUIC TLS Load Error: " + err.Error())
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

	listener, err := quic.ListenAddr("0.0.0.0:"+config.DataPort, tlsConf, quicConfig)
	if err != nil {
		logInfo("QUIC Listen Error: " + err.Error())
		return
	}
	logInfo("QUIC (HTTP/3) Listener started on port " + config.DataPort)

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
				go handleMuxStream(stream)
			}
		}(conn)
	}
}

// ==================== WebSocket ====================

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func handleWsDataConnection(wsConn *websocket.Conn) {
	defer wsConn.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil {
		logInfo("Failed to dial Xray: " + err.Error())
		return
	}
	defer xrayConn.Close()
	setKeepAlive(xrayConn)

	errChan := make(chan error, 2)

	// WS → Xray
	go func() {
		for {
			mt, message, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			if mt == websocket.BinaryMessage {
				if _, err := xrayConn.Write(message); err != nil {
					errChan <- err
					return
				}
			}
		}
	}()

	// Xray → WS
	go func() {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		for {
			n, err := xrayConn.Read(buf)
			if err != nil {
				errChan <- err
				return
			}
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				errChan <- err
				return
			}
		}
	}()

	<-errChan
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err == nil {
		handleWsDataConnection(conn)
	}
}

func startWsDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WS Listener started on port " + config.DataPort)
	server.ListenAndServe()
}

// ==================== TCPMux ====================

func startTcpMuxDataListener() {
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.DataPort)
	if err != nil {
		logInfo("TCPMux Listen Error: " + err.Error())
		return
	}
	logInfo("TCPMux Listener started on port " + config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			setKeepAlive(conn)
			go func(c net.Conn) {
				session, err := smux.Server(c, getSmuxConfig())
				if err != nil {
					logInfo("Smux Error: " + err.Error())
					c.Close()
					return
				}
				defer session.Close()
				for {
					stream, err := session.AcceptStream()
					if err != nil {
						break
					}
					go handleMuxStream(stream)
				}
			}(conn)
		}
	}
}

// ==================== WSMux ====================

func wsmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
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
		go handleMuxStream(stream)
	}
}

func startWsMuxDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/wsmux", wsmuxHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WSMux Listener started on port " + config.DataPort)
	server.ListenAndServe()
}

// ==================== WSS ====================

func startWssDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/wss", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WSS Listener started on port " + config.DataPort)
	server.ListenAndServeTLS(config.TlsCertPath, config.TlsKeyPath)
}

// ==================== WSSMux ====================

func wssmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
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
		go handleMuxStream(stream)
	}
}

func startWssMuxDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/wssmux", wssmuxHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WSSMux Listener started on port " + config.DataPort)
	server.ListenAndServeTLS(config.TlsCertPath, config.TlsKeyPath)
}

// ==================== UTCPMux (KCP) ====================

func startUtcpMuxDataListener() {
	kcpConf := config.KcpConfig
	listener, err := kcp.ListenWithOptions("0.0.0.0:"+config.DataPort, nil, kcpConf.DataShards, kcpConf.ParityShards)
	if err != nil {
		logInfo("KCP Listen Error: " + err.Error())
		return
	}
	logInfo("UTCPMux (KCP) Listener started on port " + config.DataPort)
	for {
		conn, err := listener.AcceptKCP()
		if err == nil {
			conn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
			conn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
			go func(c net.Conn) {
				session, err := smux.Server(c, getSmuxConfig())
				if err != nil {
					logInfo("Smux Server Error: " + err.Error())
					c.Close()
					return
				}
				defer session.Close()
				for {
					stream, err := session.AcceptStream()
					if err != nil {
						break
					}
					go handleMuxStream(stream)
				}
			}(conn)
		}
	}
}

// ==================== Control Channel ====================

// handleControlConnection: احراز هویت کلاینت با HMAC-SHA256 Challenge/Response
// FIX: اضافه شدن deadline برای جلوگیری از goroutine leak در صورت hang شدن کلاینت
func handleControlConnection(conn net.Conn) {
	defer conn.Close()

	// تنظیم deadline کلی برای فاز احراز هویت (10 ثانیه)
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	// 1. تولید چالش تصادفی و تبدیل به Hex
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		logInfo("Failed to generate challenge: " + err.Error())
		return
	}
	challengeHex := hex.EncodeToString(challenge)

	// 2. ارسال چالش به کلاینت
	if _, err := conn.Write([]byte(challengeHex + "\n")); err != nil {
		return
	}

	// 3. دریافت پاسخ از کلاینت
	reader := bufio.NewReader(conn)
	clientResponseHex, err := reader.ReadString('\n')
	if err != nil {
		logInfo("Error reading client response: " + err.Error())
		return
	}
	clientResponseHex = strings.TrimSpace(clientResponseHex)

	// 4. محاسبه HMAC روی چالش Hex
	mac := hmac.New(sha256.New, []byte(config.Token))
	mac.Write([]byte(challengeHex))
	expectedResponseHex := hex.EncodeToString(mac.Sum(nil))

	// 5. مقایسه constant-time برای جلوگیری از timing attack
	if subtle.ConstantTimeCompare([]byte(clientResponseHex), []byte(expectedResponseHex)) != 1 {
		logInfo("Unauthorized access attempt from " + conn.RemoteAddr().String())
		return
	}

	logInfo("Client authenticated: " + conn.RemoteAddr().String())

	// احراز هویت موفق — deadline رو پاک می‌کنیم
	conn.SetDeadline(time.Time{})

	// 6. ارسال تنظیمات پروتکل به کلاینت
	payload := fmt.Sprintf(`{"protocol":"%s","port":"%s"}`, config.Protocol, config.DataPort)
	msg := Message{Command: "start_transport", Payload: payload}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		logInfo("Error sending config to client: " + err.Error())
		return
	}

	// 7. صبر تا قطع شدن کلاینت
	reader.ReadString('\n')
	logInfo("Client disconnected: " + conn.RemoteAddr().String())
}

// ==================== Main ====================

func main() {
	loadServerConfiguration()
	logInfo("Server starting — control port: " + config.ControlPort + ", data port: " + config.DataPort + ", protocol: " + config.Protocol)

	switch config.Protocol {
	case "tcp":
		go startTcpDataListener()
	case "udp":
		go startUdpDataListener()
	case "ws":
		go startWsDataListener()
	case "tcpmux":
		go startTcpMuxDataListener()
	case "wsmux":
		go startWsMuxDataListener()
	case "wss":
		go startWssDataListener()
	case "wssmux":
		go startWssMuxDataListener()
	case "utcpmux":
		go startUtcpMuxDataListener()
	case "quic":
		go startQuicDataListener()
	default:
		logInfo("Unknown protocol: " + config.Protocol)
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "0.0.0.0:"+config.ControlPort)
	if err != nil {
		logInfo("Failed to start control port: " + err.Error())
		os.Exit(1)
	}

	logInfo("Control listener ready on port " + config.ControlPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			setKeepAlive(conn)
			go handleControlConnection(conn)
		}
	}
}
