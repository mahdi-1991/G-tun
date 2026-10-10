package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/url"

	"github.com/gorilla/websocket"
	"github.com/mahdi-1991/G-tun/internal/netutil"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/quic-go/quic-go"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

type client struct {
	opts ClientOptions
	tls  *tls.Config // pinned server certificate; nil unless the protocol uses TLS
	log  *slog.Logger
}

func (c *client) Run(ctx context.Context) error {
	if c.opts.Protocol == proto.UDP {
		return c.runUDP(ctx)
	}

	dialer := c.newDialer()
	defer dialer.Close()

	lc := net.ListenConfig{KeepAlive: keepAlive}
	ln, err := lc.Listen(ctx, "tcp", c.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()

	c.log.Info("ready, listening locally", "addr", ln.Addr().String(), "server", c.opts.ServerAddr)
	return netutil.AcceptLoop(ctx, ln, func(local net.Conn) {
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout+handshakeTimeout)
		stream, err := dialer.Dial(dialCtx)
		cancel()
		if err != nil {
			c.log.Warn("open tunnel stream failed", "err", err)
			_ = local.Close()
			return
		}
		netutil.Pipe(local, stream)
	})
}

// newDialer returns the Dialer for the configured stream protocol.
func (c *client) newDialer() Dialer {
	switch {
	case c.opts.Protocol == proto.QUIC:
		return newSessionDialer(c.connectQUIC)
	case c.opts.Protocol.Mux():
		return newSessionDialer(c.connectMux)
	default:
		return dialerFunc(func(ctx context.Context) (Stream, error) {
			conn, err := c.dialAuthenticated(ctx)
			if err != nil {
				return nil, err
			}
			return conn, nil
		})
	}
}

// dialAuthenticated opens a carrier connection and authenticates it.
func (c *client) dialAuthenticated(ctx context.Context) (deadlineConn, error) {
	conn, err := c.dialCarrier(ctx)
	if err != nil {
		return nil, err
	}
	if err := clientHandshake(conn, c.opts.Token); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	return conn, nil
}

func (c *client) dialCarrier(ctx context.Context) (deadlineConn, error) {
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}

	switch {
	case c.opts.Protocol.WebSocket():
		scheme := "ws"
		if c.opts.Protocol.TLS() {
			scheme = "wss"
		}
		u := url.URL{Scheme: scheme, Host: c.opts.ServerAddr, Path: "/" + string(c.opts.Protocol)}
		wsDialer := &websocket.Dialer{
			NetDialContext:   dialer.DialContext,
			HandshakeTimeout: dialTimeout,
			TLSClientConfig:  c.tls,
		}
		ws, _, err := wsDialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", u.Redacted(), err)
		}
		return newWSConn(ws), nil

	case c.opts.Protocol == proto.UTCPMux:
		block, err := kcpBlockCrypt(c.opts.Token)
		if err != nil {
			return nil, err
		}
		k := c.opts.KCP
		sess, err := kcp.DialWithOptions(c.opts.ServerAddr, block, k.DataShards, k.ParityShards)
		if err != nil {
			return nil, fmt.Errorf("dial kcp: %w", err)
		}
		tuneKCP(sess, k)
		return sess, nil

	default:
		conn, err := dialer.DialContext(ctx, "tcp", c.opts.ServerAddr)
		if err != nil {
			return nil, fmt.Errorf("dial tcp: %w", err)
		}
		return conn, nil
	}
}

func (c *client) connectMux(ctx context.Context) (muxSession, error) {
	conn, err := c.dialAuthenticated(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := smux.Client(conn, muxConfig())
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("start smux session: %w", err)
	}
	return &smuxSession{sess}, nil
}

func (c *client) connectQUIC(ctx context.Context) (muxSession, error) {
	conn, err := quic.DialAddr(ctx, c.opts.ServerAddr, c.tls, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("dial quic: %w", err)
	}
	raw, err := conn.OpenStreamSync(ctx)
	if err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("open auth stream: %w", err)
	}
	authStream := quicStream{raw}
	if err := clientHandshake(authStream, c.opts.Token); err != nil {
		_ = conn.CloseWithError(0, "")
		return nil, fmt.Errorf("authenticate: %w", err)
	}
	_ = authStream.Close()
	return &quicSession{conn}, nil
}
