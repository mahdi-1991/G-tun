// Package proto defines the transport protocols supported by G-Tun.
package proto

import "fmt"

// Name identifies a transport protocol.
type Name string

// Supported transport protocols.
const (
	TCP     Name = "tcp"
	UDP     Name = "udp"
	WS      Name = "ws"
	WSS     Name = "wss"
	TCPMux  Name = "tcpmux"
	WSMux   Name = "wsmux"
	WSSMux  Name = "wssmux"
	UTCPMux Name = "utcpmux"
	QUIC    Name = "quic"
	// Stealth is ShadowTLS v3 with smux: a real TLS handshake with a real web
	// site, then authenticated, multiplexed traffic. Built for heavy filtering.
	Stealth Name = "stealth"
)

var supported = []Name{TCP, UDP, WS, WSS, TCPMux, WSMux, WSSMux, UTCPMux, QUIC, Stealth}

// Parse returns s as a Name, or an error if s is not a supported protocol.
func Parse(s string) (Name, error) {
	for _, n := range supported {
		if string(n) == s {
			return n, nil
		}
	}
	return "", fmt.Errorf("unknown protocol %q (supported: %v)", s, supported)
}

// Mux reports whether the protocol multiplexes streams with smux.
func (n Name) Mux() bool {
	switch n {
	case TCPMux, WSMux, WSSMux, UTCPMux, Stealth:
		return true
	}
	return false
}

// WebSocket reports whether the protocol is carried over WebSocket.
func (n Name) WebSocket() bool {
	switch n {
	case WS, WSS, WSMux, WSSMux:
		return true
	}
	return false
}

// TLS reports whether the protocol requires a TLS certificate.
func (n Name) TLS() bool {
	switch n {
	case WSS, WSSMux, QUIC:
		return true
	}
	return false
}

// UDPBased reports whether the protocol's data port is a UDP port.
func (n Name) UDPBased() bool {
	switch n {
	case UDP, UTCPMux, QUIC:
		return true
	}
	return false
}
