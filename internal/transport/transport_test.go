package transport_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/mahdi-1991/G-tun/internal/tlsutil"
	"github.com/mahdi-1991/G-tun/internal/transport"
)

const token = "0123456789abcdef0123456789abcdef"

var (
	streamProtocols = []proto.Name{
		proto.TCP, proto.TCPMux, proto.WS, proto.WSMux, proto.WSS, proto.WSSMux, proto.UTCPMux, proto.QUIC,
		proto.Stealth,
	}
	// Protocols whose streams support half-close end to end. WebSocket has
	// no equivalent and smux half-close can lose data, so those protocols
	// close both directions as soon as either side finishes.
	halfCloseProtocols = []proto.Name{proto.TCP, proto.QUIC}
)

// ---- helpers ----------------------------------------------------------------

func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func freeUDPPort(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	_, port, _ := net.SplitHostPort(pc.LocalAddr().String())
	return port
}

// echoTCP starts a TCP echo service and reports how many connections it accepted.
func echoTCP(t *testing.T) (addr string, accepted *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted = new(atomic.Int64)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String(), accepted
}

// echoUDP starts a UDP echo service and reports how many datagrams it received.
func echoUDP(t *testing.T) (addr string, received *atomic.Int64) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	received = new(atomic.Int64)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			received.Add(1)
			_, _ = pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().String(), received
}

// fakeSite stands in for the real web site that stealth connections are
// relayed to: a TLS 1.3 server that completes handshakes and ignores the rest.
type fakeSite struct {
	addr string
	cert []byte // DER of its certificate
	pool *x509.CertPool
}

func startFakeSite(t *testing.T) *fakeSite {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := tlsutil.GenerateSelfSigned(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(io.Discard, c) }()
		}
	}()
	return &fakeSite{addr: ln.Addr().String(), cert: pair.Certificate[0], pool: pool}
}

type harness struct {
	site      *fakeSite // only for the stealth protocol
	dataAddr  string
	localAddr string
	target    *atomic.Int64 // connections (or datagrams) that reached the target
}

// startTunnel runs a server and a client for p against a fresh echo target.
// The client uses clientToken, which lets tests simulate a wrong secret.
func startTunnel(t *testing.T, p proto.Name, clientToken string) harness {
	t.Helper()
	if p == proto.UDP {
		target, reached := echoUDP(t)
		h := startTunnelTo(t, p, clientToken, target)
		h.target = reached
		return h
	}
	target, reached := echoTCP(t)
	h := startTunnelTo(t, p, clientToken, target)
	h.target = reached
	return h
}

// startTunnelTo is like startTunnel but relays to the given target.
func startTunnelTo(t *testing.T, p proto.Name, clientToken, target string) harness {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if p.TLS() {
		if err := tlsutil.GenerateSelfSigned(certPath, keyPath); err != nil {
			t.Fatal(err)
		}
	}

	var dataPort, localPort string
	if p == proto.UDP {
		dataPort, localPort = freeUDPPort(t), freeUDPPort(t)
	} else {
		dataPort, localPort = freeTCPPort(t), freeTCPPort(t)
		if p.UDPBased() {
			dataPort = freeUDPPort(t)
		}
	}

	var site *fakeSite
	if p == proto.Stealth {
		site = startFakeSite(t)
	}
	srvOpts := transport.ServerOptions{
		Protocol:   p,
		ListenAddr: "127.0.0.1:" + dataPort,
		Target:     target,
		Token:      token,
		CertPath:   certPath,
		KeyPath:    keyPath,
		KCP:        config.DefaultKCP(),
	}
	cliOpts := transport.ClientOptions{
		Protocol:   p,
		ServerAddr: "127.0.0.1:" + dataPort,
		ListenAddr: "127.0.0.1:" + localPort,
		Token:      clientToken,
		CertPath:   certPath,
		KCP:        config.DefaultKCP(),
	}
	if site != nil {
		srvOpts.StealthHandshake = site.addr
		cliOpts.StealthSNI = "localhost"
		cliOpts.StealthRootCAs = site.pool
	}
	srv, err := transport.NewServer(srvOpts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	cli, err := transport.NewClient(cliOpts)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	srvErr, cliErr := make(chan error, 1), make(chan error, 1)
	go func() { srvErr <- srv.Serve(ctx) }()
	go func() { cliErr <- cli.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		for name, ch := range map[string]chan error{"server": srvErr, "client": cliErr} {
			select {
			case err := <-ch:
				if err != nil {
					t.Errorf("%s returned an error on shutdown: %v", name, err)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("%s did not shut down", name)
			}
		}
	})
	return harness{site: site, dataAddr: "127.0.0.1:" + dataPort, localAddr: "127.0.0.1:" + localPort}
}

// eventually retries fn until it succeeds or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = fn(); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %v", timeout, err)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// echoRoundTrip sends data through addr and checks it comes back unchanged.
func echoRoundTrip(addr string, data []byte, halfClose bool) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))

	writeErr := make(chan error, 1)
	go func() {
		_, err := conn.Write(data)
		if err == nil && halfClose {
			err = conn.(*net.TCPConn).CloseWrite()
		}
		writeErr <- err
	}()

	var got []byte
	if halfClose {
		got, err = io.ReadAll(conn)
	} else {
		got = make([]byte, len(data))
		_, err = io.ReadFull(conn, got)
	}
	if err != nil {
		return fmt.Errorf("read echo: %w", err)
	}
	if err := <-writeErr; err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("echo mismatch: got %d bytes, want %d", len(got), len(data))
	}
	return nil
}

