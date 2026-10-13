//go:build unix && !aix && !illumos

package netpool

import (
	"crypto/tls"
	"errors"
	"net"
	"syscall"
)

// maxReadAhead bounds what alive buffers; a server that sent more before any
// request is treated as alive (the transport then sees the close itself).
const maxReadAhead = 16 << 10

// alive reads, without blocking, everything the server already sent on an idle
// spare and keeps it for replay. An EOF or error behind those bytes means the
// peer closed the spare. A MSG_PEEK cannot see this: once a TLS 1.3 server has
// sent its session ticket, a peek returns that data and hides the FIN behind it
// (Linux keeps the queue; macOS drops it on RST, so only Linux exposed the bug).
func alive(c net.Conn) bool {
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	rc, ok := c.(*replayConn)
	if !ok {
		return true
	}
	sc, ok := rc.Conn.(syscall.Conn)
	if !ok {
		return true
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	buf := make([]byte, 0, maxReadAhead)
	ok = true
	if err := raw.Read(func(fd uintptr) bool {
		for len(buf) < cap(buf) {
			n, err := syscall.Read(int(fd), buf[len(buf):cap(buf)])
			switch {
			case errors.Is(err, syscall.EINTR):
				continue
			case errors.Is(err, syscall.EAGAIN):
				return true // nothing more queued: open
			case err != nil || n == 0:
				ok = false // reset or EOF
				return true
			}
			buf = buf[:len(buf)+n]
		}
		return true // buffer full: cannot look further, assume open
	}); err != nil {
		return false
	}
	rc.pending = buf
	return ok
}
