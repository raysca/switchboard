//go:build !unix || aix || illumos

package netpool

import "net"

// ponytail: no portable peek; the SpareTTL bound and the transport's own
// retry on a dead idle connection cover this platform.
func alive(net.Conn) bool { return true }
