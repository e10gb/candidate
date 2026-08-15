package main

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

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

// client owns order entry for one sender: id generation, self-imposed rate
// limiting, and reply parsing.
type client struct {
	nc     *nats.Conn
	sender string
	feed   string
	subj   string // ex.req.<sender> -- the exchange listens on ex.req.>, and a
	// request to bare "ex.req" silently gets no responders (the bug in taker.py).

	mu       sync.Mutex
	nextID   uint64
	interval time.Duration // minimum gap between requests
	last     time.Time
}

func newClient(nc *nats.Conn, sender, feed string, maxTPS int) *client {
	interval := time.Duration(0)
	if maxTPS > 0 {
		interval = time.Second / time.Duration(maxTPS)
	}
	return &client{
		nc:     nc,
		sender: sender,
		feed:   feed,
		subj:   "ex.req." + sender,
		// Order ids are consumed permanently per sender -- cancelling does not
		// free them (reject 203). Seeding from the wall clock means a container
		// restart resumes above every id the previous run burned. Milliseconds
		// since epoch is ~1.8e12, comfortably inside the 36^8 = 2.8e12 that fits
		// in the protocol's 8 characters (good until ~2059).
		nextID:   uint64(time.Now().UnixMilli()),
		interval: interval,
	}
}

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

// newOrderID returns a fresh 8-char id. Monotonic, never recycled.
func (c *client) newOrderID() string {
	c.nextID++
	n := c.nextID
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = base36[n%36]
		n /= 36
	}
	return string(b[:])
}

// request rate-limits, sends, and parses. Exceeding the exchange's per-feed
// max_tps disconnects the sender outright (changelog v2.3) rather than merely
// rejecting, so we throttle ourselves regardless of what the local
// instruments file says the limit is.
func (c *client) request(msg string) (reply, error) {
	c.mu.Lock()
	if c.interval > 0 {
		if wait := c.interval - time.Since(c.last); wait > 0 {
			time.Sleep(wait)
		}
		c.last = time.Now()
	}
	c.mu.Unlock()

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
	// 206 "orderid not used" means it is already gone (filled or cancelled);
	// that is the state we wanted, so it is not an error.
	if !r.ok && r.code != 206 {
		return fmt.Errorf("cancel %s rejected: %s", id, r)
	}
	return nil
}
