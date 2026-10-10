package netutil

import (
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	server := <-accepted
	t.Cleanup(func() { client.Close(); server.Close() })
	return client.(*net.TCPConn), server.(*net.TCPConn)
}

// A client that finishes sending and half-closes must still receive the reply.
func TestPipeKeepsReplyAfterHalfClose(t *testing.T) {
	appConn, pipeA := tcpPair(t)  // application <-> Pipe
	pipeB, upstream := tcpPair(t) // Pipe <-> upstream service
	go Pipe(pipeA, pipeB)

	// Upstream: read the whole request, then answer.
	go func() {
		req, _ := io.ReadAll(upstream)
		upstream.Write(append([]byte("reply to: "), req...))
		upstream.Close()
	}()

	appConn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := appConn.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := appConn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(appConn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if want := "reply to: request"; string(got) != want {
		t.Errorf("reply = %q, want %q", got, want)
	}
}

func TestPipeClosesBothSides(t *testing.T) {
	appConn, pipeA := tcpPair(t)
	pipeB, upstream := tcpPair(t)
	done := make(chan struct{})
	go func() { Pipe(pipeA, pipeB); close(done) }()

	appConn.Close()
	upstream.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Pipe did not return after both peers closed")
	}
}
