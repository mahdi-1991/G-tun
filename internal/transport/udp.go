package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mahdi-1991/G-tun/internal/auth"
)

const (
	udpIdleTimeout   = 3 * time.Minute
	udpSweepInterval = time.Minute
	maxDatagramSize  = 65535
)

// datagramFilter transforms a datagram in transit. It returns false to drop it.
type datagramFilter func(pkt []byte) ([]byte, bool)

// udpRelay forwards datagrams between a listening socket and one upstream
// socket per remote peer, preserving datagram boundaries. Each datagram that
// crosses the tunnel carries an authentication tag, which the up and down
// filters add or verify.
type udpRelay struct {
	listener *net.UDPConn
	dial     func() (*net.UDPConn, error)
	up       datagramFilter // listener -> upstream
	down     datagramFilter // upstream -> listener
	log      *slog.Logger

	mu       sync.Mutex
	sessions map[netip.AddrPort]*udpSession
}

type udpSession struct {
	peer     netip.AddrPort
	upstream *net.UDPConn
	lastSeen atomic.Int64 // unix nanoseconds
}

func (s *udpSession) touch() { s.lastSeen.Store(time.Now().UnixNano()) }

func (r *udpRelay) run(ctx context.Context) error {
	r.sessions = make(map[netip.AddrPort]*udpSession)
	defer context.AfterFunc(ctx, func() { _ = r.listener.Close() })()
	defer r.closeAll()
	go r.sweep(ctx)

	buf := make([]byte, maxDatagramSize)
	for {
		n, peer, err := r.listener.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read datagram: %w", err)
		}
		payload, ok := r.up(buf[:n])
		if !ok {
			r.log.Debug("dropped unauthenticated datagram", "peer", peer.String())
			continue
		}
		sess, err := r.session(peer)
		if err != nil {
			r.log.Warn("create udp session failed", "peer", peer.String(), "err", err)
			continue
		}
		sess.touch()
		if _, err := sess.upstream.Write(payload); err != nil {
			r.log.Debug("forward datagram failed", "peer", peer.String(), "err", err)
		}
	}
}

func (r *udpRelay) session(peer netip.AddrPort) (*udpSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[peer]; ok {
		return s, nil
	}
	upstream, err := r.dial()
	if err != nil {
		return nil, err
	}
	s := &udpSession{peer: peer, upstream: upstream}
	s.touch()
	r.sessions[peer] = s
	go r.pump(s)
	return s, nil
}

// pump copies datagrams from the upstream socket back to the peer.
func (r *udpRelay) pump(s *udpSession) {
	defer r.remove(s)
	buf := make([]byte, maxDatagramSize)
	for {
		n, err := s.upstream.Read(buf)
		if err != nil {
			return
		}
		out, ok := r.down(buf[:n])
		if !ok {
			continue
		}
		s.touch()
		if _, err := r.listener.WriteToUDPAddrPort(out, s.peer); err != nil {
			r.log.Debug("reply datagram failed", "peer", s.peer.String(), "err", err)
		}
	}
}

// remove forgets s, but only if it has not already been replaced by a newer
// session for the same peer.
func (r *udpRelay) remove(s *udpSession) {
	r.mu.Lock()
	if r.sessions[s.peer] == s {
		delete(r.sessions, s.peer)
	}
	r.mu.Unlock()
	_ = s.upstream.Close()
}

func (r *udpRelay) sweep(ctx context.Context) {
	ticker := time.NewTicker(udpSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		cutoff := time.Now().Add(-udpIdleTimeout).UnixNano()
		var idle []*udpSession
		r.mu.Lock()
		for peer, s := range r.sessions {
			if s.lastSeen.Load() < cutoff {
				delete(r.sessions, peer)
				idle = append(idle, s)
			}
		}
		r.mu.Unlock()
		for _, s := range idle {
			r.log.Debug("udp session expired", "peer", s.peer.String())
			_ = s.upstream.Close() // Unblocks pump, which then exits.
		}
	}
}

func (r *udpRelay) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for peer, s := range r.sessions {
		delete(r.sessions, peer)
		_ = s.upstream.Close()
	}
}

func listenUDP(ctx context.Context, addr string) (*net.UDPConn, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	return pc.(*net.UDPConn), nil
}

// dialUDPFunc returns a function that opens a connected UDP socket to addr,
// resolving the address once up front.
func dialUDPFunc(addr string) (func() (*net.UDPConn, error), error) {
	remote, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", addr, err)
	}
	return func() (*net.UDPConn, error) { return net.DialUDP("udp", nil, remote) }, nil
}

func (s *server) serveUDP(ctx context.Context) error {
	dial, err := dialUDPFunc(s.opts.Target)
	if err != nil {
		return err
	}
	conn, err := listenUDP(ctx, s.opts.ListenAddr)
	if err != nil {
		return err
	}
	token := s.opts.Token
	relay := &udpRelay{
		listener: conn,
		dial:     dial,
		up: func(pkt []byte) ([]byte, bool) {
			return auth.OpenDatagram(token, auth.ClientToServer, pkt)
		},
		down: func(pkt []byte) ([]byte, bool) {
			return auth.SealDatagram(token, auth.ServerToClient, pkt), true
		},
		log: s.log,
	}
	s.log.Info("data listener started", "addr", conn.LocalAddr().String())
	return relay.run(ctx)
}

func (c *client) runUDP(ctx context.Context) error {
	dial, err := dialUDPFunc(c.opts.ServerAddr)
	if err != nil {
		return err
	}
	conn, err := listenUDP(ctx, c.opts.ListenAddr)
	if err != nil {
		return err
	}
	token := c.opts.Token
	relay := &udpRelay{
		listener: conn,
		dial:     dial,
		up: func(pkt []byte) ([]byte, bool) {
			return auth.SealDatagram(token, auth.ClientToServer, pkt), true
		},
		down: func(pkt []byte) ([]byte, bool) {
			return auth.OpenDatagram(token, auth.ServerToClient, pkt)
		},
		log: c.log,
	}
	c.log.Info("ready, listening locally", "addr", conn.LocalAddr().String(), "server", c.opts.ServerAddr)
	return relay.run(ctx)
}
