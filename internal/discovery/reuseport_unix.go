//go:build !windows

package discovery

import (
	"syscall"
)

func reuseAddrControl(network, address string, c syscall.RawConn) error {
	var opErr error
	err := c.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
		// SO_REUSEPORT: macOS=0x200, Linux=15
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, 0x0200, 1)
	})
	if err != nil {
		return err
	}
	return opErr
}
