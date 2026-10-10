// Package auth implements the shared-secret authentication used by G-Tun.
//
// Both the control channel and every data connection start with the same
// three-message handshake, which proves knowledge of the token to both sides
// without ever sending it:
//
//	client -> server: Nc                      (32 random bytes)
//	server -> client: Ns || HMAC(token, "server" || Nc || Ns)
//	client -> server: HMAC(token, "client" || Nc || Ns)
//
// The handshake is mutual, replay-resistant (both sides contribute a fresh
// nonce) and direction-bound (the two proofs use different labels).
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
)

const (
	// NonceSize is the size in bytes of the random nonces exchanged in the handshake.
	NonceSize = 32
	// TagSize is the size in bytes of a full authentication tag.
	TagSize = sha256.Size

	labelClient = "gtun/v1/client"
	labelServer = "gtun/v1/server"
)

// ErrAuthFailed is returned when the peer fails to prove knowledge of the token.
var ErrAuthFailed = errors.New("auth: authentication failed")

// Channel identifies one authenticated session. It is the transcript of the
// handshake nonces and can be mixed into later messages to bind them to the
// session.
type Channel []byte

// Sign returns HMAC-SHA256 keyed with token over label followed by parts.
func Sign(token, label string, parts ...[]byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(label))
	for _, p := range parts {
		mac.Write(p)
	}
	return mac.Sum(nil)
}

// Verify reports whether tag matches Sign(token, label, parts...) in constant time.
func Verify(token, label string, tag []byte, parts ...[]byte) bool {
	return hmac.Equal(tag, Sign(token, label, parts...))
}

// DeriveKey derives a purpose-specific key from token.
func DeriveKey(token, purpose string) []byte {
	return Sign(token, "gtun/v1/key/"+purpose)
}

// ClientHandshake runs the client side of the handshake over rw.
func ClientHandshake(rw io.ReadWriter, token string) (Channel, error) {
	clientNonce := make([]byte, NonceSize)
	if _, err := rand.Read(clientNonce); err != nil {
		return nil, fmt.Errorf("auth: generate nonce: %w", err)
	}
	if _, err := rw.Write(clientNonce); err != nil {
		return nil, fmt.Errorf("auth: send nonce: %w", err)
	}

	reply := make([]byte, NonceSize+TagSize)
	if _, err := io.ReadFull(rw, reply); err != nil {
		return nil, fmt.Errorf("auth: read server proof: %w", err)
	}
	serverNonce, serverTag := reply[:NonceSize], reply[NonceSize:]
	if !Verify(token, labelServer, serverTag, clientNonce, serverNonce) {
		return nil, ErrAuthFailed
	}

	if _, err := rw.Write(Sign(token, labelClient, clientNonce, serverNonce)); err != nil {
		return nil, fmt.Errorf("auth: send client proof: %w", err)
	}
	return transcript(clientNonce, serverNonce), nil
}

// ServerHandshake runs the server side of the handshake over rw.
func ServerHandshake(rw io.ReadWriter, token string) (Channel, error) {
	clientNonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rw, clientNonce); err != nil {
		return nil, fmt.Errorf("auth: read client nonce: %w", err)
	}

	serverNonce := make([]byte, NonceSize)
	if _, err := rand.Read(serverNonce); err != nil {
		return nil, fmt.Errorf("auth: generate nonce: %w", err)
	}
	reply := append(serverNonce[:NonceSize:NonceSize], Sign(token, labelServer, clientNonce, serverNonce)...)
	if _, err := rw.Write(reply); err != nil {
		return nil, fmt.Errorf("auth: send server proof: %w", err)
	}

	clientTag := make([]byte, TagSize)
	if _, err := io.ReadFull(rw, clientTag); err != nil {
		return nil, fmt.Errorf("auth: read client proof: %w", err)
	}
	if !Verify(token, labelClient, clientTag, clientNonce, serverNonce) {
		return nil, ErrAuthFailed
	}
	return transcript(clientNonce, serverNonce), nil
}

func transcript(clientNonce, serverNonce []byte) Channel {
	ch := make(Channel, 0, len(clientNonce)+len(serverNonce))
	ch = append(ch, clientNonce...)
	return append(ch, serverNonce...)
}
