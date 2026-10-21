//go:build !linux

package control

import "net"

// peerUID has no SO_PEERCRED outside Linux, so the log records "-"
// (design §6.4).
func peerUID(net.Conn) string {
	return "-"
}
