package main

import (
	"testing"
	"time"
)

func TestParseReply(t *testing.T) {
	for _, tc := range []struct {
		in      string
		ok      bool
		n, code int
		wantErr bool
	}{
		{in: "EXCHANGE Y 0", ok: true, n: 0},
		{in: "EXCHANGE Y 10", ok: true, n: 10},
		{in: "EXCHANGE N 203 re-used order id", code: 203},
		{in: "EXCHANGE N 305 orderid not active", code: 305},
		{in: "garbage", wantErr: true},
		{in: "EXCHANGE Q 1", wantErr: true},
	} {
		r, err := parseReply(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", tc.in, err)
			continue
		}
		if r.ok != tc.ok || r.n != tc.n || r.code != tc.code {
			t.Errorf("%q: got ok=%v n=%d code=%d", tc.in, r.ok, r.n, r.code)
		}
	}
}

// Order ids are consumed permanently per sender (reject 203), so the generator
// must never repeat -- including across a container restart, which is why it is
// seeded from the clock.
func TestOrderIDsAreUniqueAndEightChars(t *testing.T) {
	c := newClient(nil, "QUOTE001", "AAH6", 20, 8)
	seen := make(map[string]bool, 10000)
	for i := 0; i < 10000; i++ {
		id := c.newOrderID()
		if len(id) != 8 {
			t.Fatalf("order id %q is %d chars, protocol requires 8", id, len(id))
		}
		if seen[id] {
			t.Fatalf("order id %q reused after %d ids", id, i)
		}
		seen[id] = true
	}
}

// The reason for a bucket rather than a fixed gap: repricing both sides is four
// requests, and forcing them 1/rate apart left us on stale quotes for ~200ms.
func TestTokenBucketAllowsABurstThenThrottles(t *testing.T) {
	burst := 8
	c := newClient(nil, "QUOTE001", "AAH6", 20, burst)

	for i := 0; i < burst; i++ {
		if w := c.reserve(); w != 0 {
			t.Fatalf("request %d of the burst should not wait, got %v", i+1, w)
		}
	}
	w := c.reserve()
	if w <= 0 {
		t.Fatal("the request after the burst should be throttled")
	}
	if w > time.Second {
		t.Errorf("throttle of %v is longer than the refill rate justifies", w)
	}
}

func TestTokenBucketRefills(t *testing.T) {
	c := newClient(nil, "QUOTE001", "AAH6", 100, 2) // refills every 10ms
	c.reserve()
	c.reserve()
	if c.reserve() == 0 {
		t.Fatal("expected throttling once the bucket is empty")
	}
	time.Sleep(50 * time.Millisecond)
	if w := c.reserve(); w != 0 {
		t.Errorf("bucket should have refilled after 50ms, still waiting %v", w)
	}
}

func TestUnlimitedWhenRateIsZero(t *testing.T) {
	c := newClient(nil, "QUOTE001", "AAH6", 0, 0)
	for i := 0; i < 100; i++ {
		if w := c.reserve(); w != 0 {
			t.Fatalf("maxTPS=0 means unlimited, got a wait of %v", w)
		}
	}
}
