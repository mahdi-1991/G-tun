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
var bufferPool = sync.Pool{
	New: func() interface{} { b := make([]byte, 64*1024); return &b },
}

type Message struct {
	Command string `json:"command"`
	Payload string `json:"payload"`
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
	json.NewDecoder(file).Decode(&config)
}

// ---------------- Helper Functions ----------------

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

// ---------------- Data Transport Functions ----------------

func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

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

func startUdpDataListener() {
	udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:"+config.DataPort)
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		logInfo("UDP Listen Error: " + err.Error())
		return
	}
	logInfo("UDP Listener started on port " + config.DataPort)
	sessions := make(map[string]net.Conn)
	var mapMutex sync.Mutex
	buf := make([]byte, 4096)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		mapMutex.Lock()
		xrayConn, ok := sessions[remoteAddr.String()]
		if !ok {
			xrayConn, err = net.Dial("tcp", config.XrayInboundAddress)
			if err != nil {
				logInfo("Failed to dial Xray: " + err.Error())
				mapMutex.Unlock()
				continue
			}
			sessions[remoteAddr.String()] = xrayConn
			go func(udpConn *net.UDPConn, clientAddr *net.UDPAddr, tcpConn net.Conn) {
				tcpBufPtr := bufferPool.Get().(*[]byte)
				defer bufferPool.Put(tcpBufPtr)
				for {
					m, err := tcpConn.Read(*tcpBufPtr)
					if err != nil {
						mapMutex.Lock()
						delete(sessions, clientAddr.String())
						mapMutex.Unlock()
						tcpConn.Close()
						return
					}
					udpConn.WriteToUDP((*tcpBufPtr)[:m], clientAddr)
				}
			}(conn, remoteAddr, xrayConn)
		}
		mapMutex.Unlock()

		xrayConn.SetDeadline(time.Now().Add(3 * time.Minute))
		xrayConn.Write(buf[:n])
	}
}

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err == nil {
		handleWsDataConnection(conn)
	}
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

func startWsDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WS Listener started on port " + config.DataPort)
	server.ListenAndServe()
}

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
		KeepAlivePeriod: 15 * time.Second,
		MaxIdleTimeout:  30 * time.Second,
	}

	listener, err := quic.ListenAddr("0.0.0.0:"+config.DataPort, tlsConf, quicConfig)
	if err != nil {
		logInfo("QUIC Listen Error: " + err.Error())
		return
	}
	logInfo("QUIC (HTTP/3) Listener started on port " + config.DataPort)

	for {
		conn, err := listener.Accept(context.Background())
		if err == nil {
			go func(c quic.Connection) {
				for {
					stream, err := c.AcceptStream(context.Background())
					if err != nil {
						break
					}
					// QUIC streams natively implement io.ReadWriteCloser!
					go handleMuxStream(stream)
				}
			}(conn)
		}
	}
}

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
					return
				}
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

func wsmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, getSmuxConfig())
	if err != nil {
		return
	}
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			session.Close()
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

func startWssDataListener() {
	mux := http.NewServeMux()
	mux.HandleFunc("/wss", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + config.DataPort, Handler: mux}
	logInfo("WSS Listener started on port " + config.DataPort)
	server.ListenAndServeTLS(config.TlsCertPath, config.TlsKeyPath)
}

func wssmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, getSmuxConfig())
	if err != nil {
		return
	}
	for {
		stream, err := session.AcceptStream()
		if err != nil {
			session.Close()
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
					return
				}
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

// ---------------- Main & Control ----------------

func main() {
	loadServerConfiguration()
	logInfo("Server starting control listener on port " + config.ControlPort)

	switch config.Protocol {
	case "tcp": go startTcpDataListener()
	case "udp": go startUdpDataListener()
	case "ws": go startWsDataListener() // Assuming your existing WS functions are here
	case "tcpmux": go startTcpMuxDataListener()
	case "wsmux": go startWsMuxDataListener() // Assuming your existing WSMUX functions are here
	case "wss": go startWssDataListener() // Assuming your existing WSS functions are here
	case "wssmux": go startWssMuxDataListener() // Assuming your existing WSSMUX functions are here
	case "utcpmux": go startUtcpMuxDataListener()
	case "quic": go startQuicDataListener() // Added QUIC trigger
	}

	listener, err := net.Listen("tcp", "0.0.0.0:"+config.ControlPort)
	if err != nil {
		logInfo("Failed to start control port: " + err.Error())
		os.Exit(1)
	}
	for {
		conn, err := listener.Accept()
		if err == nil {
			go handleControlConnection(conn)
		}
	}
}

func handleControlConnection(conn net.Conn) {
	defer conn.Close()

	challenge := make([]byte, 32)
	rand.Read(challenge)

	conn.Write(challenge)
	conn.Write([]byte("\n"))

	reader := bufio.NewReader(conn)
	clientResponseHex, err := reader.ReadString('\n')
	if err != nil {
		logInfo("Error reading client response.")
		return
	}
	clientResponseHex = strings.TrimSpace(clientResponseHex)

	mac := hmac.New(sha256.New, []byte(config.Token))
	mac.Write(challenge)
	expectedResponseHex := hex.EncodeToString(mac.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(clientResponseHex), []byte(expectedResponseHex)) != 1 {
		logInfo("Unauthorized access attempt dropped.")
		return
	}

	logInfo("Client authenticated successfully.")

	payload := fmt.Sprintf(`{"protocol":"%s","port":"%s"}`, config.Protocol, config.DataPort)
	msg := Message{Command: "start_transport", Payload: payload}
	json.NewEncoder(conn).Encode(msg)

	bufio.NewReader(conn).ReadString('\n')
	logInfo("Client disconnected.")
}