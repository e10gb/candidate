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

	bid resting
	ask resting

	quoteMu sync.Mutex   // serialises requote cycles
	wake    chan struct{} // coalescing signal: capacity 1
}

func newQuoter(nc *nats.Conn, cfg config) *quoter {
	return &quoter{
		cfg:  cfg,
		nc:   nc,
		cl:   newClient(nc, cfg.Sender, cfg.Feed, cfg.MaxTPS),
		wake: make(chan struct{}, 1),
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
		q.lastMark = float64(q.bidPx+q.askPx) / 2
		q.haveMark = true
	}
	q.mu.Unlock()

	q.signal()
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
	for _, r := range []*resting{&q.bid, &q.ask} {
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
	for _, r := range []*resting{&q.bid, &q.ask} {
		if r.id == id {
			*r = resting{}
		}
	}
}

// target is the quote we want on one side; vol == 0 means "do not quote".
type target struct {
	px  int
	vol int
}

// desired computes both sides from the current book and our inventory.
func (q *quoter) desired() (bid, ask target, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.bidOK || !q.askOK || q.askPx <= q.bidPx {
		return target{}, target{}, false // one-sided or crossed: no fair value
	}

	fair := float64(q.bidPx+q.askPx) / 2
	// Inventory skew: push both quotes away from the position we are carrying, so
	// the side that flattens us is the attractive one. At MaxPos the shift is a
	// full SkewTicks, which is what makes this a liquidity strategy rather than a
	// directional one -- the mid drifts far more than the spread pays.
	skew := q.cfg.SkewTicks * float64(q.position) / float64(q.cfg.MaxPos)
	bid.px = int(math.Round(fair - q.cfg.EdgeTicks - skew))
	ask.px = int(math.Round(fair + q.cfg.EdgeTicks - skew))

	// Minimum-edge floor. Skew is allowed to make the side that flattens us more
	// attractive, but never past the point where the fill stops being profitable:
	// with SkewTicks > EdgeTicks the raw skew above quotes straight through fair
	// value once the position passes MaxPos*EdgeTicks/SkewTicks, so we were paying
	// to reduce inventory. Clearing inventory at a loss is the hedger's job, and it
	// can do it in one trade instead of waiting for someone to come to us.
	//
	// Only the attractive side is floored. The discouraging side stays unbounded --
	// quoting stingier than fair is always safe.
	if cap := int(math.Floor(fair - q.cfg.MinEdgeTicks)); bid.px > cap {
		bid.px = cap
	}
	if floor := int(math.Ceil(fair + q.cfg.MinEdgeTicks)); ask.px < floor {
		ask.px = floor
	}

	// Never quote a size that could take us through the position limit.
	bid.vol = clamp(q.cfg.Clip, 0, q.cfg.MaxPos-q.position)
	ask.vol = clamp(q.cfg.Clip, 0, q.cfg.MaxPos+q.position)

	// Stay passive: a limit order that crosses executes immediately, which would
	// make us the aggressor and pay the spread we are trying to earn. With a
	// positive MinEdgeTicks the floor above already guarantees this (our bid sits
	// below fair, which sits below the best ask), so this is a guard rather than a
	// reprice -- if it ever fires, sitting the side out is right, since any price
	// that satisfies it would breach the minimum edge. Applied last so it is not
	// undone by the sizing above.
	if bid.px >= q.askPx {
		bid.vol = 0
	}
	if ask.px <= q.bidPx {
		ask.vol = 0
	}
	return bid, ask, true
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
	for _, r := range []*resting{&q.bid, &q.ask} {
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
	return fmt.Sprintf("pos=%d cash=%d pnl=%s fills=%d bid=%s ask=%s",
		q.position, q.cash, pnl, q.fills, fmtQuote(q.bid), fmtQuote(q.ask))
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
