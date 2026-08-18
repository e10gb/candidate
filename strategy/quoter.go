// Quoting logic: the book and position we believe in, the prices we want, and the
// reconciliation that turns one into the other.
//
// Position and fills come from the exchange's own market-data feed rather than
// from what we asked for, because a seat that trusts its own requests is how the
// shipped taker came to report a flat book while trading (see NOTES.md).

package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// resting is one of our quotes sitting on the book.
type resting struct {
	id  string
	px  int
	vol int // remaining after any partial fills
}

func (r resting) live() bool { return r.id != "" }

type quoter struct {
	cfg config
	nc  *nats.Conn
	cl  *client

	mu      sync.Mutex
	bidPx   int
	askPx   int
	bidOK   bool
	askOK   bool
	lastTop string // dedupe: the BBO feed republishes on every book event

	position int
	cash     int64 // signed; buying spends cash, selling receives it
	fills    int

	// Last price we can value inventory against, kept so a book that goes empty
	// or one-sided does not leave us unable to mark a position at all.
	lastMark float64
	haveMark bool

	// Rolling mid history, for sizing the edge against how far the market is
	// actually moving. Trimmed to cfg.VolWindow on every push.
	mids []midSample

	// A reference instrument that leads ours, if configured. Our own book can be
	// stale: a correlated contract that moves first tells us where fair value has
	// gone before our own top of book catches up, which is the difference between
	// quoting and being picked off.
	refMid   float64
	refOK    bool
	refAt    time.Time
	basis    float64 // EWMA of (our mid - reference mid)
	basisSet bool

	// Which sibling contract leads, chosen by observation rather than
	// configuration: update counts per feed, and the current pick.
	ticks   map[string]int
	refFeed string

	// One resting order per tier per side. Tier 0 is the inner quote when two
	// tiers are configured: tight and small, to earn the calm-market flow that a
	// single wide quote forfeits. The outer tier carries the size and sits far
	// enough out to survive a sweep.
	bids []resting
	asks []resting

	quoteMu sync.Mutex    // serialises requote cycles
	wake    chan struct{} // coalescing signal: capacity 1
}

func newQuoter(nc *nats.Conn, cfg config) *quoter {
	return &quoter{
		cfg:   cfg,
		nc:    nc,
		cl:    newClient(nc, cfg.Sender, cfg.Feed, cfg.MaxTPS, cfg.MaxBurst),
		wake:  make(chan struct{}, 1),
		bids:  make([]resting, cfg.Tiers),
		asks:  make([]resting, cfg.Tiers),
		ticks: map[string]int{},
	}
}

func (q *quoter) signal() {
	select {
	case q.wake <- struct{}{}:
	default: // a requote is already pending; it will pick up the latest state
	}
}

// onBBO tracks top of book. Payload: "<ts> <FEED> <bid_px> <bid_vol> <ask_px> <ask_vol>",
// with "-" for an empty side.
func (q *quoter) onBBO(m *nats.Msg) {
	f := strings.Fields(string(m.Data))
	if len(f) < 6 {
		return
	}
	top := f[2] + " " + f[3] + " " + f[4] + " " + f[5]

	q.mu.Lock()
	if top == q.lastTop {
		q.mu.Unlock()
		return // genuinely unchanged: ignore, or we churn ids and TPS for nothing
	}
	q.lastTop = top
	q.bidPx, q.bidOK = parsePx(f[2])
	q.askPx, q.askOK = parsePx(f[4])
	if q.bidOK && q.askOK {
		mid := float64(q.bidPx+q.askPx) / 2
		q.lastMark = mid
		q.haveMark = true
		q.pushMid(mid)
	}
	q.mu.Unlock()

	q.signal()
}

