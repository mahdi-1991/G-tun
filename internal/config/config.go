// Package config loads and validates the server and client configuration files.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mahdi-1991/G-tun/internal/proto"
)

const minTokenLength = 16

// KCP holds the tuning parameters of the KCP transport. The JSON keys keep
// the names used by earlier releases so existing configuration files work.
type KCP struct {
	NoDelay      int `json:"NoDelay"`
	Interval     int `json:"Interval"`
	Resend       int `json:"Resend"`
	NoCongestion int `json:"NoCongestion"`
	SndWnd       int `json:"SndWnd"`
	RcvWnd       int `json:"RcvWnd"`
	DataShards   int `json:"DataShards"`
	ParityShards int `json:"ParityShards"`
}

// DefaultKCP returns the KCP parameters used when none are configured.
func DefaultKCP() KCP {
	return KCP{
		NoDelay:      1,
		Interval:     20,
		Resend:       2,
		NoCongestion: 1,
		SndWnd:       4096,
		RcvWnd:       4096,
		DataShards:   10,
		ParityShards: 3,
	}
}

// Validate reports whether the parameters are usable.
func (k KCP) Validate() error {
	switch {
	case k.Interval <= 0:
		return fmt.Errorf("kcp Interval must be positive, got %d", k.Interval)
	case k.SndWnd <= 0 || k.RcvWnd <= 0:
		return fmt.Errorf("kcp SndWnd and RcvWnd must be positive, got %d and %d", k.SndWnd, k.RcvWnd)
	case k.DataShards < 0 || k.ParityShards < 0:
		return fmt.Errorf("kcp DataShards and ParityShards must not be negative")
	}
	return nil
}

// Server is the configuration of g-tun-server.
type Server struct {
	ControlPort   string     `json:"control_port"`
	DataPort      string     `json:"data_port"`
	Protocol      proto.Name `json:"protocol"`
	TargetAddress string     `json:"xray_inbound_address"`
	Token         string     `json:"token"`
	TLSCertPath   string     `json:"tls_cert_path"`
	TLSKeyPath    string     `json:"tls_key_path"`
	// KCP is never nil after LoadServer returns. The server pushes it to
	// clients, so both ends always agree on the parameters.
	KCP *KCP `json:"kcp_config"`
}

// Client is the configuration of g-tun-client.
type Client struct {
	ControlAddress string `json:"control_server_address"`
	RemoteHost     string `json:"remote_server_ip"`
	ListenAddress  string `json:"local_listen_port"`
	Token          string `json:"token"`
	// TLSCertPath is the pinned server certificate. It defaults to cert.pem
	// next to the configuration file.
	TLSCertPath string `json:"tls_cert_path"`
}

// LoadServer reads and validates the server configuration at path.
func LoadServer(path string) (*Server, error) {
	var cfg Server
	if err := load(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.KCP == nil {
		d := DefaultKCP()
		cfg.KCP = &d
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid server config %s: %w", path, err)
	}
	return &cfg, nil
}

// LoadClient reads and validates the client configuration at path.
func LoadClient(path string) (*Client, error) {
	var cfg Client
	if err := load(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.TLSCertPath == "" {
		cfg.TLSCertPath = filepath.Join(filepath.Dir(path), "cert.pem")
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid client config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Server) validate() error {
	if _, err := proto.Parse(string(c.Protocol)); err != nil {
		return err
	}
	if err := validatePort("control_port", c.ControlPort); err != nil {
		return err
	}
	if err := validatePort("data_port", c.DataPort); err != nil {
		return err
	}
	if c.ControlPort == c.DataPort && !c.Protocol.UDPBased() {
		return fmt.Errorf("control_port and data_port must differ for protocol %q", c.Protocol)
	}
	if err := validateHostPort("xray_inbound_address", c.TargetAddress); err != nil {
		return err
	}
	if err := validateToken(c.Token); err != nil {
		return err
	}
	if c.Protocol.TLS() && (c.TLSCertPath == "" || c.TLSKeyPath == "") {
		return fmt.Errorf("tls_cert_path and tls_key_path are required for protocol %q", c.Protocol)
	}
	return c.KCP.Validate()
}

func (c *Client) validate() error {
	if err := validateHostPort("control_server_address", c.ControlAddress); err != nil {
		return err
	}
	if c.RemoteHost == "" {
		return fmt.Errorf("remote_server_ip must not be empty")
	}
	if err := validateHostPort("local_listen_port", c.ListenAddress); err != nil {
		return err
	}
	return validateToken(c.Token)
}

func load(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	if err := json.NewDecoder(f).Decode(v); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	return nil
}

func validateToken(token string) error {
	if len(token) < minTokenLength {
		return fmt.Errorf("token must be at least %d characters", minTokenLength)
	}
	return nil
}

func validatePort(field, port string) error {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s must be a port number between 1 and 65535, got %q", field, port)
	}
	return nil
}

func validateHostPort(field, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s must be host:port, got %q: %w", field, addr, err)
	}
	if host == "" && field != "local_listen_port" {
		return fmt.Errorf("%s must include a host, got %q", field, addr)
	}
	return validatePort(field, port)
}
