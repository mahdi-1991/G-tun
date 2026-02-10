package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
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
	ControlPort        string
	DataPorts          map[string]string
	XrayInboundAddress string
	TlsCertPath        string
	TlsKeyPath         string
	KcpConfig          KcpConfig
}

var config ServerConfig
var activeListeners []io.Closer
var mu sync.Mutex
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
func loadServerConfiguration() {
	file, err := os.Open("server_config.json")
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
func addListener(l io.Closer) {
	mu.Lock()
	defer mu.Unlock()
	activeListeners = append(activeListeners, l)
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
		return
	}
	defer xrayConn.Close()
	go relayConnections(xrayConn, clientConn)
	relayConnections(clientConn, xrayConn)
}
func startTcpDataListener() {
	port := config.DataPorts["TCP"]
	listener, _ := net.Listen("tcp", "0.0.0.0:"+port)
	addListener(listener)
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go handleTcpDataConnection(conn)
	}
}
func startUdpDataListener() {
	port := config.DataPorts["UDP"]
	udpAddr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:"+port)
	conn, _ := net.ListenUDP("udp", udpAddr)
	addListener(conn)
	defer conn.Close()
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
	if err != nil {
		return
	}
	handleWsDataConnection(conn)
}
func handleWsDataConnection(wsConn *websocket.Conn) {
	defer wsConn.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil {
		return
	}
	defer xrayConn.Close()
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
	port := config.DataPorts["WS"]
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: mux}
	addListener(server)
	server.ListenAndServe()
}
func handleMuxStream(stream io.ReadWriteCloser) {
	defer stream.Close()
	xrayConn, err := net.Dial("tcp", config.XrayInboundAddress)
	if err != nil {
		return
	}
	defer xrayConn.Close()
	go relayConnections(xrayConn, stream)
	relayConnections(stream, xrayConn)
}
func startTcpMuxDataListener() {
	port := config.DataPorts["TCPMux"]
	listener, _ := net.Listen("tcp", "0.0.0.0:"+port)
	addListener(listener)
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			session, err := smux.Server(c, nil)
			if err != nil {
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
func wsmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, nil)
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
	port := config.DataPorts["WSMux"]
	mux := http.NewServeMux()
	mux.HandleFunc("/wsmux", wsmuxHandler)
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: mux}
	addListener(server)
	server.ListenAndServe()
}
func startWssDataListener() {
	port := config.DataPorts["WSS"]
	mux := http.NewServeMux()
	mux.HandleFunc("/wss", wsHandler)
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: mux}
	addListener(server)
	server.ListenAndServeTLS(config.TlsCertPath, config.TlsKeyPath)
}
func wssmuxHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	session, err := smux.Server(&wsConnWrapper{Conn: ws}, nil)
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
	port := config.DataPorts["WSSMux"]
	mux := http.NewServeMux()
	mux.HandleFunc("/wssmux", wssmuxHandler)
	server := &http.Server{Addr: "0.0.0.0:" + port, Handler: mux}
	addListener(server)
	server.ListenAndServeTLS(config.TlsCertPath, config.TlsKeyPath)
}
func startUtcpMuxDataListener() {
	port := config.DataPorts["UTCPMux"]
	kcpConf := config.KcpConfig
	listener, err := kcp.ListenWithOptions("0.0.0.0:"+port, nil, kcpConf.DataShards, kcpConf.ParityShards)
	if err != nil {
		return
	}
	addListener(listener)
	for {
		conn, err := listener.AcceptKCP()
		if err != nil {
			return
		}
		conn.SetNoDelay(kcpConf.NoDelay, kcpConf.Interval, kcpConf.Resend, kcpConf.NoCongestion)
		conn.SetWindowSize(kcpConf.SndWnd, kcpConf.RcvWnd)
		go func(c net.Conn) {
			session, err := smux.Server(c, nil)
			if err != nil {
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

func main() {
	loadServerConfiguration()
	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, os.Interrupt, syscall.SIGTERM)
	log("Control Server is starting...")
	listener, err := net.Listen("tcp", "0.0.0.0:"+config.ControlPort)
	if err != nil {
		os.Exit(1)
	}
	addListener(listener)
	go func() {
		<-quitChannel
		log("INFO: Shutdown signal received. Closing all listeners...")
		mu.Lock()
		for _, l := range activeListeners {
			l.Close()
		}
		mu.Unlock()
		log("INFO: All listeners closed. Exiting.")
		os.Exit(0)
	}()
	log(fmt.Sprintf("Waiting for control client to connect on %s", config.ControlPort))
	conn, err := listener.Accept()
	if err != nil {
		if !strings.Contains(err.Error(), "use of closed network connection") {
			log(fmt.Sprintf("FATAL: Could not accept control client: %v", err))
		}
		return
	}
	handleControlConnection(conn)
}
func handleControlConnection(conn net.Conn) {
	defer conn.Close()
	log(fmt.Sprintf("INFO: Control client connected from %s", conn.RemoteAddr().String()))
	fmt.Println("\n--- Transport Protocol Selection ---")
	fmt.Println("1. TCP")
	fmt.Println("2. UDP")
	fmt.Println("3. WebSocket (WS)")
	fmt.Println("4. TCPMux")
	fmt.Println("5. WSMux")
	fmt.Println("6. WebSocket Secure (WSS)")
	fmt.Println("7. WSSMux")
	fmt.Println("8. UTCPMux (KCP)")
	fmt.Print("Enter your choice: ")
	reader := bufio.NewReader(os.Stdin)
	choice, _ := reader.ReadString('\n')
	choice = strings.TrimSpace(choice)
	writer := json.NewEncoder(conn)
	var proto, port string
	switch choice {
	case "1":
		proto, port = "tcp", config.DataPorts["TCP"]
		go startTcpDataListener()
	case "2":
		proto, port = "udp", config.DataPorts["UDP"]
		go startUdpDataListener()
	case "3":
		proto, port = "ws", config.DataPorts["WS"]
		go startWsDataListener()
	case "4":
		proto, port = "tcpmux", config.DataPorts["TCPMux"]
		go startTcpMuxDataListener()
	case "5":
		proto, port = "wsmux", config.DataPorts["WSMux"]
		go startWsMuxDataListener()
	case "6":
		proto, port = "wss", config.DataPorts["WSS"]
		go startWssDataListener()
	case "7":
		proto, port = "wssmux", config.DataPorts["WSSMux"]
		go startWssMuxDataListener()
	case "8":
		proto, port = "utcpmux", config.DataPorts["UTCPMux"]
		go startUtcpMuxDataListener()
	default:
		log("Invalid choice")
		conn.Close()
		os.Exit(1)
		return
	}
	log(fmt.Sprintf("INFO: User selected %s. Sending command to client...", proto))
	payload := fmt.Sprintf(`{"protocol":"%s","port":"%s"}`, proto, port)
	msg := Message{Command: "start_transport", Payload: payload}
	writer.Encode(msg)
	log(fmt.Sprintf("INFO: %s command sent. Data listener is running.", strings.ToUpper(proto)))
	select {}
}
