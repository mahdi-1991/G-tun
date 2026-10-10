// Package control defines the message the server sends to a client over the
// authenticated control channel.
package control

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"github.com/mahdi-1991/G-tun/internal/auth"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
)

const (
	maxBodySize = 4096
	labelConfig = "gtun/v1/config"
)

// Config tells a client which transport to run.
type Config struct {
	Protocol proto.Name  `json:"protocol"`
	Port     string      `json:"port"`
	KCP      *config.KCP `json:"kcp,omitempty"`
}

// Equal reports whether c and o describe the same transport.
func (c Config) Equal(o Config) bool {
	if c.Protocol != o.Protocol || c.Port != o.Port {
		return false
	}
	if (c.KCP == nil) != (o.KCP == nil) {
		return false
	}
	return c.KCP == nil || *c.KCP == *o.KCP
}

// Send writes cfg to w. The message is authenticated with the token and bound
// to channel, so a man in the middle cannot alter or replay it.
//
// Wire format: uint16 length | JSON body | HMAC tag.
func Send(w io.Writer, token string, channel auth.Channel, cfg Config) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("control: encode config: %w", err)
	}
	if len(body) > maxBodySize {
		return fmt.Errorf("control: config too large (%d bytes)", len(body))
	}
	frame := make([]byte, 2, 2+len(body)+auth.TagSize)
	binary.BigEndian.PutUint16(frame, uint16(len(body)))
	frame = append(frame, body...)
	frame = append(frame, auth.Sign(token, labelConfig, channel, body)...)
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("control: send config: %w", err)
	}
	return nil
}

// Receive reads and verifies a message written by Send.
func Receive(r io.Reader, token string, channel auth.Channel) (Config, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Config{}, fmt.Errorf("control: read header: %w", err)
	}
	size := int(binary.BigEndian.Uint16(header[:]))
	if size > maxBodySize {
		return Config{}, fmt.Errorf("control: message too large (%d bytes)", size)
	}
	buf := make([]byte, size+auth.TagSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Config{}, fmt.Errorf("control: read message: %w", err)
	}
	body, tag := buf[:size], buf[size:]
	if !auth.Verify(token, labelConfig, tag, channel, body) {
		return Config{}, auth.ErrAuthFailed
	}

	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return Config{}, fmt.Errorf("control: decode config: %w", err)
	}
	if _, err := proto.Parse(string(cfg.Protocol)); err != nil {
		return Config{}, fmt.Errorf("control: %w", err)
	}
	if cfg.KCP != nil {
		if err := cfg.KCP.Validate(); err != nil {
			return Config{}, fmt.Errorf("control: %w", err)
		}
	}
	return cfg, nil
}
