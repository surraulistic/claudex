package codex

import (
	"net"
	"syscall"
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
		if u, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
			pid = int(u.Pid)
		}
	})
	return pid
}
