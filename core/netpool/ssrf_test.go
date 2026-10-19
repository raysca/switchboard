package netpool

import (
	"net/netip"
	"testing"
)

func TestAllowedIP(t *testing.T) {
	for _, c := range []struct {
		ip           string
		def, private bool
	}{
		{"8.8.8.8", true, true},
		{"2606:4700::1111", true, true},
		{"169.254.169.254", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fd00:ec2::254", false, false},
		{"fe80::1", false, false},
		{"0.0.0.0", false, false},
		{"127.0.0.1", false, true},
		{"::1", false, true},
		{"10.0.0.1", false, true},
		{"10.1.2.3", false, true},
		{"192.168.1.1", false, true},
		{"100.64.0.1", false, true},
		{"fd12::1", false, true},
	} {
		ip := netip.MustParseAddr(c.ip)
		if AllowedIP(ip, false) != c.def || AllowedIP(ip, true) != c.private {
			t.Errorf("%s: want default=%v private=%v", c.ip, c.def, c.private)
		}
	}
}
