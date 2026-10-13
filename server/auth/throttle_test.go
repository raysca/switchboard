package auth

import (
	"testing"
	"time"
)

func newTestThrottle(t *testing.T) (*Throttle, *time.Time) {
	t.Helper()
	now := time.Unix(1000, 0)
	th := NewThrottle(10, 6*time.Second)
	th.now = func() time.Time { return now }
	return th, &now
}

func TestThrottleBlocksAfterTheAllowanceAndRefills(t *testing.T) {
	th, now := newTestThrottle(t)
	for i := 0; i < 10; i++ {
		if th.Blocked("1.2.3.4") {
			t.Fatalf("blocked after only %d failures", i)
		}
		th.Fail("1.2.3.4")
	}
	if !th.Blocked("1.2.3.4") {
		t.Fatal("10 failures must block")
	}
	if th.Blocked("5.6.7.8") {
		t.Fatal("another address must be unaffected")
	}
	*now = now.Add(6 * time.Second)
	if th.Blocked("1.2.3.4") {
		t.Fatal("one token is back after 6 s")
	}
	th.Fail("1.2.3.4")
	if !th.Blocked("1.2.3.4") {
		t.Fatal("and is used up again")
	}
}

func TestThrottleTableIsBounded(t *testing.T) {
	th, _ := newTestThrottle(t)
	for i := 0; i < maxThrottleEntries+500; i++ {
		th.Fail("10." + itoa(i>>16&255) + "." + itoa(i>>8&255) + "." + itoa(i&255))
	}
	th.mu.Lock()
	n := len(th.limits)
	th.mu.Unlock()
	if n > maxThrottleEntries+1 { // +1: the shared overflow bucket
		t.Fatalf("table grew to %d entries", n)
	}
}

func TestThrottleIdleEntriesAreSwept(t *testing.T) {
	th, now := newTestThrottle(t)
	for i := 0; i < maxThrottleEntries; i++ {
		th.Fail("a" + itoa(i))
	}
	*now = now.Add(2 * time.Minute) // every bucket is full again
	th.Fail("fresh")
	th.mu.Lock()
	_, ok := th.limits["fresh"]
	th.mu.Unlock()
	if !ok {
		t.Fatal("an idle table must make room for a new address instead of sharing the overflow bucket")
	}
}

func TestThrottleOverflowFailsClosed(t *testing.T) {
	th, _ := newTestThrottle(t)
	for i := 0; i < maxThrottleEntries; i++ {
		th.Fail("a" + itoa(i)) // one failure each: not idle, so never swept
	}
	for i := 0; i < 10; i++ {
		th.Fail("hot")
	}
	for i := 0; i < 10; i++ {
		th.Fail("fresh" + itoa(i)) // diverted to the shared overflow bucket
	}
	if !th.Blocked("fresh-new") {
		t.Fatal("a new address must be refused once the overflow bucket is spent")
	}
	if !th.Blocked("hot") {
		t.Fatal("an existing throttled key stays blocked")
	}
	if th.Blocked("a5") {
		t.Fatal("a known quiet key is unaffected")
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}
