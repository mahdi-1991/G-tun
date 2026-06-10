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
		if err != nil {
			return 0, err
		}
		c.r = r
	}
	n, err := c.r.Read(b)
	if err == io.EOF {
		c.r = nil
		err = nil
	}
	return n, err
}

func (c *wsConnWrapper) Write(b []byte) (int, error) {
	err := c.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

type quicSessionWrapper struct { quic.Connection }
func (q quicSessionWrapper) Close() error { 
	return q.Connection.CloseWithError(0, "closed by gtun client") 
}

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
	json.NewDecoder(file).Decode(&config)
}

func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

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

// ==== Standard Protocols ====

func startTcpDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil { return }
			defer rconn.Close()
			setKeepAlive(rconn)
			go relayConnections(rconn, lconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}

func startUdpDataForwarder(dataPort string) {
	localAddr, _ := net.ResolveUDPAddr("udp", config.LocalListenPort)
	localConn, err := net.ListenUDP("udp", localAddr)
	if err != nil { return }
	mu.Lock()
	activeListener = localConn
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	sessions := make(map[string]*net.UDPConn)
	var mapMutex sync.Mutex
	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := localConn.ReadFromUDP(buf)
		if err != nil { return }
		mapMutex.Lock()
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
						mapMutex.Lock()
						delete(sessions, cAddr.String())
						mapMutex.Unlock()
						rconn.Close()
						return
					}
					lconn.WriteToUDP((*remoteBufPtr)[:m], cAddr)
				}
			}(localConn, remoteConn, clientAddr)
		}
		mapMutex.Unlock()
		remoteConn.SetDeadline(time.Now().Add(3 * time.Minute))
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

func startWsDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/ws"}
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil { return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func startWssDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()

	localCertPEM, err := os.ReadFile("cert.pem")
	if err != nil { return }
	block, _ := pem.Decode(localCertPEM)
	if block == nil { return }
	expectedCertDER := block.Bytes

	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wss"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if !bytes.Equal(rawCerts[0], expectedCertDER) { return fmt.Errorf("MITM Attack") }
			return nil
		},
	}

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := dialer.Dial(u.String(), nil)
			if err != nil { return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

// ==== Multiplexed Protocols ====

func startTcpMuxDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()
			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				baseConn, err := net.Dial("tcp", config.RemoteServerIP+":"+dataPort)
				if err == nil {
					setKeepAlive(baseConn)
					newSess, err := smux.Client(baseConn, getSmuxConfig())
					if err == nil { sess = newSess }
				}
			}
			currentSess := sess
			sessMu.Unlock()

			if currentSess == nil { return }
			stream, err := currentSess.OpenStream()
			if err != nil { return }
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startWsMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wsmux"}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()
			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
				if err == nil {
					newSess, err := smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
					if err == nil { sess = newSess }
				}
			}
			currentSess := sess
			sessMu.Unlock()

			if currentSess == nil { return }
			stream, err := currentSess.OpenStream()
			if err != nil { return }
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startWssMuxDataForwarder(dataPort string) {
	localCertPEM, err := os.ReadFile("cert.pem")
	if err != nil { logInfo("Error reading cert.pem"); return }
	block, _ := pem.Decode(localCertPEM)
	if block == nil { return }
	expectedCertDER := block.Bytes

	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wssmux"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if !bytes.Equal(rawCerts[0], expectedCertDER) { return fmt.Errorf("MITM Attack") }
			return nil
		},
	}

	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort)

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()
			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				ws, _, err := dialer.Dial(u.String(), nil)
				if err == nil {
					newSess, err := smux.Client(&wsConnWrapper{Conn: ws}, getSmuxConfig())
					if err == nil { sess = newSess }
				}
			}
			currentSess := sess
			sessMu.Unlock()

			if currentSess == nil { return }
			stream, err := currentSess.OpenStream()
			if err != nil { return }
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startUtcpMuxDataForwarder(dataPort string) {
	kcpConf := config.KcpConfig
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort + " for traffic.")

	var sess *smux.Session
	var sessMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()
			sessMu.Lock()
			if sess == nil || sess.IsClosed() {
				baseConn, err := kcp.DialWithOptions(config.RemoteServerIP+":"+dataPort, nil, kcpConf.DataShards, kcpConf.ParityShards)
				if err == nil {
					baseConn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
					baseConn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
					newSess, err := smux.Client(baseConn, getSmuxConfig())
					if err == nil {
						sess = newSess
					} else {
						baseConn.Close()
					}
				}
			}
			currentSess := sess
			sessMu.Unlock()

			if currentSess == nil { return }
			stream, err := currentSess.OpenStream()
			if err != nil { return }
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