// ---- tests ------------------------------------------------------------------

func TestStreamProtocolsEcho(t *testing.T) {
	for _, p := range streamProtocols {
		t.Run(string(p), func(t *testing.T) {
			h := startTunnel(t, p, token)
			eventually(t, 15*time.Second, func() error {
				return echoRoundTrip(h.localAddr, []byte("ping"), false)
			})

			// Many concurrent connections, each moving 512 KiB.
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					data := make([]byte, 512*1024)
					_, _ = rand.Read(data)
					if err := echoRoundTrip(h.localAddr, data, false); err != nil {
						errs <- err
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

func TestHalfCloseKeepsReply(t *testing.T) {
	for _, p := range halfCloseProtocols {
		t.Run(string(p), func(t *testing.T) {
			h := startTunnel(t, p, token)
			data := randomBytes(t, 64*1024)
			eventually(t, 15*time.Second, func() error {
				return echoRoundTrip(h.localAddr, data, true)
			})
		})
	}
}

// replyThenClose is a target that sends a fixed reply and closes at once, like
// an HTTP/1.0 server. The whole reply must reach the client.
func TestTargetReplyThenClose(t *testing.T) {
	for _, p := range streamProtocols {
		t.Run(string(p), func(t *testing.T) {
			reply := randomBytes(t, 256*1024)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ln.Close() })
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						_, _ = c.Write(reply)
						c.Close()
					}()
				}
			}()

			h := startTunnelTo(t, p, token, ln.Addr().String())
			eventually(t, 15*time.Second, func() error {
				conn, err := net.DialTimeout("tcp", h.localAddr, 5*time.Second)
				if err != nil {
					return err
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				got, err := io.ReadAll(conn)
				if err != nil {
					return err
				}
				if !bytes.Equal(got, reply) {
					return fmt.Errorf("got %d bytes, want %d", len(got), len(reply))
				}
				return nil
			})
		})
	}
}

// Anyone who connects to a stealth server without the token, such as a
// firewall's active prober, must see the real web site and nothing else.
func TestStealthProbeSeesRealSite(t *testing.T) {
	h := startTunnel(t, proto.Stealth, token)
	// Make sure the server is up first.
	eventually(t, 15*time.Second, func() error {
		return echoRoundTrip(h.localAddr, []byte("ping"), false)
	})

	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", h.dataAddr, &tls.Config{
		ServerName:         "localhost",
		InsecureSkipVerify: true, // The test compares the certificate itself below.
		MinVersion:         tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("a plain TLS client could not complete a handshake: %v", err)
	}
	defer conn.Close()

	got := conn.ConnectionState().PeerCertificates[0].Raw
	if !bytes.Equal(got, h.site.cert) {
		t.Error("the prober received a certificate other than the real site's")
	}
}

