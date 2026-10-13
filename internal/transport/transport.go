// Package transport implements the data-plane protocols that carry traffic
// between a G-Tun client and server.
//
// Every connection, whatever the protocol, starts with the authentication
// handshake from package auth, so an unauthenticated peer can never reach the
// target service. Stream protocols (tcp, ws, wss, quic and the smux based
// *mux variants) relay TCP streams; the udp protocol relays datagrams.
package transport

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/mahdi-1991/G-tun/internal/auth"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/mahdi-1991/G-tun/internal/tlsutil"
	"github.com/xtaci/smux"
)

const (
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second
	keepAlive        = 15 * time.Second

	// quicALPN is the application protocol negotiated by QUIC. It matches
	// HTTP/3 so the handshake looks like ordinary web traffic.
	quicALPN = "h3"
)

// Stream is one bidirectional byte stream.
type Stream = io.ReadWriteCloser

// ServerOptions configures a Server.
type ServerOptions struct {
	Protocol proto.Name
	// ListenAddr is the address of the data port, for example ":8081".
	ListenAddr string
	// Target is the TCP (UDP for the udp protocol) address that traffic is relayed to.
	Target string
	Token  string
	// CertPath and KeyPath are required for protocols that use TLS.
	CertPath string
	KeyPath  string
	KCP      config.KCP
	// StealthHandshake is the host:port of the real web site that
	// unauthenticated stealth connections are forwarded to.
	StealthHandshake string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	Protocol proto.Name
	// ServerAddr is the host:port of the server's data port.
	ServerAddr string
	// ListenAddr is the local address that applications connect to.
	ListenAddr string
	Token      string
	// CertPath is the pinned server certificate, required for protocols that use TLS.
	CertPath string
	KCP      config.KCP
	// StealthSNI is the server name presented in the stealth TLS handshake.
	StealthSNI string
	// StealthRootCAs verifies the handshake site's certificate. Nil means the
	// system roots.
	StealthRootCAs *x509.CertPool
}

// Server accepts tunnel connections and relays them to the target.
type Server interface {
	// Serve runs until ctx is done or a fatal error occurs.
	Serve(ctx context.Context) error
}

// Client listens locally and relays connections through the tunnel.
type Client interface {
	// Run runs until ctx is done or a fatal error occurs.
	Run(ctx context.Context) error
}

// NewServer returns a Server for the given options.
func NewServer(o ServerOptions) (Server, error) {
	if _, err := proto.Parse(string(o.Protocol)); err != nil {
		return nil, err
	}
	if o.Protocol == proto.Stealth {
		if _, _, err := net.SplitHostPort(o.StealthHandshake); err != nil {
			return nil, fmt.Errorf("stealth handshake server must be host:port: %w", err)
		}
	}
	s := &server{opts: o, log: slog.With("protocol", string(o.Protocol))}
	if o.Protocol.TLS() {
		var err error
		if s.tls, err = tlsutil.ServerConfig(o.CertPath, o.KeyPath, alpn(o.Protocol)...); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// NewClient returns a Client for the given options.
func NewClient(o ClientOptions) (Client, error) {
	if _, err := proto.Parse(string(o.Protocol)); err != nil {
		return nil, err
	}
	if o.Protocol == proto.Stealth && o.StealthSNI == "" {
		return nil, errors.New("stealth requires a server name (SNI)")
	}
	c := &client{opts: o, log: slog.With("protocol", string(o.Protocol))}
	if o.Protocol.TLS() {
		var err error
		if c.tls, err = tlsutil.PinnedClientConfig(o.CertPath, alpn(o.Protocol)...); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func alpn(p proto.Name) []string {
	if p == proto.QUIC {
		return []string{quicALPN}
	}
	return nil
}

// deadlineConn is the connection type the handshake and smux layers need.
type deadlineConn interface {
	io.ReadWriteCloser
	SetDeadline(time.Time) error
}

// handshake runs fn on c under the handshake timeout.
func handshake(c deadlineConn, fn func(io.ReadWriter) error) error {
	if err := c.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return err
	}
	if err := fn(c); err != nil {
		return err
	}
	return c.SetDeadline(time.Time{})
}

func serverHandshake(c deadlineConn, token string) error {
	return handshake(c, func(rw io.ReadWriter) error {
		_, err := auth.ServerHandshake(rw, token)
		return err
	})
}

func clientHandshake(c deadlineConn, token string) error {
	return handshake(c, func(rw io.ReadWriter) error {
		_, err := auth.ClientHandshake(rw, token)
		return err
	})
}

func muxConfig() *smux.Config {
	cfg := smux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.KeepAliveTimeout = 30 * time.Second
	cfg.MaxFrameSize = 32 * 1024
	cfg.MaxReceiveBuffer = 4 * 1024 * 1024
	cfg.MaxStreamBuffer = 64 * 1024
	return cfg
}

// remoteAddr returns the peer address of v for logging, if it has one.
func remoteAddr(v any) string {
	if r, ok := v.(interface{ RemoteAddr() net.Addr }); ok {
		return r.RemoteAddr().String()
	}
	return "unknown"
}
