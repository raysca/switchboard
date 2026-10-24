// Package egress holds the outbound-connection rules shared by everything the server calls on a caller's behalf
// (MCP servers, HTTP tools): only public addresses, judged at validation time and again when dialing, so a name that
// resolves to a private address after validation is refused. Loopback is reachable only when a caller opts in
// (development and tests; production refuses the switch).
package egress

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Lookup resolves a host name; net.DefaultResolver.LookupIPAddr is the production one.
type Lookup = func(context.Context, string) ([]net.IPAddr, error)

// LoopbackOnly reports whether host is a loopback literal or a name whose every
// resolved address is loopback.
func LoopbackOnly(ctx context.Context, host string, lookup Lookup) bool {
	if ip := net.ParseIP(host); ip != nil {
		return isLoopbackIP(ip)
	}
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return false
	}
	for _, a := range ips {
		if !isLoopbackIP(a.IP) {
			return false
		}
	}
	return true
}

func isLoopbackIP(raw net.IP) bool {
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback()
}

// ValidatePublicHost refuses a host that resolves to anything but public addresses. Dial repeats the check at connect
// time to resist DNS rebinding.
func ValidatePublicHost(ctx context.Context, host string) error {
	_, err := PublicIPs(ctx, host, net.DefaultResolver.LookupIPAddr)
	return err
}

func PublicIPs(ctx context.Context, host string, lookup Lookup) ([]net.IPAddr, error) {
	ips, err := lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("destination resolution failed")
	}
	for _, addr := range ips {
		if !IsPublicIP(addr.IP) {
			return nil, errors.New("destination denied")
		}
	}
	return ips, nil
}

var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("192.175.48.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("fec0::/10"), netip.MustParsePrefix("ff00::/8"),
}

func IsPublicIP(raw net.IP) bool {
	addr, ok := netip.AddrFromSlice(raw)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range deniedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// Dial connects only to public addresses, or also to loopback when
// allowLoopback is set; the name is resolved again here, so a validation-time
// answer cannot be swapped for a private address at dial time.
func Dial(ctx context.Context, network, address string, allowLoopback bool, lookup Lookup, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("invalid destination")
	}
	var ips []net.IPAddr
	if allowLoopback {
		// Resolve once and judge those answers: the addresses dialed are exactly the
		// ones checked, so a rebinding resolver cannot swap in another address.
		var answers []net.IPAddr
		if ip := net.ParseIP(host); ip != nil {
			answers = []net.IPAddr{{IP: ip}}
		} else if answers, err = lookup(ctx, host); err != nil || len(answers) == 0 {
			return nil, errors.New("destination resolution failed")
		}
		fixed := func(context.Context, string) ([]net.IPAddr, error) { return answers, nil }
		if LoopbackOnly(ctx, host, fixed) {
			ips = answers
		} else if ips, err = PublicIPs(ctx, host, fixed); err != nil {
			return nil, err
		}
	} else if ips, err = PublicIPs(ctx, host, lookup); err != nil {
		return nil, err
	}
	for _, entry := range ips {
		conn, err := dial(ctx, network, net.JoinHostPort(entry.IP.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("destination connection failed")
}

// NewClient returns an HTTP client for calls to caller-chosen hosts: addresses are checked at dial time (Dial), no
// proxy is consulted, and redirects are never followed (the caller sees the 3xx response). allowLoopback is the
// development and test switch.
func NewClient(allowLoopback bool) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		return Dial(ctx, network, address, allowLoopback, net.DefaultResolver.LookupIPAddr, dialer.DialContext)
	}
	return &http.Client{Transport: base, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
