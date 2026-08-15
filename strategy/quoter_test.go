package main

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func testCfg() config {
	return config{
		Sender:       "QUOTE001",
		Feed:         "AAH6",
		Clip:         5,
		MaxPos:       25,
		EdgeTicks:    2,
		SkewTicks:    4,
		MinEdgeTicks: 1,
		EdgeVolMult:  0, // fixed edge unless a test opts in
		MaxEdgeFrac:  0, // caps off unless a test opts in
		MaxEdgeTicks: 120,
		VolWindow:    2 * time.Second,
		MaxTPS:       20,
		MaxBurst:     8,
	}
}

// newTestQuoter builds a quoter with a known book and no network.
func newTestQuoter(t *testing.T, cfg config, bid, ask int) *quoter {
	t.Helper()
	q := newQuoter(nil, cfg)
	q.bidPx, q.bidOK = bid, true
	q.askPx, q.askOK = ask, true
	return q
}

func TestQuotesStraddleMidByEdge(t *testing.T) {
	q := newTestQuoter(t, testCfg(), 648, 652) // fair 650
	bid, ask, ok := q.desired()
	if !ok {
		t.Fatal("expected a quote on a healthy two-sided book")
	}
	if bid.px != 648 || ask.px != 652 {
		t.Errorf("want 648/652 at edge 2 around fair 650, got %d/%d", bid.px, ask.px)
	}
	if bid.vol != 5 || ask.vol != 5 {
		t.Errorf("want clip 5 both sides, got %d/%d", bid.vol, ask.vol)
	}
}

// The bug that made v1 lose money: skew larger than edge quoted through fair
// value, selling below what the position was worth to get flat.
func TestSkewNeverQuotesThroughMinEdge(t *testing.T) {
	cfg := testCfg()
	q := newTestQuoter(t, cfg, 648, 652) // fair 650
	for _, pos := range []int{0, 10, 20, 25, -10, -25} {
		q.position = pos
		bid, ask, ok := q.desired()
		if !ok {
			t.Fatalf("pos=%d: expected a quote", pos)
		}
		if bid.vol > 0 && float64(bid.px) > 650-cfg.MinEdgeTicks {
			t.Errorf("pos=%d: bid %d is inside the %.0f-tick margin around fair 650",
				pos, bid.px, cfg.MinEdgeTicks)
		}
		if ask.vol > 0 && float64(ask.px) < 650+cfg.MinEdgeTicks {
			t.Errorf("pos=%d: ask %d is inside the %.0f-tick margin around fair 650",
				pos, ask.px, cfg.MinEdgeTicks)
		}
	}
}

func TestSkewLeansAgainstInventory(t *testing.T) {
	q := newTestQuoter(t, testCfg(), 648, 652)
	q.position = 20 // long: we want to sell
	longBid, longAsk, _ := q.desired()
	q.position = -20 // short: we want to buy
	shortBid, shortAsk, _ := q.desired()

	if longAsk.px >= shortAsk.px {
		t.Errorf("long should offer cheaper than short: long ask %d, short ask %d",
			longAsk.px, shortAsk.px)
	}
	if longBid.px >= shortBid.px {
		t.Errorf("long should bid stingier than short: long bid %d, short bid %d",
			longBid.px, shortBid.px)
	}
}

func TestNoQuoteWithoutTwoSidedBook(t *testing.T) {
	for _, tc := range []struct {
		name         string
		bidOK, askOK bool
	}{
		{"no bid", false, true},
		{"no ask", true, false},
		{"empty book", false, false},
	} {
		q := newTestQuoter(t, testCfg(), 648, 652)
		q.bidOK, q.askOK = tc.bidOK, tc.askOK
		if _, _, ok := q.desired(); ok {
			t.Errorf("%s: expected no quote without a fair value", tc.name)
		}
	}
}

func TestSizeNeverBreachesPositionLimit(t *testing.T) {
	cfg := testCfg()
	q := newTestQuoter(t, cfg, 648, 652)
	q.position = cfg.MaxPos - 2 // room for 2 more
	bid, ask, _ := q.desired()
	if bid.vol != 2 {
		t.Errorf("bid size should be capped to the remaining 2, got %d", bid.vol)
	}
	if ask.vol != cfg.Clip {
		t.Errorf("sell side should be unrestricted, got %d", ask.vol)
	}
	q.position = cfg.MaxPos
	bid, _, _ = q.desired()
	if bid.vol != 0 {
		t.Errorf("at the limit the buy side must not quote, got %d", bid.vol)
	}
}

func TestEdgeScalesWithVolatility(t *testing.T) {
	cfg := testCfg()
	cfg.EdgeVolMult = 4
	q := newTestQuoter(t, cfg, 648, 652)

	q.mu.Lock()
	calm := q.edge()
	q.mu.Unlock()
	if calm != cfg.EdgeTicks {
		t.Errorf("with no history the edge should be the floor %.0f, got %.1f",
			cfg.EdgeTicks, calm)
	}

	q.mu.Lock()
	for _, m := range []float64{600, 640, 580, 660, 560, 700} { // volatile
		q.pushMid(m)
	}
	wild := q.edge()
	q.mu.Unlock()
	if wild <= calm {
		t.Errorf("edge should widen with volatility: calm %.1f, volatile %.1f", calm, wild)
	}
	if wild > cfg.MaxEdgeTicks {
		t.Errorf("edge %.1f exceeded the cap %.0f", wild, cfg.MaxEdgeTicks)
	}
}

