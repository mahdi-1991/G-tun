package transport

import (
	"context"
	"errors"
	"sync"

	"github.com/quic-go/quic-go"
	"github.com/xtaci/smux"
)

// muxSession is a multiplexed connection to the server that can open many streams.
type muxSession interface {
	open(ctx context.Context) (Stream, error)
	isClosed() bool
	close()
}

// Dialer opens streams to the server.
type Dialer interface {
	Dial(ctx context.Context) (Stream, error)
	Close() error
}

var errDialerClosed = errors.New("transport: dialer closed")

// dialerFunc adapts a function to a Dialer that has nothing to close.
type dialerFunc func(ctx context.Context) (Stream, error)

func (f dialerFunc) Dial(ctx context.Context) (Stream, error) { return f(ctx) }
func (dialerFunc) Close() error                               { return nil }

// sessionDialer opens streams on one shared muxSession and transparently
// replaces the session when it dies. Only one goroutine connects at a time;
// the others wait for its result instead of dialing in parallel.
type sessionDialer struct {
	connect func(ctx context.Context) (muxSession, error)

	mu     sync.Mutex
	cur    muxSession
	closed bool
}

func newSessionDialer(connect func(context.Context) (muxSession, error)) *sessionDialer {
	return &sessionDialer{connect: connect}
}

func (d *sessionDialer) Dial(ctx context.Context) (Stream, error) {
	var lastErr error
	for range 2 { // A stale session is detected on first use; retry once on a fresh one.
		sess, err := d.session(ctx)
		if err != nil {
			return nil, err
		}
		stream, err := sess.open(ctx)
		if err == nil {
			return stream, nil
		}
		lastErr = err
		d.discard(sess)
	}
	return nil, lastErr
}

func (d *sessionDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.cur != nil {
		d.cur.close()
		d.cur = nil
	}
	return nil
}

func (d *sessionDialer) session(ctx context.Context) (muxSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, errDialerClosed
	}
	if d.cur != nil && !d.cur.isClosed() {
		return d.cur, nil
	}
	if d.cur != nil {
		d.cur.close()
		d.cur = nil
	}
	sess, err := d.connect(ctx)
	if err != nil {
		return nil, err
	}
	d.cur = sess
	return sess, nil
}

func (d *sessionDialer) discard(sess muxSession) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cur == sess {
		d.cur.close()
		d.cur = nil
	}
}

// smuxSession is a muxSession backed by smux.
type smuxSession struct{ sess *smux.Session }

func (s *smuxSession) open(context.Context) (Stream, error) {
	st, err := s.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	return muxStream{st}, nil
}

func (s *smuxSession) isClosed() bool { return s.sess.IsClosed() }
func (s *smuxSession) close()         { _ = s.sess.Close() }

// quicSession is a muxSession backed by a QUIC connection.
type quicSession struct{ conn quic.Connection }

func (s *quicSession) open(ctx context.Context) (Stream, error) {
	st, err := s.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	// QUIC announces a stream to the peer only once data is sent on it. Send
	// a marker byte so that servers which speak first (SSH, SMTP, ...) are
	// told about the stream straight away.
	if _, err := st.Write([]byte{quicStreamMarker}); err != nil {
		st.CancelRead(0)
		_ = st.Close()
		return nil, err
	}
	return quicStream{st}, nil
}

func (s *quicSession) isClosed() bool { return s.conn.Context().Err() != nil }
func (s *quicSession) close()         { _ = s.conn.CloseWithError(0, "") }

// quicStream makes a QUIC stream behave like a TCP connection: CloseWrite
// finishes only the sending side, and Close tears down both directions so the
// stream's resources are released.
type quicStream struct{ quic.Stream }

func (s quicStream) CloseWrite() error { return s.Stream.Close() }

func (s quicStream) Close() error {
	s.Stream.CancelRead(0)
	return s.Stream.Close()
}

// muxStream wraps an smux stream. It deliberately hides CloseWrite: when both
// directions of an smux stream have finished, smux discards any received data
// the application has not read yet, so half-close can lose the tail of a
// reply. Without CloseWrite, netutil.Pipe closes the whole stream as soon as
// either side is done, which never loses data that was already sent.
//
// It also does not embed *smux.Stream, so io.Copy cannot bypass this wrapper
// through the stream's own WriteTo.
type muxStream struct{ st *smux.Stream }

func (s muxStream) Read(p []byte) (int, error)  { return s.st.Read(p) }
func (s muxStream) Write(p []byte) (int, error) { return s.st.Write(p) }
func (s muxStream) Close() error                { return s.st.Close() }
