//go:build !darwin && !linux

package codex

import "net"

func peerPID(net.Conn) int { return 0 }
