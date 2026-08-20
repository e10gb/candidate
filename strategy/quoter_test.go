// Tests for the quoting logic. Each one pins a behaviour that was, at some point,
// wrong in a way that produced plausible-looking output rather than an error.

package main

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func testCfg() config {
	return config{
		Sender:        "QUOTE001",
		Tiers:         1, // single tier unless a test opts in
		Feed:          "AAH6",
		Clip:          5,
		MaxPos:        25,
		EdgeTicks:     2,
		SkewFrac:      1.0,
		MinEdgeTicks:  1,
		EdgeVolMult:   0, // fixed edge unless a test opts in
		MaxEdgeTicks:  120,
		VolWindow:     2 * time.Second,
		FastVolWindow: 0, // off unless a test opts in
		PullMove:      0, // off unless a test opts in
		PullWindow:    200 * time.Millisecond,
		PullFor:       300 * time.Millisecond,
		MaxTPS:        20,
		MaxBurst:      8,
	}
}

// outer returns the widest tier on each side, which is the one every
// pre-existing test was written against.
func outer(bids, asks []target) (target, target) {
	return bids[len(bids)-1], asks[len(asks)-1]
}

// newTestQuoter builds a quoter with a known book and no network.
func newTestQuoter(t *testing.T, cfg config, bid, ask int) *quoter {
	t.Helper()
	q := newQuoter(nil, cfg, newIDs())
	q.bidPx, q.bidOK = bid, true
	q.askPx, q.askOK = ask, true
	return q
}

func TestQuotesStraddleMidByEdge(t *testing.T) {
	q := newTestQuoter(t, testCfg(), 648, 652) // fair 650
	bids, asks, ok := q.desired()
	if !ok {
		t.Fatal("expected a quote on a healthy two-sided book")
	}
	bid, ask := outer(bids, asks)
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
		bids, asks, ok := q.desired()
		if !ok {
			t.Fatalf("pos=%d: expected a quote", pos)
		}
		bid, ask := outer(bids, asks)
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
	lb, la, _ := q.desired()
	longBid, longAsk := outer(lb, la)
	q.position = -20 // short: we want to buy
	sb, sa, _ := q.desired()
	shortBid, shortAsk := outer(sb, sa)

	if longAsk.px >= shortAsk.px {
		t.Errorf("long should offer cheaper than short: long ask %d, short ask %d",
			longAsk.px, shortAsk.px)
	}
	if longBid.px >= shortBid.px {
		t.Errorf("long should bid stingier than short: long bid %d, short bid %d",
			longBid.px, shortBid.px)
	}
}

// Skew is a fraction of the *current* edge, so the lean keeps its intended
// strength whatever the edge happens to be. It used to be an absolute tick count
// chosen when the edge was 2, which left it doing almost nothing once the edge
// became volatility-sized -- the quoter stopped managing its own inventory and
// pushed the work onto the hedger, which pays the spread to do the same job.
func TestSkewScalesWithTheEdge(t *testing.T) {
	lean := func(edge float64) float64 {
		cfg := testCfg()
		cfg.EdgeTicks = edge // no vol history, so the edge is the floor
		cfg.SkewFrac = 1.0
		q := newTestQuoter(t, cfg, 1000-int(edge)-5, 1000+int(edge)+5)
		q.position = cfg.MaxPos / 2 // half inventory -> half a lean
		b, a, ok := q.desired()
		if !ok {
			t.Fatalf("edge %.0f: expected a quote", edge)
		}
		bid, _ := outer(b, a)
		// Unskewed the bid would sit at fair-edge; the lean is how much further.
		return (1000 - edge) - float64(bid.px)
	}

	narrow, wide := lean(4), lean(40)
	if narrow <= 0 || wide <= 0 {
		t.Fatalf("expected a downward lean when long, got %.1f and %.1f", narrow, wide)
	}
	if ratio := wide / narrow; ratio < 8 || ratio > 12 {
		t.Errorf("lean should scale with the edge (10x here), got %.1fx "+
			"(narrow %.1f, wide %.1f)", ratio, narrow, wide)
	}
}

