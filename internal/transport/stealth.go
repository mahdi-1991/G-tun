package transport

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/mahdi-1991/G-tun/internal/netutil"
	utls "github.com/metacubex/utls"
	shadowtls "github.com/sagernet/sing-shadowtls"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// The stealth transport is ShadowTLS v3.
//
// The client performs a genuine TLS 1.3 handshake with a real, popular web
// site; the server relays that handshake to the site, so an observer, or an
// active prober that connects to the server, sees the real site's certificate
// and behaviour. The client proves it knows the token inside the TLS
// ClientHello's session ID. Only after that does the connection switch to
// HMAC-authenticated frames that carry the tunnel's smux session.
//
// The client's ClientHello imitates Chrome (uTLS), so the handshake also looks
// like a browser at the TLS fingerprint level.

const (
	stealthUser          = "gtun"
	stealthProtocolLevel = 3
	// A stealth handshake crosses the tunnel to a third-party site and back,
	// so it gets more time than a plain handshake.
	stealthDialTimeout = 2 * handshakeTimeout
)

// stealthHandler receives each connection that proved it knows the token.
type stealthHandler struct{ serve func(net.Conn) }

func (h stealthHandler) NewConnectionEx(_ context.Context, conn net.Conn, _, _ M.Socksaddr, onClose N.CloseHandlerFunc) {
	h.serve(conn)
	if onClose != nil {
		onClose(nil)
	}
}

func (s *server) serveStealth(ctx context.Context) error {
	service, err := shadowtls.NewService(shadowtls.ServiceConfig{
		Version: stealthProtocolLevel,
		Users:   []shadowtls.User{{Name: stealthUser, Password: s.opts.Token}},
		Handshake: shadowtls.HandshakeConfig{
			Server: M.ParseSocksaddr(s.opts.StealthHandshake),
			Dialer: N.SystemDialer,
		},
		StrictMode: true, // Require TLS 1.3 so the handshake looks modern and authentic.
		Handler:    stealthHandler{serve: func(c net.Conn) { s.handleConn(ctx, c) }},
		Logger:     logger.NOP(),
	})
	if err != nil {
		return fmt.Errorf("create stealth service: %w", err)
	}

	lc := net.ListenConfig{KeepAlive: keepAlive}
	ln, err := lc.Listen(ctx, "tcp", s.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()

	s.log.Info("data listener started",
		"addr", ln.Addr().String(), "handshake_site", s.opts.StealthHandshake)
	return netutil.AcceptLoop(ctx, ln, func(conn net.Conn) {
		source := M.SocksaddrFromNet(conn.RemoteAddr())
		destination := M.SocksaddrFromNet(conn.LocalAddr())
		// Connections without valid credentials are relayed to the real site
		// until either side closes, exactly like a transparent proxy.
		if err := service.NewConnection(ctx, conn, source, destination, nil); err != nil {
			s.log.Debug("stealth connection ended", "remote", source.String(), "err", err)
			_ = conn.Close()
		}
	})
}

func (c *client) dialStealth(ctx context.Context, dialer *net.Dialer) (deadlineConn, error) {
	raw, err := dialer.DialContext(ctx, "tcp", c.opts.ServerAddr)
	if err != nil {
		return nil, fmt.Errorf("dial tcp: %w", err)
	}

	client, err := shadowtls.NewClient(shadowtls.ClientConfig{
		Version:      stealthProtocolLevel,
		Password:     c.opts.Token,
		StrictMode:   true,
		TLSHandshake: c.chromeHandshake,
		Logger:       logger.NOP(),
	})
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("create stealth client: %w", err)
	}

	_ = raw.SetDeadline(time.Now().Add(stealthDialTimeout))
	conn, err := client.DialContextConn(ctx, raw)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("stealth handshake: %w", err)
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

// chromeHandshake performs the TLS handshake with a Chrome-like ClientHello
// and verifies the handshake site's real certificate.
func (c *client) chromeHandshake(ctx context.Context, conn net.Conn, sessionID shadowtls.TLSSessionIDGeneratorFunc) error {
	config := &utls.Config{
		ServerName:         c.opts.StealthSNI,
		RootCAs:            c.opts.StealthRootCAs,
		MinVersion:         utls.VersionTLS12,
		SessionIDGenerator: sessionID,
	}
	return utls.UClient(conn, config, utls.HelloChrome_Auto).HandshakeContext(ctx)
}
