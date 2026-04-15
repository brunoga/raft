//go:build windows

package udpbroadcast

import "syscall"

// setBroadcast is the net.ListenConfig.Control callback that sets SO_BROADCAST
// and SO_REUSEADDR on the UDP socket.
//
// SO_BROADCAST enables writes to subnet broadcast addresses.
// SO_REUSEADDR on Windows allows multiple processes on the same machine to bind
// the same port and receive broadcast packets independently.
func setBroadcast(_, _ string, c syscall.RawConn) error {
	var setErr error
	if err := c.Control(func(fd uintptr) {
		if setErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); setErr != nil {
			return
		}
		setErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
