package ws

import (
	"testing"
	"time"
)

func TestHandshakeRateMapRejectsNewKeysAtCapacity(t *testing.T) {
	s := &Server{
		cfg:       ServerConfig{HandshakeRateLimit: 1},
		ratePerIP: make(map[string]*ipRateState),
	}
	now := time.Now()
	for i := 0; i < 10000; i++ {
		s.ratePerIP[string(rune(i))] = &ipRateState{lastCheck: now, tokens: 1}
	}
	if s.allowHandshakeRate("new-source") {
		t.Fatal("a full rate map accepted a new key")
	}
	if len(s.ratePerIP) != 10000 {
		t.Fatalf("rate map grew past the cap: %d", len(s.ratePerIP))
	}
}
