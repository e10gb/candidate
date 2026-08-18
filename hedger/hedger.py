#!/usr/bin/env python3
"""Desk hedger.

Keeps the desk's *combined* position -- quoter + taker + hedger, summed across
every contract they trade -- near zero, hedging in the most liquid one.
When the other seats accumulate exposure, this reduces it. It is the seat that is
allowed to pay for speed -- a small position held for a while is cheap, a large one
held for a second is not -- but it only pays when it has to: modest exposure is
first worked passively at the touch, and only crossed if that does not fill in
HEDGE_PASSIVE_MS or the position grows past HEDGE_URGENT.

Two decisions worth stating up front, both forced by what we found in the exchange
(see NOTES.md):

1. Positions are derived from the exchange's own fill feed
   (`ex.md.<FEED>.<sender>`), never from a seat's self-reported
   `strat.<sender>.status`. The shipped taker reports `pos=0` while being unable to
   trade at all, and books sells as buys once it can. A hedger that trusts its
   siblings' bookkeeping inherits their bugs; the exchange cannot be wrong about
   what it matched.

2. `F` (fill-and-kill) orders fill *partially*, despite what CHANGELOG.md v2.3 and
   sim/market.py both claim. Every send therefore reads the traded volume back off
   the reply and the remainder is re-hedged on the next tick.

Config (env):
  NATS_URL         default nats://127.0.0.1:4222
  HEDGER_SENDER    our sender tag               (default HEDGE001)
  SENDER           the quoter's sender          (default QUOTE001)
  TAKER_SENDER     the taker's sender           (default PYTKR001)
  HEDGER_FEED      contract to hedge            (default AAH6, or TAKER_FEED)
  HEDGE_THRESH     desk position that triggers a hedge   (default 5)
  HEDGE_CLIP       max size per hedge order              (default 25)
  HEDGE_SLIP       ticks through the touch we will pay   (default 10)
  HEDGE_INTERVAL   seconds between checks                (default 0.05)
  HEDGE_MAX_TPS    self-imposed request rate cap         (default 20; lowered
                   automatically if the exchange's EX_META declares a tighter
                   max_tps -- a disconnected hedger is no risk control at all)
  HEDGE_URGENT     exposure at/above which we cross at once (default 15)
  HEDGE_PASSIVE_MS how long to rest before crossing        (default 300)
  HEDGE_PASSIVE_IMPROVE  ticks to improve on the touch     (default 1)
"""
import asyncio
import os
import time

import nats

NATS_URL = os.environ.get("NATS_URL", "nats://127.0.0.1:4222")
SENDER = os.environ.get("HEDGER_SENDER", "HEDGE001")
FEED = os.environ.get("HEDGER_FEED") or os.environ.get("TAKER_FEED", "AAH6")
SEATS = {
    "quoter": os.environ.get("SENDER", "QUOTE001"),
    "taker": os.environ.get("TAKER_SENDER", "PYTKR001"),
    "hedger": SENDER,
}
def env(key, default, cast):
    """Read a numeric setting. Treats unset and empty the same, so a compose
    pass-through like `HEDGE_THRESH: ${HEDGE_THRESH:-}` falls back to the default
    rather than crashing on int("")."""
    raw = os.environ.get(key, "")
    if raw == "":
        return default
    try:
        return cast(raw)
    except ValueError:
        print(f"[hedger] bad {key}={raw!r}; using {default}", flush=True)
        return default


THRESH = env("HEDGE_THRESH", 5, int)
CLIP = env("HEDGE_CLIP", 25, int)
SLIP = env("HEDGE_SLIP", 10, int)
INTERVAL = env("HEDGE_INTERVAL", 0.05, float)
MAX_TPS = env("HEDGE_MAX_TPS", 20, int)

