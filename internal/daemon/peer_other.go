//go:build !linux && !darwin

package daemon

import "net"

// ReportsPeerCredentials is false: this platform has no peer-credential
// implementation, so a socket verb carries no peer.
const ReportsPeerCredentials = false

// peerCredentials has no peer-credential socket option off linux and darwin,
// so the intent-change line (issue #835) carries no peer: the platform does
// not give it.
func peerCredentials(raw net.Conn) string { return "" }
