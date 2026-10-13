package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/mahdi-1991/G-tun/internal/netutil"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/quic-go/quic-go"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

type server struct {
	opts ServerOptions
	tls  *tls.Config // nil unless the protocol uses TLS
	log  *slog.Logger
}

func (s *server) Serve(ctx context.Context) error {
	switch s.opts.Protocol {
	case proto.TCP, proto.TCPMux:
		return s.serveTCP(ctx)
	case proto.WS, proto.WSS, proto.WSMux, proto.WSSMux:
		return s.serveWebSocket(ctx)
	case proto.UTCPMux:
		return s.serveKCP(ctx)
	case proto.QUIC:
		return s.serveQUIC(ctx)
	case proto.UDP:
		return s.serveUDP(ctx)
	case proto.Stealth:
		return s.serveStealth(ctx)
	}
	return fmt.Errorf("transport: unsupported protocol %q", s.opts.Protocol)
}

func (s *server) serveTCP(ctx context.Context) error {
	lc := net.ListenConfig{KeepAlive: keepAlive}
	ln, err := lc.Listen(ctx, "tcp", s.opts.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()

	s.log.Info("data listener started", "addr", ln.Addr().String())
	return netutil.AcceptLoop(ctx, ln, func(c net.Conn) { s.handleConn(ctx, c) })
}

func (s *server) serveWebSocket(ctx context.Context) error {
	upgrader := &websocket.Upgrader{ReadBufferSize: 32 * 1024, WriteBufferSize: 32 * 1024}
	mux := http.NewServeMux()
	mux.HandleFunc("/"+string(s.opts.Protocol), func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return // Upgrade has already written the HTTP error.
		}
		s.handleConn(ctx, newWSConn(ws))
	})

	srv := &http.Server{
		Addr:              s.opts.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: handshakeTimeout,
		TLSConfig:         s.tls,
		// Hijacking is impossible over HTTP/2, so never negotiate it.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ErrorLog:     slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
	}
	defer context.AfterFunc(ctx, func() { _ = srv.Close() })()

	s.log.Info("data listener started", "addr", s.opts.ListenAddr)
	var err error
	if s.opts.Protocol.TLS() {
		err = srv.ListenAndServeTLS("", "")
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *server) serveKCP(ctx context.Context) error {
	block, err := kcpBlockCrypt(s.opts.Token)
	if err != nil {
		return err
	}
	k := s.opts.KCP
	ln, err := kcp.ListenWithOptions(s.opts.ListenAddr, block, k.DataShards, k.ParityShards)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()

	s.log.Info("data listener started", "addr", s.opts.ListenAddr)
	return netutil.AcceptLoop(ctx, ln, func(c net.Conn) {
		tuneKCP(c.(*kcp.UDPSession), k)
		s.handleConn(ctx, c)
	})
}

func (s *server) serveQUIC(ctx context.Context) error {
	ln, err := quic.ListenAddr(s.opts.ListenAddr, s.tls, quicConfig())
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer ln.Close()

	s.log.Info("data listener started", "addr", s.opts.ListenAddr)
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		go s.serveQUICConn(ctx, conn)
	}
}

// serveQUICConn authenticates a QUIC connection on its first stream and then
// relays every further stream.
func (s *server) serveQUICConn(ctx context.Context, conn quic.Connection) {
	defer conn.CloseWithError(0, "")

	authCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	raw, err := conn.AcceptStream(authCtx)
	cancel()
	if err != nil {
		return
	}
	authStream := quicStream{raw}
	if err := serverHandshake(authStream, s.opts.Token); err != nil {
		s.log.Warn("data connection rejected", "remote", conn.RemoteAddr().String(), "err", err)
		return
	}
	_ = authStream.Close()

	for {
		st, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go s.handleQUICStream(quicStream{st})
	}
}

// handleQUICStream consumes the marker byte sent by the client and relays the stream.
func (s *server) handleQUICStream(st quicStream) {
	var marker [1]byte
	if _, err := io.ReadFull(st, marker[:]); err != nil || marker[0] != quicStreamMarker {
		_ = st.Close()
		return
	}
	s.handleStream(st)
}

// handleConn authenticates a raw connection and then relays it, either as a
// single stream or, for the mux protocols, as many smux streams. The
// connection is closed when ctx is done.
func (s *server) handleConn(ctx context.Context, conn deadlineConn) {
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	if err := serverHandshake(conn, s.opts.Token); err != nil {
		s.log.Warn("data connection rejected", "remote", remoteAddr(conn), "err", err)
		_ = conn.Close()
		return
	}
	if !s.opts.Protocol.Mux() {
		s.handleStream(conn)
		return
	}

	sess, err := smux.Server(conn, muxConfig())
	if err != nil {
		s.log.Warn("smux session failed", "remote", remoteAddr(conn), "err", err)
		_ = conn.Close()
		return
	}
	defer sess.Close()
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return
		}
		go s.handleStream(muxStream{st})
	}
}

// handleStream relays an authenticated stream to the target.
func (s *server) handleStream(st Stream) {
	dialer := net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}
	target, err := dialer.Dial("tcp", s.opts.Target)
	if err != nil {
		s.log.Warn("dial target failed", "target", s.opts.Target, "err", err)
		_ = st.Close()
		return
	}
	netutil.Pipe(st, target)
}
