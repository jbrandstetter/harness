//go:build !linux

package daemon

import "net"

// peerCredentials has no SO_PEERCRED off-platform, so the intent-change line
// (issue #835) carries no peer: the platform does not give it.
func peerCredentials(raw net.Conn) string { return "" }
