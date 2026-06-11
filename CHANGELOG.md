# Changelog

All notable changes to G-Tun will be documented in this file.

## [Unreleased] - 2026-06-11

### 🔒 Security
- Added proper authentication deadline (10s) in control channel to prevent goroutine leaks from hanging connections
- Fixed timing-safe token comparison already using `subtle.ConstantTimeCompare`

### 🐛 Bug Fixes
- **CRITICAL:** Added missing `quic-go` dependency to both `server/go.mod` and `client/go.mod`
  - Version locked to `v0.48.2` for stability
  - Build will no longer fail when QUIC protocol is selected
- **UDP Session Leak (Server & Client):** Fixed memory leak where idle UDP sessions were never cleaned up
  - Added `lastSeen` timestamp tracking to all UDP sessions
  - Background goroutine now cleans up sessions idle for more than 3 minutes
  - Runs cleanup check every 1 minute
- **Client Reconnect Flood:** Implemented exponential backoff for control connection retries
  - Starts at 3 seconds, doubles on each failure
  - Caps at 60 seconds maximum
  - Resets to 3 seconds on successful connection
  - Prevents overwhelming the server during outages

### ✨ Improvements
- **Smux Session Management (Client):** Fixed resource leak in multiplexed protocols
  - Old sessions are now explicitly closed before creating new ones
  - Failed `OpenStream` attempts now trigger session rebuild
  - Applies to: TCPMux, WSMux, WSSMux, UTCPMux, QUIC
- **Better Error Handling:** 
  - All dial/connect operations now have proper timeout handling
  - Improved error logging with connection details
  - More graceful degradation on partial failures

### 🔧 Code Quality
- Added comprehensive comments in Persian for better maintainability
- Improved `wsConnWrapper.Read()` logic to properly handle EOF on WebSocket frames
- Consistent error handling patterns across all protocol implementations
- Better goroutine lifecycle management

### 📦 Dependencies
- Updated `quic-go` from v0.42.0 to v0.48.2
- All indirect dependencies updated via `go mod tidy`

### 🚀 Deployment
- Updated `install.sh` to use `quic-go@v0.48.2`
- Updated `g-tun.sh` update function to include proper dependency management
- Added build failure detection with error messages

---

## How to Update

If you have G-Tun already installed:

1. **On the server:** Run `g-tun` and select option `8) Update G-Tun to Latest Version`
2. The script will automatically:
   - Pull latest code from GitHub
   - Update dependencies
   - Rebuild binary
   - Restart the service

No need to reinstall or reconfigure!

---

## Migration Notes

- No configuration changes required
- Existing tokens and certificates remain valid
- No downtime required (service auto-restarts)
- Compatible with all existing clients

---

## Testing Checklist

- [x] Server builds without errors
- [x] Client builds without errors
- [x] TCP protocol works
- [x] UDP protocol works with cleanup
- [x] WebSocket protocols work
- [x] Multiplexed protocols work with proper session handling
- [x] QUIC protocol works
- [x] Client reconnects with backoff
- [x] Update script works without reinstall
