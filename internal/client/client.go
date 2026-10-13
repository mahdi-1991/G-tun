// Package client implements g-tun-client: it keeps an authenticated control
// connection to the server and runs whichever transport the server announces.
package client

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
)

const (
	controlDialTimeout      = 10 * time.Second
	controlHandshakeTimeout = 15 * time.Second
	keepAlive               = 15 * time.Second

	minBackoff = 3 * time.Second
	maxBackoff = 60 * time.Second
)

// Run keeps the tunnel up until ctx is done. It reconnects with exponential
// backoff whenever the control connection fails or is lost. The transport
// keeps serving local connections while the control connection is down.
func Run(ctx context.Context, cfg *config.Client) error {
	r := newRunner(ctx, cfg)
	defer r.stop()

	if cfg.Protocol != "" {
		return runStatic(ctx, cfg, r)
	}

	backoff := minBackoff
	for {
		established, err := runControl(ctx, cfg, r)
		if ctx.Err() != nil {
			return nil
		}
		if established {
			backoff = minBackoff
		}
		slog.Warn("control connection lost", "err", err, "retry_in", backoff.String())
		if !sleep(ctx, backoff) {
			return nil
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

// runStatic runs the transport named in the configuration and never contacts
// a control port. The transport restarts itself with backoff if it fails.
func runStatic(ctx context.Context, cfg *config.Client, r *runner) error {
	slog.Info("static mode: no control connection", "protocol", string(cfg.Protocol), "port", cfg.DataPort)
	if err := r.apply(control.Config{Protocol: cfg.Protocol, Port: cfg.DataPort}); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// runControl holds one control connection. It reports whether the server
// accepted the client, which tells the caller whether to reset its backoff.
func runControl(ctx context.Context, cfg *config.Client, r *runner) (established bool, err error) {
	dialer := net.Dialer{Timeout: controlDialTimeout, KeepAlive: keepAlive}
	slog.Info("connecting to control server", "addr", cfg.ControlAddress)
	conn, err := dialer.DialContext(ctx, "tcp", cfg.ControlAddress)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	defer context.AfterFunc(ctx, func() { _ = conn.Close() })()

	if err := conn.SetDeadline(time.Now().Add(controlHandshakeTimeout)); err != nil {
		return false, err
	}
	channel, err := auth.ClientHandshake(conn, cfg.Token)
	if err != nil {
		return false, fmt.Errorf("authenticate with server: %w", err)
	}
	announced, err := control.Receive(conn, cfg.Token, channel)
	if err != nil {
		return false, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return false, err
	}

	slog.Info("authenticated", "protocol", string(announced.Protocol), "port", announced.Port)
	if err := r.apply(announced); err != nil {
		return false, err
	}

	// Block until the server closes the connection or the network breaks.
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return true, err
	}
	return true, errors.New("server closed the control connection")
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
