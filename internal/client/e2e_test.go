package client_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/mahdi-1991/G-tun/internal/client"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/mahdi-1991/G-tun/internal/server"
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
