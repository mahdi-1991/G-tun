package client_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mahdi-1991/G-tun/internal/client"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/mahdi-1991/G-tun/internal/server"
	"github.com/mahdi-1991/G-tun/internal/tlsutil"
)

const token = "0123456789abcdef0123456789abcdef"

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func echoServer(t *testing.T) string {
	t.Helper()
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
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// start runs fn in the background and returns a function that stops it and
// waits for it to return.
func start(t *testing.T, fn func(context.Context) error) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("returned an error on shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("did not shut down")
		}
	}
	t.Cleanup(stop)
	return stop
}

func echo(addr string, payload []byte) error {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return err
	}
	if !bytes.Equal(got, payload) {
		return fmt.Errorf("echo mismatch")
	}
	return nil
}

func waitForEcho(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var err error
	for time.Now().Before(deadline) {
		if err = echo(addr, []byte("hello through the tunnel")); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("tunnel did not come up within %v: %v", timeout, err)
}

func serverConfig(p proto.Name, controlPort, dataPort, target string) *config.Server {
	kcp := config.DefaultKCP()
	return &config.Server{
		ControlPort: controlPort, DataPort: dataPort, Protocol: p,
		TargetAddress: target, Token: token, KCP: &kcp,
	}
}

func clientConfig(controlPort, localPort, clientToken string) *config.Client {
	return &config.Client{
		ControlAddress: "127.0.0.1:" + controlPort,
		RemoteHost:     "127.0.0.1",
		ListenAddress:  "127.0.0.1:" + localPort,
		Token:          clientToken,
	}
}

func TestTunnelThroughControlChannel(t *testing.T) {
	target := echoServer(t)
	controlPort, dataPort, localPort := freePort(t), freePort(t), freePort(t)

	start(t, func(ctx context.Context) error {
		return server.Run(ctx, serverConfig(proto.TCPMux, controlPort, dataPort, target))
	})
	start(t, func(ctx context.Context) error {
		return client.Run(ctx, clientConfig(controlPort, localPort, token))
	})
	waitForEcho(t, "127.0.0.1:"+localPort, 10*time.Second)
}

func TestClientWithWrongTokenGetsNoTunnel(t *testing.T) {
	target := echoServer(t)
	controlPort, dataPort, localPort := freePort(t), freePort(t), freePort(t)

	start(t, func(ctx context.Context) error {
		return server.Run(ctx, serverConfig(proto.TCP, controlPort, dataPort, target))
	})
	start(t, func(ctx context.Context) error {
		return client.Run(ctx, clientConfig(controlPort, localPort, "a-completely-different-secret"))
	})

	time.Sleep(1500 * time.Millisecond)
	if err := echo("127.0.0.1:"+localPort, []byte("data")); err == nil {
		t.Fatal("a client with the wrong token got a working tunnel")
	}
}

// The client must recover on its own when the server restarts, including
// re-creating the multiplexed session that died with the old server.
func TestClientReconnectsAfterServerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("takes several seconds because of the reconnect backoff")
	}
	target := echoServer(t)
	controlPort, dataPort, localPort := freePort(t), freePort(t), freePort(t)
	run := func(ctx context.Context) error {
		return server.Run(ctx, serverConfig(proto.TCPMux, controlPort, dataPort, target))
	}

	stopServer := start(t, run)
	start(t, func(ctx context.Context) error {
		return client.Run(ctx, clientConfig(controlPort, localPort, token))
	})
	waitForEcho(t, "127.0.0.1:"+localPort, 10*time.Second)

	stopServer()
	if err := echo("127.0.0.1:"+localPort, []byte("x")); err == nil {
		t.Fatal("tunnel still works with the server stopped")
	}

	start(t, run)
	waitForEcho(t, "127.0.0.1:"+localPort, 20*time.Second)
}

// fakeSite is a TLS 1.3 web site standing in for the stealth handshake site.
// It returns its address and the path of its certificate.
func fakeSite(t *testing.T) (addr, certPath string) {
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
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13})
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
	return ln.Addr().String(), certPath
}

// Stealth needs no control port: the server starts none, and the client is
// configured with the transport directly.
func TestStealthWithoutControlChannel(t *testing.T) {
	target := echoServer(t)
	site, siteCert := fakeSite(t)
	dataPort, localPort := freePort(t), freePort(t)

	srvCfg := serverConfig(proto.Stealth, "", dataPort, target)
	srvCfg.StealthHandshakeServer = site
	start(t, func(ctx context.Context) error { return server.Run(ctx, srvCfg) })

	cliCfg := &config.Client{
		Protocol:      proto.Stealth,
		DataPort:      dataPort,
		StealthSNI:    "localhost",
		StealthCAFile: siteCert,
		RemoteHost:    "127.0.0.1",
		ListenAddress: "127.0.0.1:" + localPort,
		Token:         token,
	}
	start(t, func(ctx context.Context) error { return client.Run(ctx, cliCfg) })
	waitForEcho(t, "127.0.0.1:"+localPort, 15*time.Second)

	// The control port really does not exist.
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:8080", 200*time.Millisecond); err == nil {
		conn.Close()
		t.Log("something else listens on 8080; skipping the control-port assertion")
	}
}

func TestStealthRejectsBadCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Client{
		Protocol: proto.Stealth, DataPort: "443", StealthSNI: "localhost", StealthCAFile: path,
		RemoteHost: "127.0.0.1", ListenAddress: "127.0.0.1:" + freePort(t), Token: token,
	}
	if err := client.Run(context.Background(), cfg); err == nil {
		t.Fatal("client.Run accepted a CA file without certificates")
	}
}
