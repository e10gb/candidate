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

	// While set, we are out of the market entirely: a move large enough to be
	// informed just happened and quoting into it is how we get picked off.
	pullUntil time.Time
	// Recent reference-contract mids. The lead moves before our book does, so a
	// move here is advance warning; our own mid moving is not -- by then the
	// repricing orders have already crossed us.
	refMids []midSample

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

	bid resting
	ask resting

	quoteMu sync.Mutex    // serialises requote cycles
	wake    chan struct{} // coalescing signal: capacity 1
}

func newQuoter(nc *nats.Conn, cfg config, g *ids) *quoter {
	return &quoter{
		cfg:   cfg,
		nc:    nc,
		cl:    newClient(nc, cfg.Sender, cfg.Feed, cfg.MaxTPS, cfg.MaxBurst, g),
		wake:  make(chan struct{}, 1),
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
		q.checkPull(mid)
	}
	q.mu.Unlock()

	q.signal()
}

// onAnyBBO watches every contract's top of book so the leading one can be picked
// by observation. Same underlying = same two-character prefix (PROTOCOL.md).
//
// Rule: price off the busiest sibling, unless that is us -- pricing the most
// active contract off a quieter one would import lag rather than remove it.
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
// The basis is learned, not assumed, and must adapt *slowly*: it stands for the
// structural offset between the contracts, not the lag we are trying to trade.
// Learn it fast and it absorbs the lead's move on the very tick we wanted to
// react to. Slow adaptation is also what makes an unrelated "lead" harmless.
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
	q.checkPullRef(rm)
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

// fairValue is what we price around: the reference contract plus the learned
// basis when one leads, our own mid otherwise. Caller must hold q.mu.
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
		// Not duplicates: E when we were the resting side, T when we crossed. We
		// stay passive so E is normal, but a limit can still cross if the book
		// moves before it lands -- handling only E loses those fills silently.
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

// volatility is the stdev of the mid over the recent window, in price units.
// Caller must hold q.mu.
//
// This makes the edge adaptive rather than fitted: a spread has to cover how far
// the price moves while you hold, which is a property of the market. A fixed
// number would just be this sim's move size memorised.
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

// checkPullRef is the same guard driven by the *leading* contract, which is the
// point: our own mid moving is not a warning, because by then the repricing
// orders have already traded with us. The lead moving is. Caller must hold q.mu.
func (q *quoter) checkPullRef(mid float64) {
	if q.cfg.PullMove <= 0 {
		return
	}
	now := time.Now()
	q.refMids = append(q.refMids, midSample{at: now, mid: mid})
	cut := now.Add(-q.cfg.PullWindow)
	i := 0
	for i < len(q.refMids) && q.refMids[i].at.Before(cut) {
		i++
	}
	if i > 0 {
		q.refMids = append(q.refMids[:0], q.refMids[i:]...)
	}
	// Same rule as checkPull: without a sample a full window old we cannot judge
	// a move over that window, and guessing from a much older one re-triggers
	// the pull indefinitely.
	var ref float64
	found := false
	for _, s := range q.refMids {
		if s.at.After(cut) {
			break
		}
		ref, found = s.mid, true
	}
	if !found {
		return
	}
	if math.Abs(mid-ref) >= q.cfg.PullMove {
		q.pullUntil = now.Add(q.cfg.PullFor)
	}
}

// checkPull takes us out of the market after a sharp move. Caller must hold q.mu.
//
// Pulling, not widening: widening changes the target price, so reconcile cancels
// and re-adds, and the re-add drops a fresh order into the middle of the move --
// measured worse. Pulling has no such failure mode; there is nothing left to hit.
func (q *quoter) checkPull(mid float64) {
	if q.cfg.PullMove <= 0 || len(q.mids) < 2 {
		return
	}
	// The reference is the mid as it was one PullWindow ago. If no sample is that
	// old we cannot measure a move over the window and must not guess: falling
	// back to the oldest sample in the buffer (up to VolWindow, 10x longer) would
	// compare against a far older price, read a much larger "move" than really
	// occurred, and re-trigger the pull indefinitely.
	cut := time.Now().Add(-q.cfg.PullWindow)
	var ref float64
	found := false
	for _, s := range q.mids {
		if s.at.After(cut) {
			break
		}
		ref, found = s.mid, true
	}
	if !found {
		return
	}
	if move := math.Abs(mid - ref); move >= q.cfg.PullMove {
		q.pullUntil = time.Now().Add(q.cfg.PullFor)
	}
}

// edge returns the half-spread to quote, in price units. Caller must hold q.mu.
//
// The cap matters: volatility is sampled when we requote, and we requote when the
// market moves, so the edge is priced off *conditional* volatility and runs wider
// than the parameters imply -- uncapped it reached a quoted spread of 240 against
// a market spread of 1-5.
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

