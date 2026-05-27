package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
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

var (
	config         ClientConfig
	bufferPool     = sync.Pool{New: func() interface{} { b := make([]byte, 64*1024); return &b }}
	activeListener io.Closer
	activeSession  io.Closer
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

func stopTransport() {
	mu.Lock()
	defer mu.Unlock()
	if activeListener != nil {
		activeListener.Close()
		activeListener = nil
	}
	if activeSession != nil {
		activeSession.Close()
		activeSession = nil
	}
}

func startTcpDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); return }
	mu.Lock(); activeListener = listener; mu.Unlock()
	
	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil { logInfo("Failed to dial remote server: " + err.Error()); return }
			defer rconn.Close()
			go relayConnections(rconn, lconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}

func startUdpDataForwarder(dataPort string) {
	localAddr, _ := net.ResolveUDPAddr("udp", config.LocalListenPort)
	localConn, err := net.ListenUDP("udp", localAddr)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); return }
	mu.Lock(); activeListener = localConn; mu.Unlock()

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
	if err != nil { logInfo("Local Listen Error: " + err.Error()); return }
	mu.Lock(); activeListener = listener; mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/ws"}
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
			if err != nil { logInfo("WS Dial Error: " + err.Error()); return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func handleLocalMuxConnection(lconn net.Conn, session *smux.Session) {
	defer lconn.Close()
	stream, err := session.OpenStream()
	if err != nil { logInfo("Smux OpenStream Error: " + err.Error()); return }
	defer stream.Close()
	go relayConnections(stream, lconn)
	relayConnections(lconn, stream)
}

func startTcpMuxDataForwarder(dataPort string) {
	baseConn, err := net.Dial("tcp", config.RemoteServerIP+":"+dataPort)
	if err != nil { logInfo("Remote Dial Error: " + err.Error()); return }
	session, err := smux.Client(baseConn, nil)
	if err != nil { logInfo("Smux Client Error: " + err.Error()); return }
	
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); session.Close(); return }
	mu.Lock(); activeListener = listener; activeSession = session; mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startWsMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wsmux"}
	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil { logInfo("WS Dial Error: " + err.Error()); return }
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { logInfo("Smux Client Error: " + err.Error()); return }
	
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); session.Close(); return }
	mu.Lock(); activeListener = listener; activeSession = session; mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startWssDataForwarder(dataPort string) {
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); return }
	mu.Lock(); activeListener = listener; mu.Unlock()

	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wss"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	
	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := dialer.Dial(u.String(), nil)
			if err != nil { logInfo("WSS Dial Error: " + err.Error()); return }
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}

func startWssMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wssmux"}
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	ws, _, err := dialer.Dial(u.String(), nil)
	if err != nil { logInfo("WSS Dial Error: " + err.Error()); return }
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil { logInfo("Smux Client Error: " + err.Error()); return }
	
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); session.Close(); return }
	mu.Lock(); activeListener = listener; activeSession = session; mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort)
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func startUtcpMuxDataForwarder(dataPort string) {
	kcpConf := config.KcpConfig
	baseConn, err := kcp.DialWithOptions(config.RemoteServerIP+":"+dataPort, nil, kcpConf.DataShards, kcpConf.ParityShards)
	if err != nil { logInfo("KCP Dial Error: " + err.Error()); return }
	baseConn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
	baseConn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
	
	session, err := smux.Client(baseConn, nil)
	if err != nil { logInfo("Smux Client Error: " + err.Error()); baseConn.Close(); return }
	
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil { logInfo("Local Listen Error: " + err.Error()); session.Close(); return }
	mu.Lock(); activeListener = listener; activeSession = session; mu.Unlock()

	logInfo("Ready! Listening locally on " + config.LocalListenPort + " for traffic.")
	for {
		localConn, err := listener.Accept()
		if err != nil { return }
		go handleLocalMuxConnection(localConn, session)
	}
}

func main() {
	loadClientConfiguration()

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
			stopTransport() // Cleanly release ports before starting a new one
			
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
			}
		}

		reader.ReadString('\n') // Block cleanly until connection actually drops
		conn.Close()
	}
}