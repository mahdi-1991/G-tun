package main

import (
	"bufio"
	"encoding/json"
	"crypto/subtle"
	"fmt"
	"io"
	"net"
	"net/http"
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

func loadServerConfiguration() {
	file, err := os.Open("server_config.json")
	if err != nil { os.Exit(1) }
	defer file.Close()
	json.NewDecoder(file).Decode(&config)
}

func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}

func handleTcpDataConnection(clientConn net.Conn) {
	defer clientConn.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil { 
		logInfo("Failed to dial Xray: " + err.Error())
		return 
	}
	defer xrayConn.Close()
	go relayConnections(xrayConn, clientConn)
	relayConnections(clientConn, xrayConn)
}

func startTcpDataListener() {
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.DataPort)
	if err != nil { logInfo("TCP Listen Error: " + err.Error()); return }
	logInfo("TCP Listener started on port " + config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil { go handleTcpDataConnection(conn) }
	}
}

func startUdpDataListener() {
	udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:"+config.DataPort)
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil { logInfo("UDP Listen Error: " + err.Error()); return }
	logInfo("UDP Listener started on port " + config.DataPort)
	
	sessions := make(map[string]net.Conn)
	var mapMutex sync.Mutex
	buf := make([]byte, 4096)
	
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil { return }
		
		mapMutex.Lock()
		xrayConn, ok := sessions[remoteAddr.String()]
		if !ok {
			xrayConn, err = net.Dial("tcp", config.XrayInboundAddress)
			if err != nil { 
				logInfo("Failed to dial Xray: " + err.Error())
				mapMutex.Unlock(); continue 
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
	CheckOrigin: func(r *http.Request) bool { return true },
	ReadBufferSize: 4096, WriteBufferSize: 4096,
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err == nil { handleWsDataConnection(conn) }
}

func handleWsDataConnection(wsConn *websocket.Conn) {
	defer wsConn.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil { 
		logInfo("Failed to dial Xray: " + err.Error())
		return 
	}
	defer xrayConn.Close()
	errChan := make(chan error, 2)
	go func() {
		for {
			mt, message, err := wsConn.ReadMessage()
			if err != nil { errChan <- err; return }
			if mt == websocket.BinaryMessage {
				if _, err := xrayConn.Write(message); err != nil { errChan <- err; return }
			}
		}
	}()
	go func() {
		bufPtr := bufferPool.Get().(*[]byte)
		defer bufferPool.Put(bufPtr)
		buf := *bufPtr
		for {
			n, err := xrayConn.Read(buf)
			if err != nil { errChan <- err; return }
			if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil { errChan <- err; return }
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
	go relayConnections(xrayConn, stream)
	relayConnections(stream, xrayConn)
}

func startTcpMuxDataListener() {
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.DataPort)
	if err != nil { logInfo("TCPMux Listen Error: " + err.Error()); return }
	logInfo("TCPMux Listener started on port " + config.DataPort)
	for {
		conn, err := listener.Accept()
		if err == nil {
			go func(c net.Conn) {
				session, err := smux.Server(c, nil)
				if err != nil { logInfo("Smux Error: " + err.Error()); return }
				for {
					stream, err := session.AcceptStream()
					if err != nil { break }
					go handleMuxStream(stream)
				}
			}(conn)
		}
	}
}

func wsmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil { return }
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { return }
	for {
		stream, err := session.AcceptStream()
		if err != nil { session.Close(); return }
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
	if err != nil { return }
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { return }
	for {
		stream, err := session.AcceptStream()
		if err != nil { session.Close(); return }
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
	if err != nil { logInfo("KCP Listen Error: " + err.Error()); return }
	logInfo("UTCPMux (KCP) Listener started on port " + config.DataPort)
	for {
		conn, err := listener.AcceptKCP()
		if err == nil {
			conn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
			conn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
			go func(c net.Conn) {
				session, err := smux.Server(c, nil)
				if err != nil { logInfo("Smux Server Error: " + err.Error()); return }
				for {
					stream, err := session.AcceptStream()
					if err != nil { break }
					go handleMuxStream(stream)
				}
			}(conn)
		}
	}
}

func main() {
	loadServerConfiguration()
	logInfo("Server starting control listener on port " + config.ControlPort)

	// Start data listener once based on config
	switch config.Protocol {
	case "tcp": go startTcpDataListener()
	case "udp": go startUdpDataListener()
	case "ws": go startWsDataListener()
	case "tcpmux": go startTcpMuxDataListener()
	case "wsmux": go startWsMuxDataListener()
	case "wss": go startWssDataListener()
	case "wssmux": go startWssMuxDataListener()
	case "utcpmux": go startUtcpMuxDataListener()
	}

	listener, err := net.Listen("tcp", "0.0.0.0:"+config.ControlPort)
	if err != nil { logInfo("Failed to start control port: " + err.Error()); os.Exit(1) }
	for {
		conn, err := listener.Accept()
		if err == nil { go handleControlConnection(conn) }
	}
}

func handleControlConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	
	clientToken, err := reader.ReadString('\n')
	if err != nil {
		logInfo("Error reading token.")
		return
	}

	clientToken = strings.TrimSpace(clientToken)
	
	if subtle.ConstantTimeCompare([]byte(clientToken), []byte(config.Token)) != 1 {
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