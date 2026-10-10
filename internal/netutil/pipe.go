package netutil

import (
	"io"
	"sync"
)

const copyBufferSize = 32 * 1024

var bufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, copyBufferSize)
		return &b
	},
}

type closeWriter interface {
	CloseWrite() error
}

// Pipe copies data between a and b in both directions and closes both when
// the transfer is over. When one direction finishes, the write side of its
// destination is shut down (if supported) so the peer sees a clean EOF while
// the opposite direction can still drain.
func Pipe(a, b io.ReadWriteCloser) {
	defer a.Close()
	defer b.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		copyHalf(a, b)
	}()
	copyHalf(b, a)
	<-done
}

func copyHalf(dst io.WriteCloser, src io.Reader) {
	buf := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(buf)

	_, _ = io.CopyBuffer(dst, src, *buf)
	if cw, ok := dst.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = dst.Close()
}
