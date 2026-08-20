// Order entry: everything about talking to the exchange, kept away from the
// strategy. Order-id generation, self-imposed rate limiting, reply parsing, and
// the reject codes that mean "already done" rather than "failed".

package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// meta is one instrument's EX_META record: the exchange's own statement of the
// contract's trading parameters. The seats had never read it -- every limit in
// the desk was a hardcoded guess, which is wrong in both directions: a rate cap
// guessed too high gets the sender disconnected in a market with a lower limit,
// and one guessed too low leaves the quoter reacting at a fraction of the
// market's real cadence, which is what being picked off is.
type meta struct {
	tickSize      int
	maxTPS        int
	positionLimit int
}

// parseMeta reads the KV value: space-separated key=value pairs, integers
// throughout (PROTOCOL.md "Instrument metadata").
func parseMeta(s string) meta {
	m := meta{tickSize: 1}
	for _, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			continue
		}
		switch k {
		case "ticksize":
			if n > 0 {
				m.tickSize = n
			}
		case "max_tps":
			m.maxTPS = n
		case "position_limit":
			m.positionLimit = n
		}
	}
	return m
}

// fetchMeta reads the feed's EX_META entry, retrying briefly because on a fresh
// stack the exchange may not have created the bucket before this seat connects.
func fetchMeta(nc *nats.Conn, feed string, wait time.Duration) (meta, error) {
	js, err := nc.JetStream()
	if err != nil {
		return meta{tickSize: 1}, err
	}
	deadline := time.Now().Add(wait)
	for {
		kv, kerr := js.KeyValue("EX_META")
		if kerr == nil {
			if e, gerr := kv.Get(feed); gerr == nil {
				return parseMeta(string(e.Value())), nil
			} else {
				kerr = gerr
			}
		}
		if time.Now().After(deadline) {
			return meta{tickSize: 1}, kerr
		}
		time.Sleep(time.Second)
	}
}

// reply is a parsed exchange response: "<TAG> Y <n>" or "<TAG> N <code> <text>".
type reply struct {
	ok   bool
	n    int // for A: volume traded immediately; for C/X: orders cancelled
	code int
	text string
}

func (r reply) String() string {
	if r.ok {
		return fmt.Sprintf("Y %d", r.n)
	}
	return fmt.Sprintf("N %d %s", r.code, r.text)
}

// ids allocates order ids for one sender across every contract it trades.
//
// Shared deliberately: ids are consumed permanently *per sender*, not per feed,
// so a quoter running several contracts must draw from one sequence. Two clients
// each seeding from the wall clock would start within a millisecond of each other
// and collide immediately -- and the collision is a silent 203, not a crash.
type ids struct {
	mu   sync.Mutex
	next uint64
}

// newIDs seeds from the clock so a container restart resumes above everything the
// previous run burned. Milliseconds since epoch (~1.8e12) fits inside the
// 36^8 (~2.8e12) the protocol's 8 characters allow, good until ~2059.
func newIDs() *ids {
	return &ids{next: uint64(time.Now().UnixMilli())}
}

// next8 returns a fresh 8-char id. Monotonic, never recycled.
func (g *ids) next8() string {
	g.mu.Lock()
	n := g.next
	g.next++
	g.mu.Unlock()
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = base36[n%36]
		n /= 36
	}
	return string(b[:])
}

// client owns order entry for one sender on one contract: rate limiting and
// reply parsing. Order ids come from a sender-wide allocator it does not own.
type client struct {
	nc     *nats.Conn
	sender string
	feed   string
	subj   string // ex.req.<sender> -- the exchange listens on ex.req.>, and a
	// request to bare "ex.req" silently gets no responders (the bug in taker.py).

	mu  sync.Mutex
	ids *ids

	// Token bucket, not fixed spacing between requests. Repricing both sides is
	// cancel+add twice, and with a fixed 1/maxTPS gap that serialised into ~200ms
	// of forced waiting while sitting on stale quotes -- despite the measured
	// sustained rate being 8/sec against a budget of 20. A bucket lets a burst
	// through immediately and still bounds the sustained rate, which is the thing
	// that actually risks the disconnect.
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time
}