// At full inventory the flattening side is pulled a whole edge toward fair, and
// held exactly MinEdgeTicks off it -- attractive as possible, still profitable.
func TestFullInventoryLeansToTheMinimumEdge(t *testing.T) {
	cfg := testCfg()
	cfg.EdgeTicks = 20
	cfg.SkewFrac = 1.0
	q := newTestQuoter(t, cfg, 975, 1025) // fair 1000
	q.position = cfg.MaxPos               // maximum long: we want to sell
	b, a, ok := q.desired()
	if !ok {
		t.Fatal("expected a quote")
	}
	_, ask := outer(b, a)
	if want := 1000 + int(cfg.MinEdgeTicks); ask.px != want {
		t.Errorf("ask should sit at the minimum edge above fair (%d), got %d", want, ask.px)
	}
}

// Two tiers: a tight small quote to earn calm-market flow, and a wide one holding
// the size where a sweep cannot take it cheaply. A single wide quote is safe but
// forfeits the easy flow, which is why fill counts were only a few dozen a run.
func TestInnerTierIsTighterAndSmaller(t *testing.T) {
	cfg := testCfg()
	cfg.Tiers = 2
	cfg.InnerEdgeFrac, cfg.InnerSizeFrac = 0.4, 0.4
	cfg.EdgeTicks = 20
	q := newTestQuoter(t, cfg, 900, 1100) // fair 1000, wide enough not to clamp

	bids, asks, ok := q.desired()
	if !ok {
		t.Fatal("expected quotes")
	}
	if len(bids) != 2 || len(asks) != 2 {
		t.Fatalf("expected 2 tiers a side, got %d/%d", len(bids), len(asks))
	}
	if bids[0].px <= bids[1].px {
		t.Errorf("inner bid %d should be tighter (higher) than outer %d",
			bids[0].px, bids[1].px)
	}
	if asks[0].px >= asks[1].px {
		t.Errorf("inner ask %d should be tighter (lower) than outer %d",
			asks[0].px, asks[1].px)
	}
	if bids[0].vol >= bids[1].vol {
		t.Errorf("inner size %d should be smaller than outer %d",
			bids[0].vol, bids[1].vol)
	}
}

// Tiers share one position limit; they must not each quote the full remaining
// room and breach it between them.
func TestTiersShareThePositionLimit(t *testing.T) {
	cfg := testCfg()
	cfg.Tiers = 2
	q := newTestQuoter(t, cfg, 900, 1100)
	q.position = cfg.MaxPos - 3 // only 3 lots of room left, across both tiers

	bids, _, _ := q.desired()
	total := 0
	for _, b := range bids {
		total += b.vol
	}
	if total > 3 {
		t.Errorf("tiers would buy %d lots with only 3 of room", total)
	}
}

// Pricing off a leading instrument. Which contract leads is discovered from
// activity, the basis between them is learned, and a lead that goes quiet is
// ignored -- a stale lead is worse than our own book.
func TestFairValueFollowsTheLeadInstrument(t *testing.T) {
	cfg := testCfg()
	cfg.Feed = "AAM6" // we quote the back month
	cfg.UseRef = true
	// The basis must adapt *slowly*: it stands for the structural offset between
	// the contracts, not the transient lag we are trying to trade. Learn it fast
	// and it absorbs the lead's move on the very tick we wanted to react to.
	cfg.BasisAlpha = 0.05
	cfg.RefStale = time.Second
	q := newTestQuoter(t, cfg, 995, 1005) // our mid 1000

	q.mu.Lock()
	own := q.fairValue()
	q.mu.Unlock()
	if own != 1000 {
		t.Errorf("with no lead seen yet, fair is our own mid; got %.1f", own)
	}

	// The front month trades at 900 and is the busier contract: basis is +100.
	q.onAnyBBO(&nats.Msg{Data: []byte("1 AAH6 895 5 905 5")})
	q.mu.Lock()
	fair, lead := q.fairValue(), q.refFeed
	q.mu.Unlock()
	if lead != "AAH6" {
		t.Fatalf("expected AAH6 to be picked as the lead, got %q", lead)
	}
	if fair != 1000 {
		t.Errorf("basis should absorb the level difference, got %.1f", fair)
	}

	// The lead moves up 40. Our own book has not caught up, but fair should move
	// nearly all the way with it -- that gap is exactly what gets picked off.
	q.onAnyBBO(&nats.Msg{Data: []byte("1 AAH6 935 5 945 5")})
	q.mu.Lock()
	fair = q.fairValue()
	q.mu.Unlock()
	if fair < 1035 {
		t.Errorf("fair should follow the lead toward 1040, got %.1f", fair)
	}
}

