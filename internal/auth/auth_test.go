package auth

import (
	"bytes"
	"errors"
	"net"
	"testing"
)

func handshake(t *testing.T, serverToken, clientToken string) (srvCh, cliCh Channel, srvErr, cliErr error) {
	t.Helper()
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srvCh, srvErr = ServerHandshake(s, serverToken)
		if srvErr != nil {
			s.Close() // Unblock the client like a real server would.
		}
	}()
	cliCh, cliErr = ClientHandshake(c, clientToken)
	if cliErr != nil {
		c.Close()
	}
	<-done
	return
}

func TestHandshakeSuccess(t *testing.T) {
	srv, cli, srvErr, cliErr := handshake(t, "shared-secret-token", "shared-secret-token")
	if srvErr != nil || cliErr != nil {
		t.Fatalf("handshake failed: server=%v client=%v", srvErr, cliErr)
	}
	if !bytes.Equal(srv, cli) {
		t.Errorf("channels differ: server=%x client=%x", srv, cli)
	}
	if len(srv) != 2*NonceSize {
		t.Errorf("channel length = %d, want %d", len(srv), 2*NonceSize)
	}
}

func TestHandshakeWrongToken(t *testing.T) {
	_, _, srvErr, cliErr := handshake(t, "server-secret-token", "other-client-secret")
	if !errors.Is(cliErr, ErrAuthFailed) {
		t.Errorf("client error = %v, want ErrAuthFailed (server must prove itself too)", cliErr)
	}
	if srvErr == nil {
		t.Error("server accepted a client with the wrong token")
	}
}

func TestHandshakeChannelsAreUnique(t *testing.T) {
	a, _, _, _ := handshake(t, "shared-secret-token", "shared-secret-token")
	b, _, _, _ := handshake(t, "shared-secret-token", "shared-secret-token")
	if bytes.Equal(a, b) {
		t.Error("two handshakes produced the same channel; nonces are not fresh")
	}
}

func TestDatagram(t *testing.T) {
	const token = "shared-secret-token"
	payload := []byte("hello datagram")

	tests := []struct {
		name    string
		mutate  func([]byte) []byte
		openDir Direction
		token   string
		wantOK  bool
	}{
		{"valid", func(b []byte) []byte { return b }, ClientToServer, token, true},
		{"wrong direction", func(b []byte) []byte { return b }, ServerToClient, token, false},
		{"wrong token", func(b []byte) []byte { return b }, ClientToServer, "different-token-value", false},
		{"tampered payload", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }, ClientToServer, token, false},
		{"tampered tag", func(b []byte) []byte { b[0] ^= 1; return b }, ClientToServer, token, false},
		{"too short", func(b []byte) []byte { return b[:DatagramOverhead-1] }, ClientToServer, token, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pkt := tc.mutate(SealDatagram(token, ClientToServer, payload))
			got, ok := OpenDatagram(tc.token, tc.openDir, pkt)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !bytes.Equal(got, payload) {
				t.Errorf("payload = %q, want %q", got, payload)
			}
		})
	}
}

func TestDeriveKeyIsPurposeBound(t *testing.T) {
	if bytes.Equal(DeriveKey("token-value-123456", "a"), DeriveKey("token-value-123456", "b")) {
		t.Error("different purposes produced the same key")
	}
	if len(DeriveKey("token-value-123456", "kcp")) != 32 {
		t.Error("derived key is not 32 bytes (AES-256)")
	}
}