func TestUDPEcho(t *testing.T) {
	h := startTunnel(t, proto.UDP, token)

	conn, err := net.Dial("udp", h.localAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Wait until the tunnel answers, retrying because the first datagrams may
	// be sent before the server is listening.
	eventually(t, 10*time.Second, func() error {
		_, _ = conn.Write([]byte("hello"))
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		buf := make([]byte, 16)
		_, err := conn.Read(buf)
		return err
	})

	// Datagram boundaries and sizes must be preserved.
	sizes := []int{1, 100, 1200, 8000, 60000}
	for _, size := range sizes {
		payload := randomBytes(t, size)
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write %d bytes: %v", size, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 65535)
		n, err := conn.Read(buf)
		if err != nil {
			t.Fatalf("read echo of %d bytes: %v", size, err)
		}
		if !bytes.Equal(buf[:n], payload) {
			t.Errorf("echo of %d-byte datagram came back as %d bytes", size, n)
		}
	}
}

// A client that does not know the token must never reach the target.
func TestWrongTokenNeverReachesTarget(t *testing.T) {
	for _, p := range streamProtocols {
		t.Run(string(p), func(t *testing.T) {
			h := startTunnel(t, p, "a-completely-different-secret")

			// Try several times so the client has certainly attempted a
			// handshake before we assert. KCP handshakes time out instead of
			// being refused, so one attempt is enough there.
			attempts := 5
			if p == proto.UTCPMux {
				attempts = 1
			}
			for range attempts {
				if err := echoRoundTrip(h.localAddr, []byte("secret payload"), false); err == nil {
					t.Fatal("data was echoed although the token is wrong")
				}
				time.Sleep(100 * time.Millisecond)
			}
			if n := h.target.Load(); n != 0 {
				t.Errorf("target received %d connections from an unauthenticated client", n)
			}
		})
	}
}

func TestWrongTokenUDPIsDropped(t *testing.T) {
	h := startTunnel(t, proto.UDP, "a-completely-different-secret")
	conn, err := net.Dial("udp", h.localAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for range 5 {
		_, _ = conn.Write([]byte("secret payload"))
		_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := conn.Read(make([]byte, 64)); err == nil {
			t.Fatal("datagram was echoed although the token is wrong")
		}
	}
	if n := h.target.Load(); n != 0 {
		t.Errorf("target received %d datagrams from an unauthenticated client", n)
	}
}

// Anything that is not the handshake must be rejected before reaching the target.
func TestRawConnectionIsRejected(t *testing.T) {
	target, reached := echoTCP(t)
	dataPort := freeTCPPort(t)
	srv, err := transport.NewServer(transport.ServerOptions{
		Protocol:   proto.TCP,
		ListenAddr: "127.0.0.1:" + dataPort,
		Target:     target,
		Token:      token,
		KCP:        config.DefaultKCP(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	eventually(t, 5*time.Second, func() error {
		conn, err := net.Dial("tcp", "127.0.0.1:"+dataPort)
		if err != nil {
			return err
		}
		defer conn.Close()
		// 32 bytes of junk look like a client nonce; the follow-up proof is wrong.
		_, _ = conn.Write(bytes.Repeat([]byte{'x'}, 32+32))
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err = io.ReadAll(conn)
		return err // The server must close the connection, ending ReadAll.
	})
	if n := reached.Load(); n != 0 {
		t.Errorf("target received %d connections from a raw client", n)
	}
}

func TestNewClientAndServerValidateInput(t *testing.T) {
	if _, err := transport.NewServer(transport.ServerOptions{Protocol: "bogus"}); err == nil {
		t.Error("NewServer accepted an unknown protocol")
	}
	if _, err := transport.NewServer(transport.ServerOptions{Protocol: proto.WSS, CertPath: "/nonexistent", KeyPath: "/nonexistent"}); err == nil {
		t.Error("NewServer accepted a missing certificate")
	}
	if _, err := transport.NewClient(transport.ClientOptions{Protocol: proto.QUIC, CertPath: "/nonexistent"}); err == nil {
		t.Error("NewClient accepted a missing pinned certificate")
	}
}

// A client must refuse a server whose certificate is not the pinned one.
func TestClientRejectsUnpinnedCertificate(t *testing.T) {
	for _, p := range []proto.Name{proto.WSS, proto.QUIC} {
		t.Run(string(p), func(t *testing.T) {
			serverDir, otherDir := t.TempDir(), t.TempDir()
			for _, d := range []string{serverDir, otherDir} {
				if err := tlsutil.GenerateSelfSigned(filepath.Join(d, "cert.pem"), filepath.Join(d, "key.pem")); err != nil {
					t.Fatal(err)
				}
			}
			target, reached := echoTCP(t)
			dataPort, localPort := freeTCPPort(t), freeTCPPort(t)
			if p == proto.QUIC {
				dataPort = freeUDPPort(t)
			}
			srv, err := transport.NewServer(transport.ServerOptions{
				Protocol: p, ListenAddr: "127.0.0.1:" + dataPort, Target: target, Token: token,
				CertPath: filepath.Join(serverDir, "cert.pem"), KeyPath: filepath.Join(serverDir, "key.pem"),
				KCP: config.DefaultKCP(),
			})
			if err != nil {
				t.Fatal(err)
			}
			cli, err := transport.NewClient(transport.ClientOptions{
				Protocol: p, ServerAddr: "127.0.0.1:" + dataPort, ListenAddr: "127.0.0.1:" + localPort, Token: token,
				CertPath: filepath.Join(otherDir, "cert.pem"), // pins the wrong certificate
				KCP:      config.DefaultKCP(),
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 2)
			go func() { done <- srv.Serve(ctx) }()
			go func() { done <- cli.Run(ctx) }()
			t.Cleanup(func() { cancel(); <-done; <-done })

			for range 4 {
				if err := echoRoundTrip("127.0.0.1:"+localPort, []byte("data"), false); err == nil {
					t.Fatal("client talked to a server with an unpinned certificate")
				}
				time.Sleep(100 * time.Millisecond)
			}
			if n := reached.Load(); n != 0 {
				t.Errorf("target received %d connections", n)
			}
		})
	}
}
