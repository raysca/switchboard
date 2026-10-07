//go:build unix && !aix && !illumos

package netpool

import (
	"crypto/tls"
	"net"
	"syscall"
)

// alive peeks the socket without consuming bytes (an HTTP/2 server speaks
// first, so a real Read would eat its SETTINGS frame): EOF or an error means
// the peer closed the idle spare.
func alive(c net.Conn) bool {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	sc, ok := c.(syscall.Conn)
	if !ok {
		return true
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	ok = true
	if err := rc.Read(func(fd uintptr) bool {
		var b [1]byte
		n, _, err := syscall.Recvfrom(int(fd), b[:], syscall.MSG_PEEK|syscall.MSG_DONTWAIT)
		ok = err == syscall.EAGAIN || (err == nil && n > 0)
		return true
	}); err != nil {
		return false
	}
	return ok
}