// onAnyBBO watches every contract's top of book so the leading one can be picked
// by observation. Feeds on the same underlying share a two-character prefix
// (PROTOCOL.md), which is what makes them comparable at all.
//
// The rule: price off the busiest sibling, unless that is us. Activity is a
// reasonable proxy for which contract the market moves first, and the
// "unless that is us" clause matters -- quoting the most active contract and
// then pricing it off a quieter sibling would import lag rather than remove it,
// which is the exact mistake this is meant to avoid.
func (q *quoter) onAnyBBO(m *nats.Msg) {
	f := strings.Fields(string(m.Data))
	if len(f) < 6 || len(f[1]) < 2 {
		return
	}
	feed := f[1]
	if !strings.HasPrefix(feed, q.cfg.Feed[:2]) {
		return // different underlying: unrelated price
	}

	q.mu.Lock()
	q.ticks[feed]++
	best, bestN := "", q.ticks[q.cfg.Feed]
	for cand, n := range q.ticks {
		if cand != q.cfg.Feed && n > bestN {
			best, bestN = cand, n
		}
	}
	changed := best != q.refFeed
	q.refFeed = best
	if changed {
		q.basisSet, q.refOK = false, false // relearn against the new lead
	}
	lead := q.refFeed
	q.mu.Unlock()

	if lead != "" && feed == lead {
		q.onRefBBO(m)
	}
}

// onRefBBO tracks the leading instrument and the basis between it and ours.
//
// The basis is learned rather than assumed: we only know the two contracts are
// related, not by how much. It must adapt *slowly* (BasisAlpha small) -- it stands
// for the structural offset between the contracts, not the transient lag we are
// trying to trade. Learn it quickly and it absorbs the lead's move on the very
// tick we wanted to react to, and the signal vanishes. Slow adaptation also makes
// this safe if the two are unrelated: the basis converges on whatever difference
// exists and we end up quoting around our own mid.
func (q *quoter) onRefBBO(m *nats.Msg) {
	f := strings.Fields(string(m.Data))
	if len(f) < 6 {
		return
	}
	bid, bok := parsePx(f[2])
	ask, aok := parsePx(f[4])
	if !bok || !aok || ask <= bid {
		return
	}
	rm := float64(bid+ask) / 2

	q.mu.Lock()
	q.refMid, q.refOK, q.refAt = rm, true, time.Now()
	if q.bidOK && q.askOK {
		obs := float64(q.bidPx+q.askPx)/2 - rm
		if !q.basisSet {
			q.basis, q.basisSet = obs, true
		} else {
			a := q.cfg.BasisAlpha
			q.basis = a*obs + (1-a)*q.basis
		}
	}
	q.mu.Unlock()

	q.signal() // the lead moved: our fair value has moved with it
}

// fairValue is what we price around. Caller must hold q.mu.
//
// Preference is the reference instrument plus the learned basis, because it moves
// first; our own mid is the fallback. In the sample market the back months are
// quoted at `front_fair + offset` by everyone else (sim/market.py:167), so a
// quoter pricing off its own stale book is repriced last and picked off first.
func (q *quoter) fairValue() float64 {
	own := float64(q.bidPx+q.askPx) / 2
	if !q.cfg.UseRef || !q.refOK || !q.basisSet {
		return own
	}
	if q.refFeed == "" {
		return own // we are the most active contract: we are the lead
	}
	if time.Since(q.refAt) > q.cfg.RefStale {
		return own // the lead went quiet; trust what we can see
	}
	return q.refMid + q.basis
}

func parsePx(s string) (int, bool) {
	if s == "-" {
		return 0, false
	}
	v, err := strconv.Atoi(s)
	return v, err == nil
}

