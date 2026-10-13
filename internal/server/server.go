// Package server implements g-tun-server: a control listener that
// authenticates clients and tells them which transport to run, plus the data
// listener of that transport.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/mahdi-1991/G-tun/internal/auth"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/control"
	"github.com/mahdi-1991/G-tun/internal/netutil"
	"github.com/mahdi-1991/G-tun/internal/proto"
	"github.com/mahdi-1991/G-tun/internal/transport"
)

const (
	controlHandshakeTimeout = 10 * time.Second
	keepAlive               = 15 * time.Second
)

// Run serves until ctx is done or either listener fails.
func Run(ctx context.Context, cfg *config.Server) error {
	data, err := transport.NewServer(transport.ServerOptions{
		Protocol:   cfg.Protocol,
		ListenAddr: net.JoinHostPort("", cfg.DataPort),
		Target:     cfg.TargetAddress,
		Token:      cfg.Token,
		CertPath:   cfg.TLSCertPath,
		KeyPath:    cfg.TLSKeyPath,
		KCP:        *cfg.KCP,

		StealthHandshake: cfg.StealthHandshakeServer,
	})
	if err != nil {
		return err
	}
	slog.Info("server starting",
		"protocol", string(cfg.Protocol),
		"control_port", cfg.ControlPort,
		"data_port", cfg.DataPort,
		"target", cfg.TargetAddress)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The stealth protocol has no control port: a second, unusual connection
	// would only give a firewall something to fingerprint.
	listeners := 1
	errs := make(chan error, 2)
	go func() { errs <- data.Serve(ctx) }()
	if cfg.Protocol != proto.Stealth {
		listeners++
		go func() { errs <- serveControl(ctx, cfg) }()
	}

	first := <-errs
	cancel()
	for range listeners - 1 {
		first = errors.Join(first, <-errs)
	}
	return first
}

func serveControl(ctx context.Context, cfg *config.Server) error {
	lc := net.ListenConfig{KeepAlive: keepAlive}
	ln, err := lc.Listen(ctx, "tcp", net.JoinHostPort("", cfg.ControlPort))
	if err != nil {
		return fmt.Errorf("listen on control port: %w", err)
	}
	defer context.AfterFunc(ctx, func() { _ = ln.Close() })()

	slog.Info("control listener ready", "addr", ln.Addr().String())
	announce := control.Config{Protocol: cfg.Protocol, Port: cfg.DataPort}
	if cfg.Protocol == proto.UTCPMux {
		announce.KCP = cfg.KCP
	}
	return netutil.AcceptLoop(ctx, ln, func(conn net.Conn) { handleControl(conn, cfg.Token, announce) })
}

// handleControl authenticates one client, sends it the transport settings and
// then holds the connection open until the client goes away.
func handleControl(conn net.Conn, token string, announce control.Config) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()

	if err := conn.SetDeadline(time.Now().Add(controlHandshakeTimeout)); err != nil {
		return
	}
	channel, err := auth.ServerHandshake(conn, token)
	if err != nil {
		slog.Warn("control connection rejected", "remote", remote, "err", err)
		return
	}
	if err := control.Send(conn, token, channel, announce); err != nil {
		slog.Warn("send transport settings failed", "remote", remote, "err", err)
		return
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return
	}

	slog.Info("client connected", "remote", remote)
	_, _ = io.Copy(io.Discard, conn) // Blocks until the client disconnects.
	slog.Info("client disconnected", "remote", remote)
}
