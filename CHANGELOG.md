# Changelog

## 2.0.0

A full rewrite of the code base. **The wire protocol changed: update the server and every client together.**
Existing `server_config.json` / `client_config.json` files keep working; to upgrade, re-run the installer
(or `install.sh --update`). The old `g-tun` menu entry "Update" cannot upgrade a 1.x install because the
source layout changed.

### Security
- **Data ports are now authenticated.** Previously only the control channel checked the token, so anyone who
  found the data port could use the tunnel to reach the target service. Every data connection now performs a
  mutual HMAC handshake first (TCP, WS/WSS, KCP, QUIC and every mux variant); UDP datagrams carry an
  authentication tag.
- **The server is authenticated too.** The control message that selects the protocol and port is MAC-protected and
  bound to the session, so a man in the middle can no longer downgrade the client to another protocol.
- **KCP traffic is encrypted** (AES-256, key derived from the token). It was sent in clear text before.
- **`key.pem` is created with mode 0600**; configuration files are written with mode 0600 and `/etc/g-tun` with 0700.
- The control protocol no longer uses unbounded text lines (memory exhaustion), and the handshake is binary and
  fixed-size.
- QUIC uses the `h3` ALPN and the generated certificate no longer carries a fixed organisation name.
- systemd units run with `NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome` and friends.

### Bug fixes
- **UDP mode relayed datagrams over a TCP connection**, destroying datagram boundaries. It now relays
  datagram-to-datagram to a UDP target. Idle sessions are expired, and a race that could delete or leak a fresh
  session when an old one timed out is fixed.
- **QUIC streams were invisible to the server until the client sent data**, which broke protocols where the server
  speaks first (SSH, SMTP, ...). The client now sends a marker byte.
- QUIC streams are fully released (`CancelRead`) when a relay ends.
- Mux clients no longer hold a lock during a dial without a timeout; every dial and handshake has a timeout, and
  concurrent callers share one reconnection attempt.
- The accept loops back off on errors instead of spinning at 100% CPU (for example when out of file descriptors).
- A relay now closes both sides when either direction finishes instead of leaving half-open connections behind.
- Failing to read a certificate, resolve an address or generate a key no longer panics or is silently ignored.
- The client keeps the running transport when the control connection reconnects, restarts a transport that failed
  with backoff, and resets its reconnect backoff only after a successful authentication.
- Shutting the server down (SIGTERM) now closes active tunnel connections.
- Session objects of closed transports are closed instead of leaking.

### Project structure and quality
- Single Go module with `cmd/` and `internal/` packages (`auth`, `config`, `control`, `transport`, `server`,
  `client`, `netutil`, `tlsutil`, ...) instead of copied `server.go` / `client.go` files with separate modules.
- Structured logging (`log/slog`), context-based shutdown, validated configuration, `-config`, `-check`,
  `-version` and `-debug` flags.
- Unit tests and end-to-end tests for every protocol (run with the race detector), plus a CI workflow.
- `install.sh` and `g-tun.sh` rewritten: input validation, no JSON injection, Go architecture detection (amd64
  and arm64), atomic binary replacement, `install.sh --update`, configuration check before restarting.
- LF line endings enforced with `.gitattributes`.

### Behaviour changes to be aware of
- `udp` now targets a **UDP** service at `xray_inbound_address`.
- The client no longer reads `data_port` or `kcp_config`; the server announces the port and KCP parameters.
- Streams of the smux-based protocols (`tcpmux`, `wsmux`, `wssmux`, `utcpmux`) and of plain `ws`/`wss` are closed
  completely as soon as either side finishes; TCP and QUIC keep real half-close semantics.
