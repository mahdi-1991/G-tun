package main

import (
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
	ControlServerAddress string
	LocalListenPort      string
	RemoteServerIP       string
	KcpConfig            KcpConfig
}

var config ClientConfig
var bufferPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 64*1024)
		return &b
	},
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
func log(message string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), message)
}
func loadClientConfiguration() {
	file, err := os.Open("client_config.json")
	if err != nil {
		os.Exit(1)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		os.Exit(1)
	}
}
func relayConnections(dst io.Writer, src io.Reader) {
	bufPtr := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufPtr)
	io.CopyBuffer(dst, src, *bufPtr)
}
func startTcpDataForwarder(dataPort string) {
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	defer listener.Close()
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(lconn net.Conn) {
			defer lconn.Close()
			rconn, err := net.Dial("tcp", remoteDataAddr)
			if err != nil {
				return
			}
			defer rconn.Close()
			go relayConnections(rconn, lconn)
			relayConnections(lconn, rconn)
		}(localConn)
	}
}
func startUdpDataForwarder(dataPort string) {
	localAddr, _ := net.ResolveUDPAddr("udp", config.LocalListenPort)
	localConn, _ := net.ListenUDP("udp", localAddr)
	defer localConn.Close()
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	sessions := make(map[string]*net.UDPConn)
	var mu sync.Mutex
	buf := make([]byte, 4096)
	for {
		n, clientAddr, err := localConn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
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
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	defer listener.Close()
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/ws"}
	remoteWsAddr := u.String()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := websocket.DefaultDialer.Dial(remoteWsAddr, nil)
			if err != nil {
				return
			}
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}
func handleLocalMuxConnection(lconn net.Conn, session *smux.Session) {
	defer lconn.Close()
	stream, err := session.OpenStream()
	if err != nil {
		return
	}
	defer stream.Close()
	go relayConnections(stream, lconn)
	relayConnections(lconn, stream)
}
func startTcpMuxDataForwarder(dataPort string) {
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	baseConn, err := net.Dial("tcp", remoteDataAddr)
	if err != nil {
		return
	}
	session, err := smux.Client(baseConn, nil)
	if err != nil {
		return
	}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		return
	}
	defer listener.Close()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleLocalMuxConnection(localConn, session)
	}
}
func startWsMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "ws", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wsmux"}
	remoteWsAddr := u.String()
	ws, _, err := websocket.DefaultDialer.Dial(remoteWsAddr, nil)
	if err != nil {
		return
	}
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil {
		return
	}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		return
	}
	defer listener.Close()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(lconn net.Conn, sess *smux.Session) {
			handleLocalMuxConnection(lconn, sess)
		}(localConn, session)
	}
}
func startWssDataForwarder(dataPort string) {
	listener, _ := net.Listen("tcp", config.LocalListenPort)
	defer listener.Close()
	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wss"}
	remoteWssAddr := u.String()
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(lconn net.Conn) {
			defer lconn.Close()
			wsConn, _, err := dialer.Dial(remoteWssAddr, nil)
			if err != nil {
				return
			}
			defer wsConn.Close()
			relayWs(lconn, wsConn)
		}(localConn)
	}
}
func startWssMuxDataForwarder(dataPort string) {
	u := url.URL{Scheme: "wss", Host: config.RemoteServerIP + ":" + dataPort, Path: "/wssmux"}
	remoteWssAddr := u.String()
	dialer := websocket.DefaultDialer
	dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	ws, _, err := dialer.Dial(remoteWssAddr, nil)
	if err != nil {
		return
	}
	session, err := smux.Client(&wsConnWrapper{Conn: ws}, nil)
	if err != nil {
		return
	}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		return
	}
	defer listener.Close()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go func(lconn net.Conn, sess *smux.Session) {
			handleLocalMuxConnection(lconn, sess)
		}(localConn, session)
	}
}
func startUtcpMuxDataForwarder(dataPort string) {
	remoteDataAddr := config.RemoteServerIP + ":" + dataPort
	kcpConf := config.KcpConfig
	baseConn, err := kcp.DialWithOptions(remoteDataAddr, nil, kcpConf.DataShards, kcpConf.ParityShards)
	if err != nil {
		return
	}
	baseConn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
	baseConn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
	session, err := smux.Client(baseConn, nil)
	if err != nil {
		return
	}
	listener, err := net.Listen("tcp", config.LocalListenPort)
	if err != nil {
		return
	}
	defer listener.Close()
	for {
		localConn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleLocalMuxConnection(localConn, session)
	}
}

func main() {
	loadClientConfiguration()
	for {
		log(fmt.Sprintf("Attempting to connect to control server at %s", config.ControlServerAddress))
		conn, err := net.Dial("tcp", config.ControlServerAddress)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}
		handleControlConnection(conn)
		log("INFO: Control connection lost. Will attempt to reconnect.")
	}
}
func handleControlConnection(conn net.Conn) {
	log("INFO: Successfully connected to control server.")
	defer conn.Close()
	reader := json.NewDecoder(conn)
	for {
		var msg Message
		if err := reader.Decode(&msg); err != nil {
			return
		}
		log(fmt.Sprintf("Received command: '%s'", msg.Command))
		var configData TransportConfig
		json.Unmarshal([]byte(msg.Payload), &configData)
		switch configData.Protocol {
		case "tcp":
			go startTcpDataForwarder(configData.Port)
		case "udp":
			go startUdpDataForwarder(configData.Port)
		case "ws":
			go startWsDataForwarder(configData.Port)
		case "tcpmux":
			go startTcpMuxDataForwarder(configData.Port)
		case "wsmux":
			go startWsMuxDataForwarder(configData.Port)
		case "wss":
			go startWssDataForwarder(configData.Port)
		case "wssmux":
			go startWssMuxDataForwarder(configData.Port)
		case "utcpmux":
			go startUtcpMuxDataForwarder(configData.Port)
		}
	}
}