// The cap is a fraction of price so it transfers to an instrument at a different
// level: the same 3.3% must give a wider absolute edge on a dearer contract. The
// reference is the latest mid, i.e. what the contract is worth right now.
func TestEdgeCapScalesWithPriceLevel(t *testing.T) {
	cfg := testCfg()
	cfg.EdgeVolMult = 4
	cfg.MaxEdgeFrac = 0.033
	cfg.MaxEdgeTicks = 0

	// capAt drives volatility high enough that the cap must bind, ending on `last`.
	capAt := func(last float64) float64 {
		q := newTestQuoter(t, cfg, int(last)-2, int(last)+2)
		q.mu.Lock()
		defer q.mu.Unlock()
		for _, m := range []float64{last * 0.7, last * 1.3, last * 0.7, last} {
			q.pushMid(m)
		}
		return q.edge()
	}

	for _, last := range []float64{600, 6000} {
		want := cfg.MaxEdgeFrac * last
		if got := capAt(last); got < want-0.5 || got > want+0.5 {
			t.Errorf("at price %.0f the cap should be ~%.1f, got %.1f", last, want, got)
		}
	}

	if capAt(6000) <= capAt(600)*5 {
		t.Error("the cap must scale with the price level, not stay constant")
	}
}

func TestVolWindowDropsStaleSamples(t *testing.T) {
	cfg := testCfg()
	cfg.VolWindow = 50 * time.Millisecond
	q := newTestQuoter(t, cfg, 648, 652)

	q.mu.Lock()
	for _, m := range []float64{500, 700, 500, 700} {
		q.pushMid(m)
	}
	q.mu.Unlock()
	time.Sleep(80 * time.Millisecond)

	q.mu.Lock()
	q.pushMid(650)
	q.pushMid(650)
	q.pushMid(650)
	v := q.volatility()
	n := len(q.mids)
	q.mu.Unlock()

	if n != 3 {
		t.Errorf("stale samples should be trimmed, %d left", n)
	}
	if v != 0 {
		t.Errorf("a steady mid should measure zero volatility, got %.2f", v)
	}
}

// PnL must never silently value a live position at zero: that reported cash as
// profit when the sample market's book emptied.
func TestMarkPriceNeverSilentlyDropsPosition(t *testing.T) {
	q := newTestQuoter(t, testCfg(), 648, 652)
	if px, fresh, ok := q.markPrice(); !ok || !fresh || px != 650 {
		t.Errorf("two-sided book should mark at the mid: %v %v %v", px, fresh, ok)
	}

	q.position = 10
	q.askOK = false // only a bid: we would have to sell into it
	if px, _, ok := q.markPrice(); !ok || px != 648 {
		t.Errorf("long with bid only should mark at the bid, got %v (ok=%v)", px, ok)
	}

	q.bidOK = false // no book at all
	q.lastMark, q.haveMark = 640, true
	px, fresh, ok := q.markPrice()
	if !ok || px != 640 {
		t.Errorf("should fall back to the last price seen, got %v (ok=%v)", px, ok)
	}
	if fresh {
		t.Error("a fallback mark must be reported as stale")
	}

	q.haveMark = false
	if _, _, ok := q.markPrice(); ok {
		t.Error("with nothing to mark against it must report no valuation, not zero")
	}
}

// E arrives when we were resting, T when we crossed. Getting this backwards
// flips the sign of the position -- the bug the shipped taker had.
func TestFillSideFromExecutionAndTrade(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		wantPos int
	}{
		{"we rested, buyer aggressed -> we sold",
			"1 E OTHER001:aaaaaaaa QUOTE001:bbbbbbbb 5 650 1 B", -5},
		{"we rested, seller aggressed -> we bought",
			"1 E OTHER001:aaaaaaaa QUOTE001:bbbbbbbb 5 650 1 S", 5},
		{"we crossed as buyer",
			"1 T QUOTE001:bbbbbbbb OTHER001:aaaaaaaa 5 650 1 B", 5},
		{"we crossed as seller",
			"1 T QUOTE001:bbbbbbbb OTHER001:aaaaaaaa 5 650 1 S", -5},
		{"someone else's trade is ignored",
			"1 E OTHER001:aaaaaaaa OTHER002:cccccccc 5 650 1 B", 0},
	} {
		q := newTestQuoter(t, testCfg(), 648, 652)
		q.onOwnMD(&nats.Msg{Data: []byte(tc.payload)})
		if q.position != tc.wantPos {
			t.Errorf("%s: want pos %d, got %d", tc.name, tc.wantPos, q.position)
		}
	}
}

func TestPartialFillLeavesOrderResting(t *testing.T) {
	q := newTestQuoter(t, testCfg(), 648, 652)
	q.bid = resting{id: "bbbbbbbb", px: 648, vol: 5}
	q.onOwnMD(&nats.Msg{Data: []byte("1 E O:aaaaaaaa QUOTE001:bbbbbbbb 2 648 1 S")})
	if !q.bid.live() || q.bid.vol != 3 {
		t.Errorf("a partial fill should leave 3 resting, got live=%v vol=%d",
			q.bid.live(), q.bid.vol)
	}
	q.onOwnMD(&nats.Msg{Data: []byte("1 E O:aaaaaaaa QUOTE001:bbbbbbbb 3 648 1 S")})
	if q.bid.live() {
		t.Error("a fully filled order should no longer be recorded as resting")
	}
}
