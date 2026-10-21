//go:build linux

package control

import (
	"net"
	"strconv"
	"syscall"
)

// peerUID returns the uid of the process at the other end of a unix connection
// (SO_PEERCRED). A connection that is not a unix socket, or a failed lookup,
// logs "-"; the control socket is unix-only, so this is a safety net.
func peerUID(conn net.Conn) string {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return "-"
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return "-"
	}

	var (
		peer   uint32
		loaded bool
	)
	if err := raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			return
		}
		peer, loaded = cred.Uid, true
	}); err != nil || !loaded {
		return "-"
	}
	return strconv.FormatUint(uint64(peer), 10)
}