func startQuicDataForwarder(dataPort string) {
	localCertPEM, err := os.ReadFile("cert.pem")
	if err != nil { logInfo("Error reading cert.pem"); return }
	block, _ := pem.Decode(localCertPEM)
	if block == nil { return }
	expectedCertDER := block.Bytes

	tlsConf := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"gtun-quic"},
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			if !bytes.Equal(rawCerts[0], expectedCertDER) { return fmt.Errorf("MITM Attack") }
			return nil
		},
	}

	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { return }
	mu.Lock()
	activeListener = listener
	mu.Unlock()
	logInfo("Ready! Listening locally on " + config.LocalListenPort + " for QUIC traffic.")

	var qConn quic.Connection
	var connMu sync.Mutex

	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		setKeepAlive(localConn)

		go func(lconn net.Conn) {
			defer lconn.Close()
			connMu.Lock()
			if qConn == nil || qConn.Context().Err() != nil {
				newConn, err := quic.DialAddr(context.Background(), config.RemoteServerIP+":"+dataPort, tlsConf, &quic.Config{
					KeepAlivePeriod:    10 * time.Second,
					MaxIdleTimeout:     5 * time.Minute,
					MaxIncomingStreams: 10000,
				})
				if err == nil { qConn = newConn }
			}
			currentConn := qConn
			connMu.Unlock()

			if currentConn == nil { return }
			stream, err := currentConn.OpenStreamSync(context.Background())
			if err != nil { return }
			defer stream.Close()
			go relayConnections(stream, lconn)
			relayConnections(lconn, stream)
		}(localConn)
	}
}

// ---------------- Main & Control ----------------

func main() {
	loadClientConfiguration()

	for {
		logInfo("Connecting to control server...")
		conn, err := net.Dial("tcp", config.ControlServerAddress)
		if err != nil {
			time.Sleep(3 * time.Second)
			continue
		}
		
		setKeepAlive(conn) // روشن کردن KeepAlive برای جلوگیری از دراپ شدن در مسیر فایروال

		reader := bufio.NewReader(conn)
		
		// 1. دریافت چالش (که حالا متن Hex است)
		challengeStr, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			continue
		}
		challengeHex := strings.TrimSpace(challengeStr)

		// 2. محاسبه هش روی متن Hex
		mac := hmac.New(sha256.New, []byte(config.Token))
		mac.Write([]byte(challengeHex))
		responseHex := hex.EncodeToString(mac.Sum(nil))

		// 3. ارسال هش
		conn.Write([]byte(responseHex + "\n"))

		// 4. دریافت کانفیگ از سرور
		msgStr, err := reader.ReadString('\n')
		if err != nil {
			conn.Close()
			continue
		}

		var msg Message
		if err := json.Unmarshal([]byte(msgStr), &msg); err != nil {
			conn.Close()
			continue
		}

		if msg.Command == "start_transport" {
			stopTransport()

			var configData TransportConfig
			json.Unmarshal([]byte(msg.Payload), &configData)

			logInfo("Starting protocol: " + configData.Protocol)

			switch configData.Protocol {
			case "tcp": go startTcpDataForwarder(configData.Port)
			case "udp": go startUdpDataForwarder(configData.Port)
			case "ws": go startWsDataForwarder(configData.Port)
			case "tcpmux": go startTcpMuxDataForwarder(configData.Port)
			case "wsmux": go startWsMuxDataForwarder(configData.Port)
			case "wss": go startWssDataForwarder(configData.Port)
			case "wssmux": go startWssMuxDataForwarder(configData.Port)
			case "utcpmux": go startUtcpMuxDataForwarder(configData.Port)
			case "quic": go startQuicDataForwarder(configData.Port)
			}
		}

		reader.ReadString('\n') // صبر تا پایان اتصال
		conn.Close()
	}
}