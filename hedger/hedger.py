#!/usr/bin/env python3
"""Desk hedger.

Keeps the desk's *combined* position -- quoter + taker + hedger -- near zero.
When the other seats accumulate exposure, this reduces it by crossing the spread.
It is the seat that is allowed to pay for speed: a small position held for a while
is cheap, a large one held for a second is not.

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
  HEDGE_MAX_TPS    self-imposed request rate cap         (default 20)
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


class Hedger:
    def __init__(self, nc):
        self.nc = nc
        self.positions = {name: 0 for name in SEATS}
        # Volume we have traded but not yet seen echoed back on our own market-data
        # feed. Without this the desk looks unhedged for as long as the round trip
        # takes and we fire the same hedge repeatedly.
        self.inflight = 0
        self.best_bid = None
        self.best_ask = None
        self.oid = int(time.time() * 1000)
        self.hedges = 0
        self.traded = 0
        self.cash = 0
        self.last_mark = None   # last price we can value inventory against
        self.last_send = 0.0
        self.min_gap = 1.0 / MAX_TPS if MAX_TPS > 0 else 0.0

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
                # This fill is confirmed, so retire it from the in-flight bridge.
                # Must be the *signed* amount: an earlier version decremented by
                # magnitude toward zero, which corrupted the count as soon as a buy
                # hedge and a sell hedge were outstanding in the same direction.
                self.inflight -= signed

        return handler

    # ---- hedging ----------------------------------------------------------- #

    def desk(self):
        """Desk exposure including anything we have fired but not yet seen back."""
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

    async def hedge(self):
        exposure = self.desk()
        if abs(exposure) < THRESH:
            return
        # Long the desk -> sell. Short -> buy.
        side = "S" if exposure > 0 else "B"
        size = min(abs(exposure), CLIP)

        # Price is a slippage limit, not the expected fill price: the exchange
        # matches against resting orders at *their* prices, so this sweeps the book
        # from the touch and refuses to pay more than SLIP ticks through it.
        if side == "S":
            if self.best_bid is None:
                return
            price = self.best_bid - SLIP
        else:
            if self.best_ask is None:
                return
            price = self.best_ask + SLIP

        self.oid += 1
        order = f"{SENDER} A {FEED} {base36(self.oid)} {side} {size} {price} F"
        parts = await self.request(order)
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
            self.hedges += 1
            self.traded += filled
            print(f"[hedger] {side} {filled}/{size} @<={price} "
                  f"desk {exposure} -> {self.desk()}", flush=True)

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
            p = self.pnl()
            s = (f"desk={self.desk()} {pos} inflight={self.inflight} "
                 f"hedges={self.hedges} traded={self.traded} cash={self.cash} "
                 f"pnl={'n/a' if p is None else format(p, '.0f')}")
            print(f"[hedger] {s}", flush=True)
            await self.nc.publish(f"strat.{SENDER}.status", s.encode())


async def main():
    nc = await nats.connect(NATS_URL, max_reconnect_attempts=-1)
    h = Hedger(nc)
    await nc.subscribe(f"ex.bbo.{FEED}", cb=h.on_bbo)
    for seat, sender in SEATS.items():
        await nc.subscribe(f"ex.md.{FEED}.{sender}", cb=h.make_md_handler(seat, sender))
    print(f"[hedger] {SENDER} watching {sorted(SEATS.values())} on {FEED} "
          f"thresh={THRESH} clip={CLIP} slip={SLIP}", flush=True)
    await asyncio.gather(h.hedge_loop(), h.report_loop())


if __name__ == "__main__":
    asyncio.run(main())
