//go:build !windows

package udpbroadcast

import "syscall"

// setBroadcast is the net.ListenConfig.Control callback that sets SO_BROADCAST
// and SO_REUSEADDR on the UDP socket.
//
// SO_BROADCAST enables writes to subnet broadcast addresses.
// SO_REUSEADDR allows multiple processes on the same machine to bind the same
// port; for broadcast UDP on Linux and macOS the kernel delivers each packet to
// ALL matching sockets, so two idprovider nodes on one host both receive each
// other's announcements correctly.
func setBroadcast(_, _ string, c syscall.RawConn) error {
	var setErr error
	if err := c.Control(func(fd uintptr) {
		if setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); setErr != nil {
			return
		}
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