// live returns our resting slots. Caller must hold q.mu.
func (q *quoter) live() []*resting { return []*resting{&q.bid, &q.ask} }

// target is the quote we want on one side; vol == 0 means "do not quote".
type target struct {
	px  int
	vol int
}

// desired computes both sides from the book and our inventory.
func (q *quoter) desired() (bid, ask target, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.bidOK || !q.askOK || q.askPx <= q.bidPx {
		return target{}, target{}, false // one-sided or crossed: no fair value
	}
	if time.Now().Before(q.pullUntil) {
		return target{}, target{}, false // a sharp move just landed: stay out
	}

	fair := q.fairValue()
	edge := q.edge()
	// Lean against inventory so the flattening side is the attractive one, as a
	// fraction of the current edge -- an absolute tick count stops meaning
	// anything once the edge is volatility-sized.
	skew := q.cfg.SkewFrac * edge * float64(q.position) / float64(q.cfg.MaxPos)
	bid.px = int(math.Round(fair - edge - skew))
	ask.px = int(math.Round(fair + edge - skew))

	// Minimum-edge floor: skew may make the flattening side attractive, never
	// unprofitable. Clearing inventory at a loss is the hedger's job. Only the
	// attractive side is floored; quoting stingier than fair is always safe.
	if cap := int(math.Floor(fair - q.cfg.MinEdgeTicks)); bid.px > cap {
		bid.px = cap
	}
	if floor := int(math.Ceil(fair + q.cfg.MinEdgeTicks)); ask.px < floor {
		ask.px = floor
	}
	// Prices sit on the instrument's tick grid, rounded away from fair so the
	// minimum edge survives it. Untested against a live tick>1 book: the local
	// exchange lists only tick=1.
	if t := q.cfg.TickSize; t > 1 {
		bid.px = floorToTick(bid.px, t)
		ask.px = ceilToTick(ask.px, t)
	}

	// Never quote a size that could breach the position limit.
	bid.vol = clamp(q.cfg.Clip, 0, q.cfg.MaxPos-q.position)
	ask.vol = clamp(q.cfg.Clip, 0, q.cfg.MaxPos+q.position)

	// Stay passive: a limit that crosses executes immediately, making us the
	// aggressor and paying the spread we are trying to earn. With a positive
	// MinEdgeTicks the floor above already guarantees this, so this is a guard.
	if bid.px >= q.askPx {
		bid.vol = 0
	}
	if ask.px <= q.bidPx {
		ask.vol = 0
	}
	return bid, ask, true
}

// floorToTick snaps a price down to the grid. Negative prices are legal here
// (probed), so this floors mathematically rather than truncating toward zero.
func floorToTick(px, tick int) int {
	m := px % tick
	if m < 0 {
		m += tick
	}
	return px - m
}

func ceilToTick(px, tick int) int {
	if f := floorToTick(px, tick); f != px {
		return f + tick
	}
	return px
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

// requoteLoop owns all order entry, so network calls never block the market-data
// callbacks.
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

	bid, ask, ok := q.desired()
	if !ok {
		q.cancelAll()
		return
	}
	q.reconcile(&q.bid, bid, 'B')
	q.reconcile(&q.ask, ask, 'S')
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
// markPrice values inventory: the live mid, else the side we would have to trade
// against to get flat, else the last price seen. Staleness is reported rather
// than hidden -- a valuation that silently drops the position reads as profit.
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

// liqPrice values inventory at what it would fetch if closed now -- long sells
// into the bid, short buys from the ask. The session ends by liquidating, so this
// is the number that survives the close. Caller must hold q.mu.
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
	// Which contract we are actually pricing off. Reference pricing once switched
	// itself off for a whole sweep -- a deleted config assignment, so UseRef held
	// Go's zero value -- and nothing in the logs would have shown it. "ref=own"
	// is a real state (we are the busiest contract, so we are the lead); "ref=off"
	// means the feature is disabled, which should never be true by default.
	ref := "off"
	switch {
	case !q.cfg.UseRef: // disabled: never true by default, see TestConfigFromEnv
	case q.refFeed == "":
		ref = "own" // we are the busiest contract, so we are the lead
	case !q.refOK || !q.basisSet:
		ref = q.refFeed + "?" // a lead exists but we cannot use it yet
	case time.Since(q.refAt) > q.cfg.RefStale:
		ref = q.refFeed + "!stale"
	default:
		ref = fmt.Sprintf("%s%+.0f", q.refFeed, q.basis)
	}
	return fmt.Sprintf("%s pos=%d cash=%d pnl=%s liq=%s fills=%d ref=%s bid=%s ask=%s",
		q.cfg.Feed, q.position, q.cash, pnl, liq, q.fills, ref,
		fmtQuote(q.bid), fmtQuote(q.ask))
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