// Quoting the most active contract and then pricing it off a quieter sibling
// would import lag rather than remove it.
func TestTheBusiestContractDoesNotFollowAnyone(t *testing.T) {
	cfg := testCfg()
	cfg.Feed = "AAH6"
	cfg.UseRef = true
	cfg.BasisAlpha = 0.05
	cfg.RefStale = time.Second
	q := newTestQuoter(t, cfg, 995, 1005)

	// Our own contract updates far more often than its sibling.
	for i := 0; i < 20; i++ {
		q.onAnyBBO(&nats.Msg{Data: []byte("1 AAH6 995 5 1005 5")})
	}
	q.onAnyBBO(&nats.Msg{Data: []byte("1 AAM6 895 5 905 5")})

	q.mu.Lock()
	lead, fair := q.refFeed, q.fairValue()
	q.mu.Unlock()
	if lead != "" {
		t.Errorf("the busiest contract should follow nothing, picked %q", lead)
	}
	if fair != 1000 {
		t.Errorf("fair should be our own mid, got %.1f", fair)
	}
}

func TestUnrelatedUnderlyingIsIgnored(t *testing.T) {
	cfg := testCfg()
	cfg.Feed = "AAM6"
	cfg.UseRef = true
	q := newTestQuoter(t, cfg, 995, 1005)
	for i := 0; i < 5; i++ {
		q.onAnyBBO(&nats.Msg{Data: []byte("1 ZZH6 100 5 110 5")})
	}
	q.mu.Lock()
	lead := q.refFeed
	q.mu.Unlock()
	if lead != "" {
		t.Errorf("a different underlying must not be used as a lead, picked %q", lead)
	}
}

func TestStaleLeadIsIgnored(t *testing.T) {
	cfg := testCfg()
	cfg.Feed = "AAM6"
	cfg.UseRef = true
	cfg.BasisAlpha = 0.05
	cfg.RefStale = 20 * time.Millisecond
	q := newTestQuoter(t, cfg, 995, 1005)

	q.onAnyBBO(&nats.Msg{Data: []byte("1 AAH6 895 5 905 5")})
	time.Sleep(40 * time.Millisecond)
	q.mu.Lock()
	fair := q.fairValue()
	q.mu.Unlock()
	if fair != 1000 {
		t.Errorf("a lead that has gone quiet must be ignored, got %.1f", fair)
	}
}

// Pulling, not widening. Widening changes the target price, so reconcile cancels
// and re-adds -- and the re-add drops a fresh order into the middle of the move,
// which measured worse than doing nothing. Pulling leaves the book empty of our
// orders until the move has landed.
func TestSharpMovePullsUsOutOfTheMarket(t *testing.T) {
	cfg := testCfg()
	cfg.PullMove = 20
	// Short windows so the test can create real elapsed time. A move is judged
	// against the mid one PullWindow ago, so the history has to actually be that
	// old -- pushing every sample in the same microsecond measures nothing.
	cfg.PullWindow = 50 * time.Millisecond
	cfg.PullFor = 200 * time.Millisecond
	q := newTestQuoter(t, cfg, 995, 1005)

	// A calm book quotes normally.
	q.onBBO(&nats.Msg{Data: []byte("1 AAM6 995 5 1005 5")})
	if _, _, ok := q.desired(); !ok {
		t.Fatal("a calm market should be quoted")
	}
	time.Sleep(cfg.PullWindow + 30*time.Millisecond)

	// A 40-point jump against that reference: out of the market.
	q.onBBO(&nats.Msg{Data: []byte("2 AAM6 1035 5 1045 5")})
	if _, _, ok := q.desired(); ok {
		t.Error("a sharp move should pull us out, not merely widen us")
	}

	// And back in once it has passed and the market has settled.
	time.Sleep(cfg.PullFor + 50*time.Millisecond)
	q.onBBO(&nats.Msg{Data: []byte("3 AAM6 1036 5 1046 5")})
	if _, _, ok := q.desired(); !ok {
		t.Error("should return to the market once the move has landed")
	}
}

