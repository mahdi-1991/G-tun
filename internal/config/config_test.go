package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mahdi-1991/G-tun/internal/proto"
)

const token = "0123456789abcdef0123456789abcdef"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadServer(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "valid legacy file with kcp keys",
			body: `{"control_port":"8080","data_port":"8081","protocol":"tcp",
				"xray_inbound_address":"127.0.0.1:10085","token":"` + token + `",
				"kcp_config":{"NoDelay":1,"Interval":20,"Resend":2,"NoCongestion":1,"SndWnd":4096,"RcvWnd":4096,"DataShards":10,"ParityShards":3}}`,
		},
		{
			name: "valid without kcp",
			body: `{"control_port":"8080","data_port":"8081","protocol":"quic",
				"xray_inbound_address":"127.0.0.1:10085","token":"` + token + `",
				"tls_cert_path":"/c","tls_key_path":"/k"}`,
		},
		{
			name: "same port allowed for udp based protocol",
			body: `{"control_port":"8080","data_port":"8080","protocol":"udp",
				"xray_inbound_address":"127.0.0.1:10085","token":"` + token + `"}`,
		},
		{
			name:    "unknown protocol",
			body:    `{"control_port":"8080","data_port":"8081","protocol":"carrier-pigeon","xray_inbound_address":"127.0.0.1:1","token":"` + token + `"}`,
			wantErr: "unknown protocol",
		},
		{
			name:    "same port for tcp",
			body:    `{"control_port":"8080","data_port":"8080","protocol":"tcp","xray_inbound_address":"127.0.0.1:1","token":"` + token + `"}`,
			wantErr: "must differ",
		},
		{
			name:    "bad port",
			body:    `{"control_port":"99999","data_port":"8081","protocol":"tcp","xray_inbound_address":"127.0.0.1:1","token":"` + token + `"}`,
			wantErr: "control_port",
		},
		{
			name:    "short token",
			body:    `{"control_port":"8080","data_port":"8081","protocol":"tcp","xray_inbound_address":"127.0.0.1:1","token":"short"}`,
			wantErr: "token",
		},
		{
			name:    "missing target host",
			body:    `{"control_port":"8080","data_port":"8081","protocol":"tcp","xray_inbound_address":":1","token":"` + token + `"}`,
			wantErr: "xray_inbound_address",
		},
		{
			name:    "tls protocol without certificate",
			body:    `{"control_port":"8080","data_port":"8081","protocol":"wss","xray_inbound_address":"127.0.0.1:1","token":"` + token + `"}`,
			wantErr: "tls_cert_path",
		},
		{
			name:    "invalid json",
			body:    `{`,
			wantErr: "parse config",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadServer(writeConfig(t, tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("LoadServer: %v", err)
				}
				if cfg.KCP == nil {
					t.Error("KCP must never be nil after loading")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestLoadServerDefaultsKCP(t *testing.T) {
	cfg, err := LoadServer(writeConfig(t, `{"control_port":"8080","data_port":"8081","protocol":"tcp",
		"xray_inbound_address":"127.0.0.1:1","token":"`+token+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if *cfg.KCP != DefaultKCP() {
		t.Errorf("KCP = %+v, want defaults", *cfg.KCP)
	}
	if cfg.Protocol != proto.TCP {
		t.Errorf("Protocol = %q", cfg.Protocol)
	}
}

func TestLoadClient(t *testing.T) {
	path := writeConfig(t, `{"control_server_address":"203.0.113.5:8080","remote_server_ip":"203.0.113.5",
		"data_port":"8081","local_listen_port":"0.0.0.0:1080","token":"`+token+`"}`)
	cfg, err := LoadClient(path)
	if err != nil {
		t.Fatalf("LoadClient (legacy file with unused data_port): %v", err)
	}
	if want := filepath.Join(filepath.Dir(path), "cert.pem"); cfg.TLSCertPath != want {
		t.Errorf("TLSCertPath = %q, want %q", cfg.TLSCertPath, want)
	}

	for name, body := range map[string]string{
		"no control address": `{"remote_server_ip":"1.2.3.4","local_listen_port":":1080","token":"` + token + `"}`,
		"no remote host":     `{"control_server_address":"1.2.3.4:8080","local_listen_port":":1080","token":"` + token + `"}`,
		"bad listen address": `{"control_server_address":"1.2.3.4:8080","remote_server_ip":"1.2.3.4","local_listen_port":"1080","token":"` + token + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadClient(writeConfig(t, body)); err == nil {
				t.Error("LoadClient accepted an invalid configuration")
			}
		})
	}
}
