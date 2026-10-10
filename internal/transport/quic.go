package transport

import (
	"time"

	"github.com/quic-go/quic-go"
)

// quicStreamMarker is the first byte of every tunnel stream. It makes the
// server aware of the stream before the application sends any data.
const quicStreamMarker = 0x01

func quicConfig() *quic.Config {
	return &quic.Config{
		KeepAlivePeriod:    10 * time.Second,
		MaxIdleTimeout:     5 * time.Minute,
		MaxIncomingStreams: 10000,
	}
}
