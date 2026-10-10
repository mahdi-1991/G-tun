package transport

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
)

type fakeSession struct {
	openErr error
	closed  bool
}

func (s *fakeSession) open(context.Context) (Stream, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return nopStream{}, nil
}
func (s *fakeSession) isClosed() bool { return s.closed }
func (s *fakeSession) close()         { s.closed = true }

type nopStream struct{}

func (nopStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopStream) Write(p []byte) (int, error) { return len(p), nil }
func (nopStream) Close() error                { return nil }

func TestSessionDialerReusesLiveSession(t *testing.T) {
	var connects int
	d := newSessionDialer(func(context.Context) (muxSession, error) {
		connects++
		return &fakeSession{}, nil
	})
	for range 3 {
		if _, err := d.Dial(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if connects != 1 {
		t.Errorf("connected %d times, want 1", connects)
	}
}

func TestSessionDialerReplacesDeadSession(t *testing.T) {
	var sessions []*fakeSession
	d := newSessionDialer(func(context.Context) (muxSession, error) {
		s := &fakeSession{}
		sessions = append(sessions, s)
		return s, nil
	})
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The session dies; the next Dial must notice and reconnect.
	sessions[0].closed = true
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Errorf("connected %d times, want 2", len(sessions))
	}
}

func TestSessionDialerRetriesWhenOpenFails(t *testing.T) {
	var sessions []*fakeSession
	d := newSessionDialer(func(context.Context) (muxSession, error) {
		s := &fakeSession{}
		if len(sessions) == 0 {
			s.openErr = errors.New("stale session") // Looks alive but cannot open streams.
		}
		sessions = append(sessions, s)
		return s, nil
	})
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatalf("Dial did not recover with a fresh session: %v", err)
	}
	if !sessions[0].closed {
		t.Error("the failing session was not closed")
	}
}

func TestSessionDialerReportsConnectError(t *testing.T) {
	want := errors.New("server unreachable")
	d := newSessionDialer(func(context.Context) (muxSession, error) { return nil, want })
	if _, err := d.Dial(context.Background()); !errors.Is(err, want) {
		t.Errorf("error = %v, want %v", err, want)
	}
}

func TestSessionDialerClose(t *testing.T) {
	s := &fakeSession{}
	d := newSessionDialer(func(context.Context) (muxSession, error) { return s, nil })
	if _, err := d.Dial(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if !s.closed {
		t.Error("Close did not close the session")
	}
	if _, err := d.Dial(context.Background()); !errors.Is(err, errDialerClosed) {
		t.Errorf("Dial after Close: error = %v, want errDialerClosed", err)
	}
}

// Many goroutines dialing at once must share a single connection attempt.
func TestSessionDialerSingleFlight(t *testing.T) {
	var mu sync.Mutex
	var connects int
	d := newSessionDialer(func(context.Context) (muxSession, error) {
		mu.Lock()
		connects++
		mu.Unlock()
		return &fakeSession{}, nil
	})
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Dial(context.Background())
		}()
	}
	wg.Wait()
	if connects != 1 {
		t.Errorf("connected %d times, want 1", connects)
	}
}