# Passive-first hedging. Crossing the spread on every hedge is the desk's largest
# mechanical cost: measured around 4 price units per lot on ~200 lots per two
# minutes. Modest exposure does not need that urgency -- TASK.md's own risk model
# says a small position held for a while is cheap and a large one is not -- so we
# first rest an order at the touch and only cross if it has not filled.
#
# URGENT is the size above which we stop being patient and take the spread hit
# immediately. Below it, patience costs a little time and saves the spread.
URGENT = env("HEDGE_URGENT", 15, int)
PASSIVE_MS = env("HEDGE_PASSIVE_MS", 300, int)
# Ticks to improve on the touch when resting. 0 joins the back of the queue and
# rarely fills in the time available; 1 puts us at the front and still buys below
# the offer, so it remains far cheaper than crossing.
PASSIVE_IMPROVE = env("HEDGE_PASSIVE_IMPROVE", 1, int)
# How long a fired-but-unconfirmed hedge may keep suppressing further hedging.
# The round trip is milliseconds; a second is generous and bounds how long the
# desk can misread its own exposure if a confirmation never arrives.
INFLIGHT_TTL = env("HEDGE_INFLIGHT_TTL", 1.0, float)

BASE36 = "0123456789abcdefghijklmnopqrstuvwxyz"


def base36(n):
    """8-char order id. Ids are consumed permanently per sender -- cancelling does
    not free them (reject 203) -- so this only ever counts up. Seeding from the
    wall clock means a container restart resumes above everything the last run
    burned; ms since epoch (~1.8e12) fits inside 36^8 (~2.8e12)."""
    out = []
    for _ in range(8):
        out.append(BASE36[n % 36])
        n //= 36
    return "".join(reversed(out))


def flip(side):
    return "S" if side == "B" else "B"


def parse_meta(raw):
    """EX_META value: space-separated key=value pairs, integers throughout."""
    out = {}
    for part in raw.split():
        k, _, v = part.partition("=")
        try:
            out[k] = int(v)
        except ValueError:
            pass
    return out


async def fetch_meta(nc, feed, wait=20.0):
    """Read the feed's EX_META entry, retrying briefly: on a fresh stack the
    exchange may not have created the bucket before this seat connects."""
    deadline = time.monotonic() + wait
    while True:
        try:
            js = nc.jetstream()
            kv = await js.key_value("EX_META")
            entry = await kv.get(feed)
            return parse_meta(entry.value.decode())
        except Exception as err:
            if time.monotonic() > deadline:
                print(f"[hedger] EX_META unavailable ({err}); defaults apply", flush=True)
                return {}
            await asyncio.sleep(1.0)


