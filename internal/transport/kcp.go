package transport

import (
	"fmt"

	"github.com/mahdi-1991/G-tun/internal/auth"
	"github.com/mahdi-1991/G-tun/internal/config"
	"github.com/xtaci/kcp-go/v5"
)

// kcpBlockCrypt returns the cipher that encrypts all KCP traffic. The key is
// derived from the token, so a peer that does not know it cannot even
// establish a KCP session.
func kcpBlockCrypt(token string) (kcp.BlockCrypt, error) {
	block, err := kcp.NewAESBlockCrypt(auth.DeriveKey(token, "kcp"))
	if err != nil {
		return nil, fmt.Errorf("create KCP cipher: %w", err)
	}
	return block, nil
}

func tuneKCP(s *kcp.UDPSession, k config.KCP) {
	s.SetNoDelay(k.NoDelay, k.Interval, k.Resend, k.NoCongestion)
	s.SetWindowSize(k.SndWnd, k.RcvWnd)
}
