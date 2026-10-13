package client

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"

	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/control"
	"github.com/mahdi-1991/G-tun/internal/transport"
)

// runner owns the currently running transport. It restarts the transport
// only when the server announces different settings, so a control
// reconnection does not interrupt local connections.
type runner struct {
	ctx context.Context
	cfg *config.Client

	mu      sync.Mutex
	current *running
}

type running struct {
	settings control.Config
	cancel   context.CancelFunc
	done     chan struct{}
}

func newRunner(ctx context.Context, cfg *config.Client) *runner {
	return &runner{ctx: ctx, cfg: cfg}
}

// apply makes the announced transport the running one.
func (r *runner) apply(settings control.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.current != nil && r.current.settings.Equal(settings) {
		select {
		case <-r.current.done: // Gave up earlier; fall through and start again.
		default:
			return nil
		}
	}
	r.stopLocked()

	opts := transport.ClientOptions{
		Protocol:   settings.Protocol,
		ServerAddr: net.JoinHostPort(r.cfg.RemoteHost, settings.Port),
		ListenAddr: r.cfg.ListenAddress,
		Token:      r.cfg.Token,
		CertPath:   r.cfg.TLSCertPath,
		KCP:        config.DefaultKCP(),

		StealthSNI: r.cfg.StealthSNI,
	}
	if settings.KCP != nil {
		opts.KCP = *settings.KCP
	}
	if r.cfg.StealthCAFile != "" {
		pool, err := loadCertPool(r.cfg.StealthCAFile)
		if err != nil {
			return err
		}
		opts.StealthRootCAs = pool
	}
	tr, err := transport.NewClient(opts)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(r.ctx)
	run := &running{settings: settings, cancel: cancel, done: make(chan struct{})}
	r.current = run
	go func() {
		defer close(run.done)
		supervise(ctx, tr)
	}()
	return nil
}

func (r *runner) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked()
}

func (r *runner) stopLocked() {
	if r.current == nil {
		return
	}
	r.current.cancel()
	<-r.current.done
	r.current = nil
}

// supervise runs tr, restarting it with backoff if it fails, until ctx is done.
func supervise(ctx context.Context, tr transport.Client) {
	backoff := minBackoff
	for {
		err := tr.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Error("transport stopped", "err", err, "retry_in", backoff.String())
		if !sleep(ctx, backoff) {
			return
		}
		backoff = min(2*backoff, maxBackoff)
	}
}

func loadCertPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read stealth_ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("stealth_ca_file %s contains no certificates", path)
	}
	return pool, nil
}