def to_grid(px, tick, up):
    """Snap a price onto the instrument's tick grid. Floor division keeps
    negative prices (legal here) correct; marketable prices round through the
    touch -- up for buys, down for sells -- so rounding never strands an order
    on the wrong side of it."""
    if tick <= 1:
        return px
    base = (px // tick) * tick
    if base == px:
        return px
    return base + tick if up else base


class Hedger:
    def __init__(self, nc):
        self.nc = nc
        self.positions = {name: 0 for name in SEATS}
        # Volume we have traded but not yet seen echoed back on our own market-data
        # feed. Without this the desk looks unhedged for as long as the round trip
        # takes and we fire the same hedge repeatedly.
        self.inflight = 0
        self.inflight_at = 0.0
        self.best_bid = None
        self.best_ask = None
        self.oid = int(time.time() * 1000)
        self.hedges = 0
        self.traded = 0
        self.cash = 0
        self.last_mark = None   # last price we can value inventory against
        self.last_send = 0.0
        # A resting hedge order, if we are currently being patient:
        # {"id":..., "side":..., "at": monotonic}
        self.passive = None
        self.passive_lots = 0
        self.crossed_lots = 0
        # Cost of hedging, measured directly: how far from the mid each fill was,
        # signed so that paying up is positive. This is the number passive-first
        # hedging is meant to reduce, and unlike PnL it is mechanical rather than
        # path-dependent, so it can be compared across short runs.
        self.cost_lots = 0
        self.cost_sum = 0.0
        self.min_gap = 1.0 / MAX_TPS if MAX_TPS > 0 else 0.0
        self.clip = CLIP   # possibly clamped by the exchange's position_limit
        self.tick = 1

    # ---- market data ------------------------------------------------------- #

    async def on_bbo(self, msg):
        f = msg.data.decode().split()
        if len(f) < 6:
            return
        self.best_bid = None if f[2] == "-" else int(f[2])
        self.best_ask = None if f[4] == "-" else int(f[4])

    def make_md_handler(self, seat, sender):
        """One handler per seat, reading that seat's own fills off the exchange."""
        prefix = sender + ":"

        async def handler(msg):
            f = msg.data.decode().split()
            # <ts> <E|T> <incoming:17> <resting:17> <volume> <price> <matchid> <B|S>
            #
            # Both types matter, and they are not duplicates of each other: a seat
            # gets E on its own subject when it was the *resting* side of a match,
            # and T when it was the *aggressor*. Verified by watching every
            # ex.md.AAH6.* subject through a single trade -- the passive party's
            # subject carried only E, the aggressor's only T. Handling just E (as
            # this did at first) silently drops every fill of a seat that crosses
            # the spread, which is every fill the hedger itself makes.
            if len(f) < 8 or f[1] not in ("E", "T"):
                return
            incoming_is_ours = f[2].startswith(prefix)
            resting_is_ours = f[3].startswith(prefix)
            if incoming_is_ours and resting_is_ours:
                return  # seat traded with itself: no net position change
            if incoming_is_ours:
                side = f[7]          # the trailing field is the aggressor's side
            elif resting_is_ours:
                side = flip(f[7])    # we were resting, so we took the other side
            else:
                return
            vol, px = int(f[4]), int(f[5])
            signed = vol if side == "B" else -vol
            self.positions[seat] += signed
            self.last_mark = float(px)   # any seat's trade is a valid mark
            if seat == "hedger":
                self.cash -= signed * px
                # Distance from the mid at the moment we traded, signed so that
                # paying up to get done is a positive cost.
                if self.best_bid is not None and self.best_ask is not None:
                    mid = (self.best_bid + self.best_ask) / 2
                    self.cost_sum += (px - mid if side == "B" else mid - px) * vol
                    self.cost_lots += vol
                if resting_is_ours:
                    self.passive_lots += vol
                else:
                    self.crossed_lots += vol
                # Retire this fill from the in-flight bridge -- but only if it
                # was ever *on* the bridge. cross() adds to inflight; rest() does
                # not, because a resting order has not traded. Retiring a passive
                # fill here cancels the hedger's own position out of desk() and
                # leaves real exposure invisible: measured desk=-4 while the seats
                # actually held -44 between them, the desk reporting flat while
                # carrying forty lots.
                #
                # The signed amount matters too: an earlier version retired by
                # magnitude toward zero, which corrupted the count as soon as buy
                # and sell hedges were outstanding together.
                if incoming_is_ours and self.inflight:
                    left = self.inflight - signed
                    # A fill larger than what was outstanding, or a limit order
                    # that crossed on entry and so was never counted, must not
                    # flip the bridge's sign and start misreporting the other way.
                    if (self.inflight > 0) != (left > 0):
                        left = 0
                    self.inflight = left
                    self.inflight_at = time.monotonic()

        return handler

    # ---- hedging ----------------------------------------------------------- #

    def held(self):
        """Exposure actually held at the exchange, with no in-flight bridging.

        desk() is what the hedger *acts* on -- bridging stops it double-hedging an
        order already sent. held() is what the desk *is*, and is the honest number
        for reporting risk: the bridge is an estimate, and an estimate should not
        flatter the risk figure."""
        return sum(self.positions.values())

    def desk(self):
        """Desk exposure, including anything fired but not yet echoed back.

        The bridge is *self-healing*: it covers a round trip of milliseconds, so
        anything still outstanding after INFLIGHT_TTL is stale and gets dropped.
        Without that, any path which adds to inflight without producing a
        confirming fill -- the hedger crossing into its own resting order is one,
        since a self-match is no position change and is skipped -- leaves the
        bridge permanently overstated and the desk blind to real exposure. It
        reported flat while the seats held 40 lots between them. Positions come
        from the exchange and are always right; the bridge is the only guess here,
        so it is the part with an expiry on it."""
        if self.inflight and time.monotonic() - self.inflight_at > INFLIGHT_TTL:
            print(f"[hedger] dropping stale in-flight {self.inflight}", flush=True)
            self.inflight = 0
        return sum(self.positions.values()) + self.inflight

    async def request(self, msg):
        gap = self.min_gap - (time.monotonic() - self.last_send)
        if gap > 0:
            # Exceeding the exchange's max_tps disconnects the sender outright
            # (changelog v2.3) rather than rejecting, so we throttle ourselves.
            await asyncio.sleep(gap)
        self.last_send = time.monotonic()
        try:
            reply = await self.nc.request(f"ex.req.{SENDER}", msg.encode(), timeout=1.0)
        except Exception as e:
            print(f"[hedger] request failed: {e}", flush=True)
            return None
        return reply.data.decode().split()

    async def cancel(self, oid):
        parts = await self.request(f"{SENDER} C {FEED} {oid}")
        # 206 (never used) and 305 (used but no longer resting) both mean the order
        # is not on the book, which is the state we wanted.
        if parts and len(parts) >= 3 and parts[1] == "N" and parts[2] not in ("206", "305"):
            print(f"[hedger] cancel rejected {parts[2]}", flush=True)

    async def drop_passive(self):
        if self.passive:
            await self.cancel(self.passive["id"])
            self.passive = None

    async def cross(self, exposure):
        """Take the spread to get flat now. Used when exposure is urgent, or when
        patience has already been tried and did not work."""
        side = "S" if exposure > 0 else "B"
        size = min(abs(exposure), self.clip)
        # Price is a slippage limit, not the expected fill price: the exchange
        # matches against resting orders at *their* prices, so this sweeps the book
        # from the touch and refuses to pay more than SLIP ticks through it.
        if side == "S":
            if self.best_bid is None:
                return
            price = to_grid(self.best_bid - SLIP, self.tick, up=False)
        else:
            if self.best_ask is None:
                return
            price = to_grid(self.best_ask + SLIP, self.tick, up=True)

        self.oid += 1
        parts = await self.request(
            f"{SENDER} A {FEED} {base36(self.oid)} {side} {size} {price} F")
        if not parts or len(parts) < 2:
            return
        if parts[1] != "Y":
            code = parts[2] if len(parts) > 2 else "?"
            print(f"[hedger] rejected {code}: {' '.join(parts[3:])}", flush=True)
            return
        # F fills partially. The reply's count is what actually traded; whatever is
        # left over stays on the books and gets re-hedged on the next tick.
        filled = int(parts[2]) if len(parts) > 2 else 0
        if filled:
            self.inflight += filled if side == "B" else -filled
            self.inflight_at = time.monotonic()
            self.hedges += 1
            self.traded += filled
            print(f"[hedger] cross {side} {filled}/{size} @<={price} "
                  f"desk {exposure} -> {self.desk()}", flush=True)

    async def rest(self, exposure):
        """Try to get flat without paying the spread, by resting at the touch."""
        side = "S" if exposure > 0 else "B"
        size = min(abs(exposure), self.clip)
        if self.best_bid is None or self.best_ask is None:
            return
        improve = PASSIVE_IMPROVE * self.tick   # one grid step, whatever the tick
        if side == "B":
            price = self.best_bid + improve
            if price >= self.best_ask:      # improving would cross: just join
                price = self.best_bid
        else:
            price = self.best_ask - improve
            if price <= self.best_bid:
                price = self.best_ask

        self.oid += 1
        oid = base36(self.oid)
        parts = await self.request(f"{SENDER} A {FEED} {oid} {side} {size} {price} L")
        if not parts or len(parts) < 2 or parts[1] != "Y":
            if parts and len(parts) >= 3:
                print(f"[hedger] passive rejected {parts[2]}", flush=True)
            return
        # A limit at the touch normally rests; anything that traded on entry is
        # reported on our own feed and booked there.
        self.passive = {"id": oid, "side": side, "at": time.monotonic()}

    async def hedge(self):
        exposure = self.desk()

        if self.passive:
            want = "S" if exposure > 0 else "B"
            waited = (time.monotonic() - self.passive["at"]) * 1000
            if abs(exposure) < THRESH:
                await self.drop_passive()          # someone else flattened us
            elif self.passive["side"] != want:
                await self.drop_passive()          # exposure flipped sign
            elif abs(exposure) >= URGENT or waited >= PASSIVE_MS:
                await self.drop_passive()          # out of patience, or now urgent
                await self.cross(self.desk())
            else:
                pass                               # still waiting; give it time
            return

        if abs(exposure) < THRESH:
            return
        if abs(exposure) >= URGENT:
            await self.cross(exposure)             # too big to be patient about
        else:
            await self.rest(exposure)

    async def hedge_loop(self):
        while True:
            await asyncio.sleep(INTERVAL)
            try:
                await self.hedge()
            except Exception as e:  # never let one bad tick stop the risk control
                print(f"[hedger] hedge error: {e}", flush=True)

    # ---- reporting --------------------------------------------------------- #

    def mark(self):
        """Price used to value our own inventory. Falls back to the side we would
        have to trade against, then to the last price seen -- never valuing a live
        position at zero, which would report cash as profit."""
        if self.best_bid is not None and self.best_ask is not None:
            return (self.best_bid + self.best_ask) / 2
        own = self.positions["hedger"]
        if own > 0 and self.best_bid is not None:
            return float(self.best_bid)
        if own < 0 and self.best_ask is not None:
            return float(self.best_ask)
        return self.last_mark

    def liq(self):
        """What our position would actually fetch if closed now -- long sells into
        the bid, short buys from the ask. `pnl` marks at the mid; the session ends
        by liquidating against the book, so this is the honest close-out number."""
        own = self.positions["hedger"]
        if own == 0:
            return self.cash
        if own > 0 and self.best_bid is not None:
            return self.cash + own * self.best_bid
        if own < 0 and self.best_ask is not None:
            return self.cash + own * self.best_ask
        m = self.mark()
        return None if m is None else self.cash + own * m

    def pnl(self):
        """Our own mark-to-market. This is the *cost of the risk control*: the
        hedger crosses the spread every time, so it is expected to be negative.
        It is worth measuring precisely because it is the price paid for the flat
        desk position, and the two must be traded off against each other."""
        m = self.mark()
        if m is None:
            return None
        return self.cash + self.positions["hedger"] * m

    async def report_loop(self):
        while True:
            await asyncio.sleep(1.0)
            pos = " ".join(f"{k}={v}" for k, v in self.positions.items())
            p, l = self.pnl(), self.liq()
            cost = (self.cost_sum / self.cost_lots) if self.cost_lots else 0.0
            s = (f"desk={self.desk()} held={self.held()} {pos} inflight={self.inflight} "
                 f"hedges={self.hedges} traded={self.traded} cash={self.cash} "
                 f"pnl={'n/a' if p is None else format(p, '.0f')} "
                 f"liq={'n/a' if l is None else format(l, '.0f')} "
                 f"passive={self.passive_lots} crossed={self.crossed_lots} "
                 f"cost/lot={cost:.2f}")
            print(f"[hedger] {s}", flush=True)
            await self.nc.publish(f"strat.{SENDER}.status", s.encode())


async def main():
    nc = await nats.connect(NATS_URL, max_reconnect_attempts=-1)
    h = Hedger(nc)
    # The exchange's own declared limits override guessed ones. Rate matters
    # most here: exceeding max_tps disconnects the sender, and a disconnected
    # hedger means the desk has no risk control at all -- so the gap is held to
    # half the declared rate. position_limit caps how much one order may carry.
    mt = await fetch_meta(nc, FEED)
    h.tick = mt.get("ticksize", 1) or 1
    if mt.get("position_limit", 0) > 0:
        h.clip = min(h.clip, mt["position_limit"])
    if mt.get("max_tps", 0) > 0:
        h.min_gap = max(h.min_gap, 2.0 / mt["max_tps"])
    print(f"[hedger] meta {FEED}: tick={h.tick} max_tps={mt.get('max_tps', 'n/a')} "
          f"poslim={mt.get('position_limit', 'n/a')} -> clip={h.clip} "
          f"min_gap={h.min_gap:.3f}s", flush=True)
    await nc.subscribe(f"ex.bbo.{FEED}", cb=h.on_bbo)
    # Wildcard on the feed token: the seats do not all trade the same contract
    # (the quoter sits on a quieter sibling of the taker's front month), and a
    # hedger watching only one book would leave the others' exposure invisible.
    # Contracts on one underlying are cash-settled at their listed price, so a
    # lot of any of them carries the same unit risk and the signed sum across
    # feeds is the desk's exposure; it is hedged in FEED, the liquid one. The
    # residual is the spread between siblings, which is pinned in the sample
    # market and small beside the outright risk being removed.
    for seat, sender in SEATS.items():
        await nc.subscribe(f"ex.md.*.{sender}", cb=h.make_md_handler(seat, sender))
    print(f"[hedger] {SENDER} watching {sorted(SEATS.values())} on {FEED} "
          f"thresh={THRESH} clip={CLIP} slip={SLIP}", flush=True)
    await asyncio.gather(h.hedge_loop(), h.report_loop())


if __name__ == "__main__":
    asyncio.run(main())
