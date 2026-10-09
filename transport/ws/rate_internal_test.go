package ws

import (
	"fmt"
	"testing"
	"time"
)

// A full table (e.g. an address flood) must not lock every new source out: new sources share one
// overflow bucket, the table stays bounded and tracked sources keep their own bucket (M2-01).
func TestHandshakeRateMapDegradesToSharedOverflowBucketAtCapacity(t *testing.T) {
	s := &Server{
		cfg:       ServerConfig{HandshakeRateLimit: 1},
		ratePerIP: make(map[string]*ipRateState),
	}
	now := time.Now()
	for i := 0; i < maxRateEntries; i++ {
		s.ratePerIP[fmt.Sprint(i)] = &ipRateState{lastCheck: now, tokens: 1}
	}
	if !s.allowHandshakeRate("new-source") {
		t.Fatal("a full rate map refused the first new source instead of using the overflow bucket")
	}
	if len(s.ratePerIP) != maxRateEntries {
		t.Fatalf("rate map grew past the cap: %d", len(s.ratePerIP))
	}
	// The shared bucket is finite (burst 5 x 10 for rate 1): a flood is eventually refused.
	allowed := 1
	for i := 0; i < 200; i++ {
		if s.allowHandshakeRate(fmt.Sprintf("flood-%d", i)) {
			allowed++
		}
	}
	if len(s.ratePerIP) != maxRateEntries { // the sweep is throttled; the cap is not
		t.Fatalf("rate map grew past the cap during a flood: %d", len(s.ratePerIP))
	}
	if allowed < 40 || allowed > 60 {
		t.Fatalf("overflow bucket admitted %d of 201 new sources, want about 50", allowed)
	}
	// A tracked source is unaffected by the exhausted overflow bucket.
	if !s.allowHandshakeRate("0") {
		t.Fatal("tracked source refused because the overflow bucket is empty")
	}
}

// IPv6 sources are keyed by /64, IPv4 and IPv4-mapped addresses by themselves.
func TestLimiterKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.9":             "203.0.113.9",
		"::ffff:203.0.113.9":      "::ffff:203.0.113.9",
		"2001:db8:1:2:aaaa::1":    "2001:db8:1:2::/64",
		"2001:db8:1:2:bbbb::ffff": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":         "2001:db8:1:3::/64",
		"not-an-ip":               "not-an-ip",
		"":                        "",
	} {
		if got := limiterKey(in); got != want {
			t.Errorf("limiterKey(%q) = %q, want %q", in, got, want)
		}
	}
}
