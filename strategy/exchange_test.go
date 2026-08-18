// Tests for order entry: reply parsing, order-id uniqueness, and the rate limiter
// that must let a burst through without exceeding its sustained budget.

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

// EX_META is the exchange's own statement of the contract's limits. The local
// value is the first case verbatim; the second is what a grading market that
// actually enforces limits could look like.
func TestParseMeta(t *testing.T) {
	local := "ticksize=1 ref_price=600 band=5000 min_volume=1 max_volume=10000000 " +
		"position_limit=1000000000 max_tps=0 last_traded_price=612"
	m := parseMeta(local)
	if m.tickSize != 1 || m.maxTPS != 0 || m.positionLimit != 1000000000 {
		t.Errorf("local meta parsed wrongly: %+v", m)
	}
	m = parseMeta("ticksize=5 max_tps=40 position_limit=50")
	if m.tickSize != 5 || m.maxTPS != 40 || m.positionLimit != 50 {
		t.Errorf("limited meta parsed wrongly: %+v", m)
	}
	if m := parseMeta("garbage no equals ticksize=x"); m.tickSize != 1 {
		t.Errorf("unparsable input must fall back to tick 1, got %+v", m)
	}
}

// Breaching max_tps disconnects the sender, so the derived bucket must keep any
// one-second window under the declared limit: burst + rate <= 0.8 * max_tps.
func TestApplyMetaKeepsHeadroom(t *testing.T) {
	cfg := testCfg()
	cfg.MaxTPS = -1 // auto
	applyMeta(&cfg, meta{tickSize: 1, maxTPS: 40, positionLimit: 1000000000})
	if cfg.MaxTPS != 20 || cfg.MaxBurst != 8 {
		t.Errorf("max_tps=40 should give rate 20 burst 8, got %d/%d", cfg.MaxTPS, cfg.MaxBurst)
	}
	if worst := cfg.MaxBurst + cfg.MaxTPS; worst > 32 { // 0.8 * 40
		t.Errorf("worst one-second window %d exceeds 80%% of the declared limit", worst)
	}

	cfg = testCfg()
	cfg.MaxTPS = -1
	applyMeta(&cfg, meta{tickSize: 1, maxTPS: 0})
	if cfg.MaxTPS != 0 {
		t.Errorf("a declared limit of 0 means unlimited, got rate %d", cfg.MaxTPS)
	}

	cfg = testCfg()
	cfg.MaxTPS = 5 // explicit override wins over auto-derivation
	applyMeta(&cfg, meta{tickSize: 1, maxTPS: 100})
	if cfg.MaxTPS != 5 {
		t.Errorf("an explicit cap must survive applyMeta, got %d", cfg.MaxTPS)
	}
}

func TestApplyMetaClampsPositionLimit(t *testing.T) {
	cfg := testCfg()
	applyMeta(&cfg, meta{tickSize: 1, positionLimit: 10})
	if cfg.MaxPos != 10 {
		t.Errorf("MaxPos should clamp to the exchange limit 10, got %d", cfg.MaxPos)
	}
}
