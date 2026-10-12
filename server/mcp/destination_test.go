package mcp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
)

// resolverFor answers every name with the given addresses.
func resolverFor(ips ...string) func(context.Context, string) ([]net.IPAddr, error) {
	return func(context.Context, string) ([]net.IPAddr, error) {
		out := make([]net.IPAddr, 0, len(ips))
		for _, ip := range ips {
			out = append(out, net.IPAddr{IP: net.ParseIP(ip)})
		}
		return out, nil
	}
}

func TestLoopbackHTTPDestinationPolicy(t *testing.T) {
	cases := []struct {
		raw     string
		resolve []string
		ok      bool
	}{
		{"http://127.0.0.1:9/mcp", nil, true},
		{"http://[::1]:9/mcp", nil, true},
		{"https://127.0.0.1:9/mcp", nil, true},
		{"http://localhost:9/mcp", []string{"127.0.0.1"}, true},
		{"https://public.example/mcp", []string{"8.8.8.8"}, true},

		{"http://10.0.0.5/mcp", nil, false},
		{"http://169.254.169.254/latest", nil, false},
		{"http://0.0.0.0:9/mcp", nil, false},
		{"http://[::ffff:10.0.0.1]/mcp", nil, false},
		{"http://localhost:9/mcp", []string{"10.0.0.5"}, false},
		{"http://localhost:9/mcp", []string{"127.0.0.1", "10.0.0.5"}, false},
		{"http://user:pw@127.0.0.1:9/mcp", nil, false},
		{"http://127.0.0.1:9/mcp?q=1", nil, false},
		{"http://127.0.0.1:9/mcp#x", nil, false},
		{"http://public.example/mcp", []string{"8.8.8.8"}, false},
		{"ftp://127.0.0.1/mcp", nil, false},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			u, err := url.Parse(c.raw)
			if err != nil {
				t.Fatal(err)
			}
			p := LoopbackHTTPDestinationPolicy{lookup: resolverFor(c.resolve...)}
			err = p.Validate(context.Background(), u)
			if (err == nil) != c.ok {
				t.Fatalf("Validate(%s) err=%v, want ok=%v", c.raw, err, c.ok)
			}
		})
	}
}

// The dialer re-resolves: a name that validated as loopback but resolves elsewhere at
// dial time must not connect.
func TestDialEgressLoopbackAgreesWithValidation(t *testing.T) {
	var dialed []string
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("stub dial")
	}
	if _, err := dialEgress(context.Background(), "tcp", "localhost:9", true, resolverFor("10.0.0.5"), dial); err == nil || len(dialed) != 0 {
		t.Fatalf("private address dialed: err=%v dialed=%v", err, dialed)
	}
	_, _ = dialEgress(context.Background(), "tcp", "localhost:9", true, resolverFor("127.0.0.1"), dial)
	if len(dialed) != 1 || dialed[0] != "127.0.0.1:9" {
		t.Fatalf("loopback not dialed with the flag on: %v", dialed)
	}
	dialed = nil
	if _, err := dialEgress(context.Background(), "tcp", "localhost:9", false, resolverFor("127.0.0.1"), dial); err == nil || len(dialed) != 0 {
		t.Fatalf("loopback dialed with the flag off: err=%v dialed=%v", err, dialed)
	}
}