// onOwnMD consumes ex.md.<FEED>.<SENDER> -- our own order and fill events. This
// is the authoritative position: it comes from the exchange, not from our own
// belief about what our orders did.
func (q *quoter) onOwnMD(m *nats.Msg) {
	f := strings.Fields(string(m.Data))
	if len(f) < 2 {
		return
	}
	switch f[1] {
	case "E", "T":
		// <ts> <E|T> <incoming:17> <resting:17> <volume> <price> <matchid> <B|S>
		//
		// Not duplicates: our subject carries E when we were the resting side of a
		// match and T when we crossed. We are built to stay passive, so E is the
		// normal case, but a limit order can still cross if the book moves between
		// reading the BBO and the order landing -- handling only E would silently
		// lose those fills and leave the position wrong.
		if len(f) < 8 {
			return
		}
		vol, err1 := strconv.Atoi(f[4])
		px, err2 := strconv.Atoi(f[5])
		if err1 != nil || err2 != nil {
			return
		}
		aggressor := f[7] // side of the incoming order, not necessarily ours
		var side byte
		var ourID string
		switch {
		case q.isOurs(f[2]):
			side = aggressor[0]
			ourID = orderID(f[2])
		case q.isOurs(f[3]):
			side = flip(aggressor[0]) // we were resting, so we took the other side
			ourID = orderID(f[3])
		default:
			return
		}
		q.applyFill(side, vol, px, ourID)
		q.signal()
	case "C":
		// <ts> C <id:17> -- one of our orders left the book
		if len(f) >= 3 && q.isOurs(f[2]) {
			q.forget(orderID(f[2]))
		}
	}
}

func (q *quoter) isOurs(id17 string) bool { return strings.HasPrefix(id17, q.cfg.Sender+":") }

func orderID(id17 string) string {
	if i := strings.IndexByte(id17, ':'); i >= 0 {
		return id17[i+1:]
	}
	return id17
}

func flip(side byte) byte {
	if side == 'B' {
		return 'S'
	}
	return 'B'
}

func (q *quoter) applyFill(side byte, vol, px int, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if side == 'B' {
		q.position += vol
		q.cash -= int64(vol) * int64(px)
	} else {
		q.position -= vol
		q.cash += int64(vol) * int64(px)
	}
	q.fills++
	q.lastMark, q.haveMark = float64(px), true // a real trade is a valid mark
	// Decrement the resting order this filled against. F orders fill partially
	// and so do resting limits, so a fill does not mean the order is gone.
	for _, r := range q.live() {
		if r.id == id {
			r.vol -= vol
			if r.vol <= 0 {
				*r = resting{}
			}
		}
	}
	log.Printf("fill %c %d @ %d -> pos=%d", side, vol, px, q.position)
}

func (q *quoter) forget(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.live() {
		if r.id == id {
			*r = resting{}
		}
	}
}

type midSample struct {
	at  time.Time
	mid float64
}

// pushMid records a mid and drops anything older than the volatility window.
// Caller must hold q.mu.
func (q *quoter) pushMid(mid float64) {
	now := time.Now()
	q.mids = append(q.mids, midSample{at: now, mid: mid})
	cut := now.Add(-q.cfg.VolWindow)
	i := 0
	for i < len(q.mids) && q.mids[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		q.mids = append(q.mids[:0], q.mids[i:]...)
	}
}

// volatility is the standard deviation of the mid over the recent window, in
// price units. Caller must hold q.mu.
//
// This is what makes the edge adaptive instead of fitted. A market maker's spread
// has to cover how far the price moves while it is holding, and that distance is
// a property of the market, not a constant. On the sample market a volatility-
// sized edge beat the original fixed edge of 2 by roughly an order of magnitude,
// repeatably. The specific wide value that worked there is just that market's
// move size memorised, and TASK.md says grading uses a different one: measuring
// the move and pricing off it transfers, the number does not.
func (q *quoter) volatility() float64 {
	if len(q.mids) < 3 {
		return 0
	}
	var sum float64
	for _, s := range q.mids {
		sum += s.mid
	}
	mean := sum / float64(len(q.mids))
	var sq float64
	for _, s := range q.mids {
		d := s.mid - mean
		sq += d * d
	}
	return math.Sqrt(sq / float64(len(q.mids)))
}

