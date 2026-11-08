package codex

import (
	"net"
	"syscall"
)

// SOL_LOCAL и LOCAL_PEERPID из <sys/un.h>: в syscall для darwin их нет.
const (
	solLocal     = 0
	localPeerPID = 2
)

func peerPID(c net.Conn) int {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	raw.Control(func(fd uintptr) {
		if v, err := syscall.GetsockoptInt(int(fd), solLocal, localPeerPID); err == nil {
			pid = v
		}
	})
	return pid
}
