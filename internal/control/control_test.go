package control

import (
	"bytes"
	"errors"
	"testing"

	"github.com/mahdi-1991/G-tun/internal/auth"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/mahdi-1991/G-tun/internal/proto"
)

const token = "shared-secret-token"

func TestSendReceiveRoundTrip(t *testing.T) {
	kcp := config.DefaultKCP()
	want := Config{Protocol: proto.UTCPMux, Port: "8081", KCP: &kcp}
	channel := auth.Channel("channel-transcript")

	var buf bytes.Buffer
	if err := Send(&buf, token, channel, want); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got, err := Receive(&buf, token, channel)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestReceiveRejectsInvalidMessages(t *testing.T) {
	channel := auth.Channel("channel-transcript")
	valid := func() []byte {
		var buf bytes.Buffer
		if err := Send(&buf, token, channel, Config{Protocol: proto.TCP, Port: "8081"}); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}

	tests := []struct {
		name    string
		data    []byte
		token   string
		channel auth.Channel
		wantErr error
	}{
		{"wrong token", valid(), "another-secret-token", channel, auth.ErrAuthFailed},
		{"other session", valid(), token, auth.Channel("other-transcript"), auth.ErrAuthFailed},
		{"tampered body", func() []byte { b := valid(); b[5] ^= 1; return b }(), token, channel, auth.ErrAuthFailed},
		{"truncated", valid()[:10], token, channel, nil},
		{"oversized length", []byte{0xFF, 0xFF}, token, channel, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Receive(bytes.NewReader(tc.data), tc.token, tc.channel)
			if err == nil {
				t.Fatal("Receive accepted an invalid message")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestEqual(t *testing.T) {
	a, b := config.DefaultKCP(), config.DefaultKCP()
	b.Resend++
	tests := []struct {
		name string
		x, y Config
		want bool
	}{
		{"same", Config{Protocol: proto.TCP, Port: "1"}, Config{Protocol: proto.TCP, Port: "1"}, true},
		{"protocol", Config{Protocol: proto.TCP, Port: "1"}, Config{Protocol: proto.WS, Port: "1"}, false},
		{"port", Config{Protocol: proto.TCP, Port: "1"}, Config{Protocol: proto.TCP, Port: "2"}, false},
		{"kcp equal", Config{Protocol: proto.UTCPMux, KCP: &a}, Config{Protocol: proto.UTCPMux, KCP: &a}, true},
		{"kcp differs", Config{Protocol: proto.UTCPMux, KCP: &a}, Config{Protocol: proto.UTCPMux, KCP: &b}, false},
		{"kcp missing", Config{Protocol: proto.UTCPMux, KCP: &a}, Config{Protocol: proto.UTCPMux}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.x.Equal(tc.y); got != tc.want {
				t.Errorf("Equal = %v, want %v", got, tc.want)
			}
		})
	}
}
