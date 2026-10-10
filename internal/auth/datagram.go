package auth

import (
	"crypto/hmac"
	"crypto/sha256"
)

// DatagramOverhead is the number of bytes SealDatagram adds to a payload.
const DatagramOverhead = 16

// Direction tells which way a datagram travels. It is covered by the tag so a
// datagram cannot be reflected back to its sender.
type Direction byte

// Datagram directions.
const (
	ClientToServer Direction = iota + 1
	ServerToClient
)

// SealDatagram prepends an authentication tag to payload.
func SealDatagram(token string, dir Direction, payload []byte) []byte {
	out := make([]byte, DatagramOverhead+len(payload))
	copy(out, datagramTag(token, dir, payload))
	copy(out[DatagramOverhead:], payload)
	return out
}

// OpenDatagram verifies the tag of pkt and returns its payload. The result
// aliases pkt.
func OpenDatagram(token string, dir Direction, pkt []byte) ([]byte, bool) {
	if len(pkt) < DatagramOverhead {
		return nil, false
	}
	tag, payload := pkt[:DatagramOverhead], pkt[DatagramOverhead:]
	if !hmac.Equal(tag, datagramTag(token, dir, payload)) {
		return nil, false
	}
	return payload, true
}

func datagramTag(token string, dir Direction, payload []byte) []byte {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("gtun/v1/udp"))
	mac.Write([]byte{byte(dir)})
	mac.Write(payload)
	return mac.Sum(nil)[:DatagramOverhead]
}
