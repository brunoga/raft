//go:build !windows

package udpbroadcast

import "syscall"

// setBroadcast is the net.ListenConfig.Control callback that sets SO_BROADCAST
// on the UDP socket, enabling writes to subnet broadcast addresses.
func setBroadcast(_, _ string, c syscall.RawConn) error {
	var setErr error
	if err := c.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}