func newClient(nc *nats.Conn, sender, feed string, maxTPS, maxBurst int, g *ids) *client {
	return &client{
		ids:    g,
		nc:     nc,
		sender: sender,
		feed:   feed,
		subj:   "ex.req." + sender,
		rate:   float64(maxTPS),
		burst:  float64(maxBurst),
		tokens: float64(maxBurst), // start full: the first reposition should not wait
	}
}

// reserve takes one token and returns how long the caller must wait for it.
// Letting tokens go negative is deliberate: it queues concurrent callers instead
// of letting them all decide independently that they may go now.
func (c *client) reserve() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rate <= 0 {
		return 0 // unlimited
	}
	now := time.Now()
	if c.last.IsZero() {
		c.last = now
	}
	c.tokens += now.Sub(c.last).Seconds() * c.rate
	if c.tokens > c.burst {
		c.tokens = c.burst
	}
	c.last = now
	c.tokens--
	if c.tokens >= 0 {
		return 0
	}
	return time.Duration(-c.tokens / c.rate * float64(time.Second))
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

func (c *client) newOrderID() string { return c.ids.next8() }

// request rate-limits, sends, and parses. Exceeding the exchange's per-feed
// max_tps disconnects the sender outright (changelog v2.3) rather than merely
// rejecting, so we throttle ourselves regardless of what the local
// instruments file says the limit is.
func (c *client) request(msg string) (reply, error) {
	if wait := c.reserve(); wait > 0 {
		time.Sleep(wait)
	}

	m, err := c.nc.Request(c.subj, []byte(msg), time.Second)
	if err != nil {
		return reply{}, fmt.Errorf("request %q: %w", msg, err)
	}
	return parseReply(string(m.Data))
}

func parseReply(s string) (reply, error) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return reply{}, fmt.Errorf("short reply %q", s)
	}
	switch f[1] {
	case "Y":
		n := 0
		if len(f) >= 3 {
			n, _ = strconv.Atoi(f[2])
		}
		return reply{ok: true, n: n}, nil
	case "N":
		r := reply{}
		if len(f) >= 3 {
			r.code, _ = strconv.Atoi(f[2])
		}
		if len(f) >= 4 {
			r.text = strings.Join(f[3:], " ")
		}
		return r, nil
	}
	return reply{}, fmt.Errorf("unparsable reply %q", s)
}

// add places an order and returns its id and the volume that traded immediately.
func (c *client) add(side byte, vol, px int, typ byte) (string, int, error) {
	id := c.newOrderID()
	msg := fmt.Sprintf("%s A %s %s %c %d %d %c", c.sender, c.feed, id, side, vol, px, typ)
	r, err := c.request(msg)
	if err != nil {
		return "", 0, err
	}
	if !r.ok {
		return "", 0, fmt.Errorf("add rejected: %s", r)
	}
	return id, r.n, nil
}

// Reject codes that both mean "this order is not resting on the book", which is
// precisely the state cancel() is trying to reach. Distinguished by experiment:
//
//	206 orderid not used   -- this sender never used the id at all
//	305 orderid not active -- the id was used, but the order has since been
//	                          cancelled or filled
//
// Neither is an error for us. 305 shows up routinely in the full-stack run when a
// fill and a requote race: the fill removes the order a moment before we ask to
// cancel it. Treating it as a failure made reconcile bail out while still holding
// a record of an order that no longer exists, so that side stopped being requoted
// until an unrelated C message happened to clear it.
const (
	rejectOrderIDUnused = 206
	rejectOrderInactive = 305
)

// cancel removes one resting order by id.
//
// Deliberately not using X (cancel-many): it does not reliably select by
// side+price. Observed two of our own orders resting at 585 while
// "X AAH6 B 585" reported 0 cancelled, and cancelling each by id immediately
// afterwards worked. Per-id cancellation costs one request each, which the rate
// limiter above absorbs.
func (c *client) cancel(id string) error {
	r, err := c.request(fmt.Sprintf("%s C %s %s", c.sender, c.feed, id))
	if err != nil {
		return err
	}
	if !r.ok && r.code != rejectOrderIDUnused && r.code != rejectOrderInactive {
		return fmt.Errorf("cancel %s rejected: %s", id, r)
	}
	return nil
}
