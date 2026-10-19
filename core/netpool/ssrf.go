package netpool

import "net/netip"

var (
	metadataV6 = netip.MustParseAddr("fd00:ec2::254") // AWS IMDS over IPv6
	cgnat      = netip.MustParsePrefix("100.64.0.0/10")
)

// AllowedIP is the dial-time SSRF rule for operator-supplied endpoints:
// link-local (incl. 169.254.169.254 metadata), multicast and unspecified are
// always refused; loopback, private and CGNAT ranges only with allowPrivate
// (self-hosted vLLM).
func AllowedIP(ip netip.Addr, allowPrivate bool) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsInterfaceLocalMulticast(),
		ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip == metadataV6:
		return false
	case ip.IsLoopback(), ip.IsPrivate(), cgnat.Contains(ip):
		return allowPrivate
	}
	return true
}