// edge returns the half-spread to quote, in price units. Caller must hold q.mu.
//
// The cap matters more than it sounds. Volatility is sampled at the moments we
// requote, and we requote when the market moves, so the edge is priced off
// *conditional* volatility and runs much wider than the unconditional average
// suggests: measured median quoted spread of 78 where the parameters implied ~60,
// with the cap binding regularly and the widest quotes reaching 240 against a
// market spread of 1-5.
//
// A price-relative form of this cap was tried and removed: it measured no better
// and left two knobs doing one job. See NOTES.md.
func (q *quoter) edge() float64 {
	e := q.cfg.EdgeTicks // floor: never quote tighter than this
	if q.cfg.EdgeVolMult > 0 {
		if v := q.cfg.EdgeVolMult * q.volatility(); v > e {
			e = v
		}
	}
	if q.cfg.MaxEdgeTicks > 0 && e > q.cfg.MaxEdgeTicks {
		e = q.cfg.MaxEdgeTicks
	}
	return e
}

// live returns pointers to every resting slot, both sides, all tiers.
// Caller must hold q.mu.
func (q *quoter) live() []*resting {
	out := make([]*resting, 0, len(q.bids)+len(q.asks))
	for i := range q.bids {
		out = append(out, &q.bids[i])
	}
	for i := range q.asks {
		out = append(out, &q.asks[i])
	}
	return out
}

// target is the quote we want on one side; vol == 0 means "do not quote".
type target struct {
	px  int
	vol int
}

// desired computes every tier on both sides from the book and our inventory.
//
// With two tiers the inner one is tighter and smaller: a single wide quote is
// safe against sweeps but forfeits all the calm-market flow, and fill counts fell
// to a few dozen per run because of it. The inner quote earns that flow while
// risking little; the outer keeps the size where a sweep cannot reach it cheaply.
func (q *quoter) desired() (bids, asks []target, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.bidOK || !q.askOK || q.askPx <= q.bidPx {
		return nil, nil, false // one-sided or crossed: no fair value
	}

	fair := q.fairValue()
	edge := q.edge()
	// Inventory skew: push both quotes away from the position we are carrying, so
	// the side that flattens us is the attractive one. This is what makes it a
	// liquidity strategy rather than a directional one -- the mid drifts far more
	// than the spread pays.
	//
	// Expressed as a fraction of the *current edge*, not an absolute tick count.
	// It was absolute (4 ticks) and chosen when the edge was a fixed 2, i.e. a 200%
	// lean at full inventory. Once the edge became volatility-sized and capped at
	// 20, that same 4 was a 20% lean and the quoter had all but stopped managing
	// its own inventory -- pushing the work onto the hedger, which pays the spread
	// to do what skew does for free.
	skew := q.cfg.SkewFrac * edge * float64(q.position) / float64(q.cfg.MaxPos)

	// Remaining room before the position limit, shared across tiers: the inner
	// quote is filled first, so it gets first call on the capacity.
	buyRoom := q.cfg.MaxPos - q.position
	sellRoom := q.cfg.MaxPos + q.position

	bids = make([]target, len(q.bids))
	asks = make([]target, len(q.asks))
	for i := range bids {
		eFrac, sFrac := 1.0, 1.0
		if len(bids) > 1 && i == 0 {
			eFrac, sFrac = q.cfg.InnerEdgeFrac, q.cfg.InnerSizeFrac
		}
		e := edge * eFrac
		b := target{px: int(math.Round(fair - e - skew))}
		a := target{px: int(math.Round(fair + e - skew))}

		// Minimum-edge floor. Skew is allowed to make the side that flattens us
		// more attractive, but never past the point where the fill stops being
		// profitable. Paying to reduce inventory is the hedger's job.
		//
		// Only the attractive side is floored. The discouraging side stays
		// unbounded -- quoting stingier than fair is always safe.
		if cap := int(math.Floor(fair - q.cfg.MinEdgeTicks)); b.px > cap {
			b.px = cap
		}
		if floor := int(math.Ceil(fair + q.cfg.MinEdgeTicks)); a.px < floor {
			a.px = floor
		}

		// Prices must sit on the instrument's tick grid, rounding always away
		// from fair -- bid down, ask up -- so the minimum edge survives it.
		// Untested against a live tick>1 book: the local exchange lists only
		// tick=1, so this path rests on EX_META and the reject probe alone.
		if t := q.cfg.TickSize; t > 1 {
			b.px = floorToTick(b.px, t)
			a.px = ceilToTick(a.px, t)
		}

		size := int(math.Round(float64(q.cfg.Clip) * sFrac))
		b.vol = clamp(size, 0, buyRoom)
		a.vol = clamp(size, 0, sellRoom)
		buyRoom -= b.vol
		sellRoom -= a.vol

		// Stay passive: a limit that crosses executes immediately, making us the
		// aggressor and paying the spread we are trying to earn.
		if b.px >= q.askPx {
			b.vol = 0
		}
		if a.px <= q.bidPx {
			a.vol = 0
		}
		bids[i], asks[i] = b, a
	}
	return bids, asks, true
}

