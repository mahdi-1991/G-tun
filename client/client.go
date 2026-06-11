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
	"fmt"
	"io"
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

type ClientConfig struct {
	ControlServerAddress string    `json:"control_server_address"`
	LocalListenPort      string    `json:"local_listen_port"`
	RemoteServerIP       string    `json:"remote_server_ip"`
	DataPort             string    `json:"data_port"`
	Token                string    `json:"token"`
	KcpConfig            KcpConfig `json:"kcp_config"`
}

var (
	config         ClientConfig
	bufferPool     = sync.Pool{New: func() interface{} { b := make([]byte, 64*1024); return &b }}
	activeListener io.Closer
	mu             sync.Mutex
)

// ==================== Message Structs ====================

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
}

type TransportConfig struct {
	Protocol string `json:"protocol"`
	Port     string `json:"port"`
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

func loadClientConfiguration() {
	file, err := os.Open("client_config.json")
	if err != nil {
		fmt.Println("Error loading client_config.json:", err)
		os.Exit(1)
	}
	defer file.Close()
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		fmt.Println("Error parsing client_config.json:", err)
		os.Exit(1)
	}
}

func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

// stopTransport: listener فعلی رو می‌بنده تا transport قدیمی متوقف بشه
func stopTransport() {
	mu.Lock()
	defer mu.Unlock()
	if activeListener != nil {
		activeListener.Close()
		activeListener = nil
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

// loadPinnedCert: خواندن cert.pem و استخراج DER bytes برای certificate pinning
func loadPinnedCert() ([]byte, error) {
	localCertPEM, err := os.ReadFile("cert.pem")
	if err != nil {
		return nil, fmt.Errorf("error reading cert.pem: %w", err)
	}
	block, _ := pem.Decode(localCertPEM)
	if block == nil {
		return nil, fmt.Errorf("failed to decode cert.pem")
	}
	return block.Bytes, nil
}

// makePinnedTLSConfig: ساخت tls.Config با certificate pinning برای جلوگیری از MITM
func makePinnedTLSConfig(expectedCertDER []byte, nextProtos []string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         nextProtos,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 || !bytes.Equal(rawCerts[0], expectedCertDER) {
				return fmt.Errorf("certificate mismatch: possible MITM attack")
			}
			return nil
		},
	}
}

// ==================== Standard Protocols ====================

func startTcpDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("TCP local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil {
				logInfo("TCP dial error: " + err.Error())
				return
			}
			defer rconn.Close()
			setKeepAlive(rconn)
			go relayConnections(rconn, lconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}

// udpClientSession: session سمت کلاینت برای هر UDP peer
type udpClientSession struct {
	conn     *net.UDPConn
	lastSeen time.Time
}

// startUdpDataForwarder: UDP forwarding با idle session cleanup
// FIX: اضافه شدن goroutine پاک‌سازی برای جلوگیری از session leak
func startUdpDataForwarder(dataPort string) {
	localAddr, _ := net.ResolveUDPAddr("udp", config.LocalListenPort)
	localConn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		logInfo("UDP local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = localConn
	mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort

	sessions := make(map[string]*udpClientSession)
	var mapMutex sync.Mutex

	// goroutine پاک‌سازی session های idle
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
					logInfo("UDP client session expired for " + addr)
				}
			}
			mapMutex.Unlock()
		}
	}()

	buf := make([]byte, 4096)
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
				logInfo("UDP dial error: " + err.Error())
				mapMutex.Unlock()
				continue
			}
			sess = &udpClientSession{conn: remoteConn, lastSeen: time.Now()}
			sessions[clientAddr.String()] = sess

			go func(lconn *net.UDPConn, rconn *net.UDPConn, cAddr *net.UDPAddr, addrStr string) {
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
					lconn.WriteToUDP((*remoteBufPtr)[:m], cAddr)
				}
			}(localConn, remoteConn, clientAddr, clientAddr.String())
		} else {
			sess.lastSeen = time.Now()
		}
		currentConn := sess.conn
		mapMutex.Unlock()

		currentConn.SetDeadline(time.Now().Add(3 * time.Minute))
		currentConn.Write(buf[:n])
	}
}

