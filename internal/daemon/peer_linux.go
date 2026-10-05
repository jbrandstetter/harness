//go:build linux

package daemon

import (
	"fmt"
	"net"
	"syscall"
)

// ReportsPeerCredentials says peerCredentials names the peer here, so a test
// can expect a peer on a socket verb without hardcoding which OS gives one.
const ReportsPeerCredentials = true

// peerCredentials reads SO_PEERCRED off an accepted unix-socket connection:
// the uid and pid the kernel saw on the other end, so an intent-change line
// can name the process that issued the verb (issue #835). It returns "" when
// the connection is not a unix socket or the kernel gives no credentials.
func peerCredentials(raw net.Conn) string {
	uc, ok := raw.(*net.UnixConn)
	if !ok {
		return ""
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return ""
	}
	var ucred *syscall.Ucred
	_ = rc.Control(func(fd uintptr) {
		ucred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if ucred == nil {
		return ""
	}
	return fmt.Sprintf("uid=%d pid=%d", ucred.Uid, ucred.Pid)
}