// floorToTick snaps a price down to the grid; negative prices are legal here
// (probed: the band is ref +/- band, and bids below zero are accepted), so this
// floors mathematically rather than truncating toward zero.
func floorToTick(px, tick int) int {
	m := px % tick
	if m < 0 {
		m += tick
	}
	return px - m
}

func ceilToTick(px, tick int) int {
	f := floorToTick(px, tick)
	if f == px {
		return px
	}
	return f + tick
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// requoteLoop owns all order entry, so network calls never block the NATS
// callbacks that feed us market data.
func (q *quoter) requoteLoop(ctx context.Context) {
	// A slow tick refreshes quotes even when the top of book is static, so a
	// dropped reply or a missed event cannot leave us un-quoted indefinitely.
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		case <-tick.C:
		}
		q.requote()
		if q.cfg.MinRequote > 0 {
			time.Sleep(q.cfg.MinRequote) // coalesce bursts of book updates
		}
	}
}

func (q *quoter) requote() {
	q.quoteMu.Lock()
	defer q.quoteMu.Unlock()

	bids, asks, ok := q.desired()
	if !ok {
		q.cancelAll()
		return
	}
	for i := range bids {
		q.reconcile(&q.bids[i], bids[i], 'B')
		q.reconcile(&q.asks[i], asks[i], 'S')
	}
}

// reconcile brings one side to its target, leaving an already-correct order
// alone so it keeps its place in the queue.
func (q *quoter) reconcile(cur *resting, want target, side byte) {
	q.mu.Lock()
	now := *cur
	q.mu.Unlock()

	if now.live() && now.px == want.px && now.vol == want.vol {
		return
	}
	if now.live() {
		if err := q.cl.cancel(now.id); err != nil {
			log.Printf("cancel %s: %v", now.id, err)
			return // leave it recorded; we retry next cycle rather than orphan it
		}
		q.mu.Lock()
		if cur.id == now.id {
			*cur = resting{}
		}
		q.mu.Unlock()
	}
	if want.vol <= 0 {
		return
	}
	id, traded, err := q.cl.add(side, want.vol, want.px, 'L')
	if err != nil {
		log.Printf("add %c %d @ %d: %v", side, want.vol, want.px, err)
		return
	}
	q.mu.Lock()
	// Anything that traded on entry is reported on our md feed and booked there;
	// only the remainder is actually resting.
	if rem := want.vol - traded; rem > 0 {
		*cur = resting{id: id, px: want.px, vol: rem}
	}
	q.mu.Unlock()
}

func (q *quoter) cancelAll() {
	q.mu.Lock()
	all := q.live()
	q.mu.Unlock()
	for _, r := range all {
		q.mu.Lock()
		cur := *r
		q.mu.Unlock()
		if !cur.live() {
			continue
		}
		if err := q.cl.cancel(cur.id); err != nil {
			log.Printf("cancel %s: %v", cur.id, err)
			continue
		}
		q.mu.Lock()
		if r.id == cur.id {
			*r = resting{}
		}
		q.mu.Unlock()
	}
}