func TestPullIsOffByDefault(t *testing.T) {
	cfg := testCfg() // PullMove 0
	q := newTestQuoter(t, cfg, 995, 1005)
	q.onBBO(&nats.Msg{Data: []byte("1 AAM6 995 5 1005 5")})
	time.Sleep(cfg.PullWindow + 30*time.Millisecond)
	q.onBBO(&nats.Msg{Data: []byte("2 AAM6 1195 5 1205 5")}) // huge jump
	if _, _, ok := q.desired(); !ok {
		t.Error("with PullMove unset a move must not stop us quoting")
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
	b, a, _ := q.desired()
	bid, ask := outer(b, a)
	if bid.vol != 2 {
		t.Errorf("bid size should be capped to the remaining 2, got %d", bid.vol)
	}
	if ask.vol != cfg.Clip {
		t.Errorf("sell side should be unrestricted, got %d", ask.vol)
	}
	q.position = cfg.MaxPos
	b, a, _ = q.desired()
	bid, _ = outer(b, a)
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

// A burst inside the fast window must widen the edge even while the slow window
// still reads calm -- that gap is where the picking-off happened: measured -7.15
// per lot at 100ms after a fill against -0.49 by 500ms.
func TestFastVolatilityCatchesABurstTheSlowWindowMisses(t *testing.T) {
	cfg := testCfg()
	cfg.EdgeVolMult = 4
	cfg.VolWindow = 10 * time.Second
	cfg.FastVolWindow = 200 * time.Millisecond
	cfg.MaxEdgeTicks = 0
	q := newTestQuoter(t, cfg, 995, 1005)

	// A long calm history, then a real pause so it falls *outside* the fast
	// window -- otherwise both horizons see the same samples and the test proves
	// nothing (which is exactly what it did first time round).
	q.mu.Lock()
	for i := 0; i < 200; i++ {
		q.pushMid(1000)
	}
	slowOnly := q.cfg.EdgeVolMult * q.volatility()
	q.mu.Unlock()
	time.Sleep(cfg.FastVolWindow + 50*time.Millisecond)

	// Then a burst, entirely inside the fast window.
	q.mu.Lock()
	for _, m := range []float64{1000, 1040, 960, 1050} {
		q.pushMid(m)
	}
	fast := q.volatilityOver(cfg.FastVolWindow)
	slow := q.volatility()
	edge := q.edge()
	q.mu.Unlock()

	if fast <= slow {
		t.Errorf("the burst should read louder on the fast window: fast %.1f, slow %.1f",
			fast, slow)
	}
	if edge <= slowOnly {
		t.Errorf("edge should widen on the fast reading (%.1f), got %.1f", fast, edge)
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
	q.bids[0] = resting{id: "bbbbbbbb", px: 648, vol: 5}
	q.onOwnMD(&nats.Msg{Data: []byte("1 E O:aaaaaaaa QUOTE001:bbbbbbbb 2 648 1 S")})
	if !q.bids[0].live() || q.bids[0].vol != 3 {
		t.Errorf("a partial fill should leave 3 resting, got live=%v vol=%d",
			q.bids[0].live(), q.bids[0].vol)
	}
	q.onOwnMD(&nats.Msg{Data: []byte("1 E O:aaaaaaaa QUOTE001:bbbbbbbb 3 648 1 S")})
	if q.bids[0].live() {
		t.Error("a fully filled order should no longer be recorded as resting")
	}
}

// Negative prices are legal on this exchange (probed), so grid rounding must
// floor mathematically rather than truncate toward zero.
func TestTickRounding(t *testing.T) {
	for _, tc := range []struct{ px, tick, floor, ceil int }{
		{603, 5, 600, 605},
		{600, 5, 600, 600},
		{-3, 5, -5, 0},
		{-7, 5, -10, -5},
	} {
		if got := floorToTick(tc.px, tc.tick); got != tc.floor {
			t.Errorf("floorToTick(%d,%d) = %d, want %d", tc.px, tc.tick, got, tc.floor)
		}
		if got := ceilToTick(tc.px, tc.tick); got != tc.ceil {
			t.Errorf("ceilToTick(%d,%d) = %d, want %d", tc.px, tc.tick, got, tc.ceil)
		}
	}
}

func TestQuotesSitOnTheTickGrid(t *testing.T) {
	cfg := testCfg()
	cfg.TickSize = 5
	cfg.EdgeTicks = 12
	q := newTestQuoter(t, cfg, 940, 1060) // fair 1000
	bids, asks, ok := q.desired()
	if !ok {
		t.Fatal("expected a quote")
	}
	bid, ask := outer(bids, asks)
	if bid.px%5 != 0 || ask.px%5 != 0 {
		t.Errorf("quotes off the tick grid: bid %d ask %d", bid.px, ask.px)
	}
	if float64(bid.px) > 1000-cfg.EdgeTicks || float64(ask.px) < 1000+cfg.EdgeTicks {
		t.Errorf("grid rounding must move prices away from fair, not toward it: %d/%d",
			bid.px, ask.px)
	}
}
