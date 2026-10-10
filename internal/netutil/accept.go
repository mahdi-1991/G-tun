// Package netutil contains small networking helpers shared by the client and
// the server.
package netutil

import (
	"context"
	"errors"
	"net"
	"time"
)

const (
	minAcceptDelay = 5 * time.Millisecond
	maxAcceptDelay = time.Second
)

// AcceptLoop accepts connections from ln and serves each one in its own
// goroutine until ctx is done or ln is closed. Transient accept errors, such
// as running out of file descriptors, are retried with exponential backoff
// instead of spinning.
func AcceptLoop(ctx context.Context, ln net.Listener, handle func(net.Conn)) error {
	var delay time.Duration
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			delay = min(max(2*delay, minAcceptDelay), maxAcceptDelay)
			select {
			case <-time.After(delay):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		delay = 0
		go handle(conn)
	}
}