// status reports position and mark-to-market PnL once a second, mirroring the
// taker's strat.<sender>.status convention.
// markPrice values inventory. Preference order: the live mid; the side we would
// actually have to trade against to get flat; the last price we saw. Staleness is
// reported to the caller rather than hidden.
//
// The previous version added position*mid only when the book happened to be
// two-sided, and otherwise reported raw cash as PnL. In the full-stack run the
// book emptied and it printed pnl=9065 while short 14 lots; the real figure was
// about -385. A valuation that silently drops the position is worse than none.
//
// Caller must hold q.mu.
func (q *quoter) markPrice() (px float64, fresh, ok bool) {
	switch {
	case q.bidOK && q.askOK:
		return float64(q.bidPx+q.askPx) / 2, true, true
	case q.position > 0 && q.bidOK:
		return float64(q.bidPx), true, true // long: we would have to sell into the bid
	case q.position < 0 && q.askOK:
		return float64(q.askPx), true, true // short: we would have to buy from the ask
	case q.bidOK:
		return float64(q.bidPx), true, true
	case q.askOK:
		return float64(q.askPx), true, true
	case q.haveMark:
		return q.lastMark, false, true // no book at all: last price we saw
	}
	return 0, false, false
}

// liqPrice values inventory at what it would actually fetch if closed right now:
// a long sells into the bid, a short buys from the ask. markPrice uses the mid,
// which is the fair value of the position but not what you would receive for it.
// The session ends by liquidating whatever is left against the book, so this is
// the number that survives contact with the close. Caller must hold q.mu.
func (q *quoter) liqPrice() (float64, bool) {
	switch {
	case q.position > 0 && q.bidOK:
		return float64(q.bidPx), true
	case q.position < 0 && q.askOK:
		return float64(q.askPx), true
	case q.position == 0:
		return 0, true // nothing to liquidate
	case q.haveMark:
		return q.lastMark, true // no book to hit; the best guess we have
	}
	return 0, false
}

func (q *quoter) status() string {
	q.mu.Lock()
	defer q.mu.Unlock()

	mark, fresh, ok := q.markPrice()
	pnl := "n/a" // no position can be valued and we are not flat: say so
	switch {
	case q.position == 0:
		pnl = fmt.Sprintf("%d", q.cash) // nothing to mark; cash is the whole story
	case ok:
		pnl = fmt.Sprintf("%.0f", float64(q.cash)+float64(q.position)*mark)
		if !fresh {
			pnl += "?" // valued against a stale price -- flagged, not hidden
		}
	}
	liq := "n/a"
	if px, lok := q.liqPrice(); lok {
		liq = fmt.Sprintf("%.0f", float64(q.cash)+float64(q.position)*px)
	}
	return fmt.Sprintf("pos=%d cash=%d pnl=%s liq=%s fills=%d bid=%s ask=%s",
		q.position, q.cash, pnl, liq, q.fills, fmtQuote(best(q.bids, true)),
		fmtQuote(best(q.asks, false)))
}

// best returns the tightest live quote on a side: the highest bid, the lowest
// ask. That is the price we are actually showing the market, which is what the
// status line and the benchmark harness care about.
func best(rs []resting, highest bool) resting {
	var out resting
	for _, r := range rs {
		if !r.live() {
			continue
		}
		if !out.live() || (highest && r.px > out.px) || (!highest && r.px < out.px) {
			out = r
		}
	}
	return out
}

func fmtQuote(r resting) string {
	if !r.live() {
		return "-"
	}
	return fmt.Sprintf("%dx%d", r.px, r.vol)
}

func (q *quoter) reportLoop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s := q.status()
			log.Print(s)
			_ = q.nc.Publish("strat."+q.cfg.Sender+".status", []byte(s))
		}
	}
}