// relayWs: relay داده بین یه TCP conn محلی و یه WebSocket conn
func relayWs(localConn net.Conn, wsConn *websocket.Conn) {
	errChan := make(chan error, 2)

	// local → WS
	go func() {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		for {
			n, err := localConn.Read(buf)
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

	// WS → local
	go func() {
		for {
			mt, message, err := wsConn.ReadMessage()
			if err != nil {
				errChan <- err
				return
			}
			if mt == websocket.BinaryMessage {
				if _, err := localConn.Write(message); err != nil {
					errChan <- err
					return
				}
			}
		}
	}()

	<-errChan
}

func startWsDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("WS local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/ws"}

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil {
				logInfo("WS dial error: " + err.Error())
				return
			}
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func startWssDataForwarder(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		logInfo("WSS cert error: " + err.Error())
		return
	}

	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("WSS local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()

	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wss"}
	dialer := &websocket.Dialer{
		TLSClientConfig: makePinnedTLSConfig(expectedCertDER, nil),
	}

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := dialer.Dial(u.String(), nil)
			if err != nil {
				logInfo("WSS dial error: " + err.Error())
				return
			}
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

// ==================== Multiplexed Protocols ====================

// newSmuxSession: ساخت یه smux session جدید روی TCP
// FIX: session قدیمی بسته می‌شه قبل از ساخت جدید تا resource leak نشه
func newSmuxSession(remoteAddr string, oldSess *smux.Session) (*smux.Session, error) {
	if oldSess != nil && !oldSess.IsClosed() {
		oldSess.Close()
	}
	baseConn, err := net.Dial("tcp", remoteAddr)
	if err != nil {
		return nil, err
	}
	setKeepAlive(baseConn)
	sess, err := smux.Client(baseConn, getSmuxConfig())
	if err != nil {
		baseConn.Close()
		return nil, err
	}
	return sess, nil
}

func startTcpMuxDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("TCPMux local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	remoteAddr := config.RemoteServerIP + ":" + dataPort
	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()

			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				newSess, err := newSmuxSession(remoteAddr, sess)
				if err != nil {
					logInfo("TCPMux session error: " + err.Error())
					sessMu.Unlock()
					return
				}
				sess = newSess
			}
			currentSess := sess
			sessMu.Unlock()

			stream, err := currentSess.OpenStream()
			if err != nil {
				// session احتمالاً مرده، دفعه بعد rebuild میشه
				sessMu.Lock()
				if sess == currentSess {
					sess = nil
				}
				sessMu.Unlock()
				return
			}
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startWsMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wsmux"}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("WSMux local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()

			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				// session قدیمی رو ببند
				if sess != nil {
					sess.Close()
				}
				ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
				if err != nil {
					logInfo("WSMux dial error: " + err.Error())
					sessMu.Unlock()
					return
				}
				newSess, err := smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
				if err != nil {
					ws.Close()
					sessMu.Unlock()
					return
				}
				sess = newSess
			}
			currentSess := sess
			sessMu.Unlock()

			stream, err := currentSess.OpenStream()
			if err != nil {
				sessMu.Lock()
				if sess == currentSess {
					sess = nil
				}
				sessMu.Unlock()
				return
			}
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startWssMuxDataForwarder(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		logInfo("WSSMux cert error: " + err.Error())
		return
	}

	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wssmux"}
	dialer := &websocket.Dialer{
		TLSClientConfig: makePinnedTLSConfig(expectedCertDER, nil),
	}

	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("WSSMux local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()

			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				if sess != nil {
					sess.Close()
				}
				ws, _, err := dialer.Dial(u.String(), nil)
				if err != nil {
					logInfo("WSSMux dial error: " + err.Error())
					sessMu.Unlock()
					return
				}
				newSess, err := smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
				if err != nil {
					ws.Close()
					sessMu.Unlock()
					return
				}
				sess = newSess
			}
			currentSess := sess
			sessMu.Unlock()

			stream, err := currentSess.OpenStream()
			if err != nil {
				sessMu.Lock()
				if sess == currentSess {
					sess = nil
				}
				sessMu.Unlock()
				return
			}
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startUtcpMuxDataForwarder(dataPort string) {
	kcpConf := config.KcpConfig
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("UTCPMux local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()

			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				if sess != nil {
					sess.Close()
				}
				baseConn, err := kcp.DialWithOptions(config.RemoteServerIP+":"+dataPort, nil, kcpConf.DataShards, kcpConf.ParityShards)
				if err != nil {
					logInfo("KCP dial error: " + err.Error())
					sessMu.Unlock()
					return
				}
				baseConn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
				baseConn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
				newSess, err := smux.Client(baseConn, getSmuxConfig())
				if err != nil {
					baseConn.Close()
					sessMu.Unlock()
					return
				}
				sess = newSess
			}
			currentSess := sess
			sessMu.Unlock()

			stream, err := currentSess.OpenStream()
			if err != nil {
				sessMu.Lock()
				if sess == currentSess {
					sess = nil
				}
				sessMu.Unlock()
				return
			}
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startQuicDataForwarder(dataPort string) {
	expectedCertDER, err := loadPinnedCert()
	if err != nil {
		logInfo("QUIC cert error: " + err.Error())
		return
	}

	tlsConf := makePinnedTLSConfig(expectedCertDER, []string{"gtun-quic"})

	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		logInfo("QUIC local listen error: " + err.Error())
		return
	}
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var qConn quic.Connection
	var connMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()

			connMu.Lock()
			if qConn == nil || qConn.Context().Err() != nil {
				// کانکشن QUIC قدیمی رو ببند
				if qConn != nil {
					qConn.CloseWithError(0, "reconnecting")
				}
				newConn, err := quic.DialAddr(
					context.Background(),
					config.RemoteServerIP+":"+dataPort,
					tlsConf,
					&quic.Config{
						KeepAlivePeriod:    10 * time.Second,
						MaxIdleTimeout:     5 * time.Minute,
						MaxIncomingStreams: 10000,
					},
				)
				if err != nil {
					logInfo("QUIC dial error: " + err.Error())
					connMu.Unlock()
					return
				}
				qConn = newConn
			}
			currentConn := qConn
			connMu.Unlock()

			stream, err := currentConn.OpenStreamSync(context.Background())
			if err != nil {
				// کانکشن مرده، reset کن
				connMu.Lock()
				if qConn == currentConn {
					qConn = nil
				}
				connMu.Unlock()
				return
			}
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

// ==================== Main & Control ====================

// connectControl: اتصال به سرور کنترل و احراز هویت
// FIX: Exponential backoff برای جلوگیری از flood کردن سرور در صورت قطعی
func main() {
	loadClientConfiguration()

	// backoff: شروع از 3 ثانیه، حداکثر 60 ثانیه
	backoff := 3 * time.Second
	const maxBackoff = 60 * time.Second

	for {
		logInfo("Connecting to control server " + config.ControlServerAddress + "...")
		conn, err := net.DialTimeout("tcp", config.ControlServerAddress, 10*time.Second)
		if err != nil {
			logInfo(fmt.Sprintf("Connection failed: %s — retrying in %v", err.Error(), backoff))
			time.Sleep(backoff)
			// دو برابر کردن backoff تا سقف maxBackoff
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		// اتصال موفق — backoff رو ریست کن
		backoff = 3 * time.Second

		setKeepAlive(conn)

		// deadline برای فاز احراز هویت
		conn.SetDeadline(time.Now().Add(15 * time.Second))

		reader := bufio.NewReader(conn)

		// 1. دریافت چالش Hex از سرور
		challengeStr, err := reader.ReadString('\n')
		if err != nil {
			logInfo("Error reading challenge: " + err.Error())
			conn.Close()
			continue
		}
		challengeHex := strings.TrimSpace(challengeStr)

		// 2. محاسبه HMAC-SHA256 روی چالش
		mac := hmac.New(sha256.New, []byte(config.Token))
		mac.Write([]byte(challengeHex))
		responseHex := hex.EncodeToString(mac.Sum(nil))

		// 3. ارسال پاسخ
		if _, err := conn.Write([]byte(responseHex + "\n")); err != nil {
			logInfo("Error sending auth response: " + err.Error())
			conn.Close()
			continue
		}

		// 4. دریافت دستور از سرور
		msgStr, err := reader.ReadString('\n')
		if err != nil {
			logInfo("Error reading server command: " + err.Error())
			conn.Close()
			continue
		}

		// deadline رو پاک کن — اتصال باقی می‌مونه
		conn.SetDeadline(time.Time{})

		var msg Message
		if err := json.Unmarshal([]byte(strings.TrimSpace(msgStr)), &msg); err != nil {
			logInfo("Error parsing server message: " + err.Error())
			conn.Close()
			continue
		}

		if msg.Command == "start_transport" {
			stopTransport()

			var transportCfg TransportConfig
			if err := json.Unmarshal([]byte(msg.Payload), &transportCfg); err != nil {
				logInfo("Error parsing transport config: " + err.Error())
				conn.Close()
				continue
			}

			logInfo("Starting protocol: " + transportCfg.Protocol + " on port " + transportCfg.Port)

			switch transportCfg.Protocol {
			case "tcp":
				go startTcpDataForwarder(transportCfg.Port)
			case "udp":
				go startUdpDataForwarder(transportCfg.Port)
			case "ws":
				go startWsDataForwarder(transportCfg.Port)
			case "tcpmux":
				go startTcpMuxDataForwarder(transportCfg.Port)
			case "wsmux":
				go startWsMuxDataForwarder(transportCfg.Port)
			case "wss":
				go startWssDataForwarder(transportCfg.Port)
			case "wssmux":
				go startWssMuxDataForwarder(transportCfg.Port)
			case "utcpmux":
				go startUtcpMuxDataForwarder(transportCfg.Port)
			case "quic":
				go startQuicDataForwarder(transportCfg.Port)
			default:
				logInfo("Unknown protocol received: " + transportCfg.Protocol)
			}
		}

		// صبر تا قطع شدن کانکشن کنترل
		reader.ReadString('\n')
		conn.Close()
		logInfo("Control connection lost — reconnecting...")
	}
}
