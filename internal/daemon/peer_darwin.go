//go:build darwin

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// peerCredentials reads LOCAL_PEERCRED and LOCAL_PEERPID off an accepted
// unix-socket connection: darwin's counterpart to linux SO_PEERCRED, giving
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
	var (
		xucred *unix.Xucred
		pid    int
		pidErr error
	)
	_ = rc.Control(func(fd uintptr) {
		xucred, _ = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		pid, pidErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if xucred == nil || pidErr != nil {
		return ""
	}
	return fmt.Sprintf("uid=%d pid=%d", xucred.Uid, pid)
}
