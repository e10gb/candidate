#!/usr/bin/env python3
"""Momentum taker strategy.

Watches the best-bid/offer for one contract and, when the mid-price moves far
enough in one direction, crosses the spread with a fill-and-kill order to follow
the move. Tracks its own position and mark-to-market PnL and reports them once a
second on `strat.<sender>.status`.

Config (env):
  NATS_URL        default nats://127.0.0.1:4222
  TAKER_FEED      contract to trade            (default AAH6)
  TAKER_SENDER    8-char sender tag            (default PYTKR001)
  TAKER_CLIP      order size per trade         (default 3)
  TAKER_MAX_POS   max absolute position        (default 30)
  TAKER_THRESH    mid move (price units) that triggers a trade (default 10)
  TAKER_LAG       distinct top-of-book changes the move spans (default 5)
  TAKER_RUN       seconds to run               (default 20)
"""
import asyncio
import collections
import os
import random
import time

import nats

NATS_URL = os.environ.get("NATS_URL", "nats://127.0.0.1:4222")
# BTH6 was the default and is not a listed instrument (see
# exchange/instruments.txt), so an unconfigured local run got 202 bad feedcode on
# every order -- another way this went quiet rather than loud.
FEED = os.environ.get("TAKER_FEED", "AAH6")
SENDER = os.environ.get("TAKER_SENDER", "PYTKR001")
CLIP = int(os.environ.get("TAKER_CLIP", "3"))
MAX_POS = int(os.environ.get("TAKER_MAX_POS", "30"))
THRESH = int(os.environ.get("TAKER_THRESH", "10"))
LAG = int(os.environ.get("TAKER_LAG", "5"))
RUN_S = float(os.environ.get("TAKER_RUN", "20"))
# Minimum gap between trades. Without it, maybe_trade() fires on every incoming
# message for as long as the signal holds, turning one move into a burst of
# orders. It doubles as this seat's rate limit: exceeding the exchange's max_tps
# disconnects the sender outright (changelog v2.3) rather than rejecting, and this
# was the only seat with no self-imposed cap at all.
COOLDOWN = float(os.environ.get("TAKER_COOLDOWN", "0.5"))


class Taker:
    def __init__(self, nc):
        self.nc = nc
        self.oid = random.randint(0, 80_000_000)  # avoid id clashes across runs
        self.best_bid = None
        self.best_ask = None
        self.mids = collections.deque(maxlen=LAG + 1)
        self.position = 0
        self.cash = 0.0       # signed: buys spend cash, sells receive cash
        self.fills = 0
        self.last_mark = None  # last price we can value inventory against
        self.last_top = None   # dedupe: the BBO feed republishes unchanged tops
        self.last_trade_at = 0.0
        self.send_lock = asyncio.Lock()

    async def on_md(self, msg):
        """Book fills from the exchange's own record of what matched.

        <ts> <E|T> <incoming:17> <resting:17> <volume> <price> <matchid> <B|S>

        A seat receives E on its own subject when it was the *resting* side of a
        match and T when it was the aggressor -- they are not duplicates of one
        another. This strategy only ever crosses with F, so T is the normal case,
        but both are handled so a change of order type cannot silently lose fills.
        One order can also produce several messages; they accumulate.
        """
        f = msg.data.decode().split()
        if len(f) < 8 or f[1] not in ("E", "T"):
            return
        prefix = SENDER + ":"
        if f[2].startswith(prefix):
            side = f[7]                              # we crossed: aggressor's side
        elif f[3].startswith(prefix):
            side = "S" if f[7] == "B" else "B"       # we rested: the other side
        else:
            return
        self.apply_fill(side, int(f[4]), int(f[5]))
        self.fills += 1

    def next_oid(self):
        self.oid += 1
        return f"{self.oid:08d}"

    def mid(self):
        if self.best_bid is None or self.best_ask is None:
            return None
        return (self.best_bid + self.best_ask) / 2

    async def on_bbo(self, msg):
        # payload: "<ts> <FEED> <bid_px> <bid_vol> <ask_px> <ask_vol>", '-' if empty
        f = msg.data.decode().split()
        if len(f) < 6:
            return
        # The BBO feed republishes on every book event, not only when the top
        # changes: measured 154 messages/sec against 24.8 genuine changes/sec.
        # Recording all of them made TAKER_LAG count *messages* rather than price
        # moves, so the "move over the last 5 updates" was really the move over
        # ~32ms of mostly-identical quotes -- a momentum signal measuring noise.
        top = (f[2], f[3], f[4], f[5])
        if top == self.last_top:
            return
        self.last_top = top
        self.best_bid = None if f[2] == "-" else int(f[2])
        self.best_ask = None if f[4] == "-" else int(f[4])
        m = self.mid()
        if m is None:
            return
        self.mids.append(m)
        await self.maybe_trade()

    async def maybe_trade(self):
        if len(self.mids) < self.mids.maxlen:
            return
        if time.monotonic() - self.last_trade_at < COOLDOWN:
            return
        past, now = self.mids[0], self.mids[-1]
        if now - past >= THRESH and self.position < MAX_POS and self.best_ask is not None:
            await self.take("B")
        elif past - now >= THRESH and self.position > -MAX_POS and self.best_bid is not None:
            await self.take("S")

    async def take(self, side):
        async with self.send_lock:
            self.last_trade_at = time.monotonic()  # set before the await, not after
            px = self.best_ask if side == "B" else self.best_bid
            oid = self.next_oid()
            order = f"{SENDER} A {FEED} {oid} {side} {CLIP} {px} F"
            try:
                # The exchange listens on `ex.req.>`, which does not match a
                # subject with no trailing token. This used to publish to bare
                # `ex.req`, so every order got "no responders" and the strategy
                # never traded at all -- silently, because the failure was
                # swallowed by a bare except.
                reply = await self.nc.request(f"ex.req.{SENDER}", order.encode(),
                                              timeout=1.0)
            except Exception as e:
                print(f"[taker] order failed: {e}", flush=True)
                return
            parts = reply.data.decode().split()
            # ["EXCHANGE", "Y", "<n>"] on accept, ["EXCHANGE", "N", "<code>", ...]
            # on reject.
            if len(parts) >= 2 and parts[1] == "Y":
                # <n> is the volume that actually traded (F fills *partially*,
                # despite CHANGELOG v2.3). We deliberately do not book the fill
                # here: the reply gives volume but not price, and a marketable
                # order is filled at the resting orders' prices, not at our limit.
                # Booking `filled * px` overstated every buy and understated every
                # sell -- measured 60s: this reported cash=-22820 where the
                # exchange's own feed said -7731. Fills are booked in on_md below,
                # from what actually traded.
                pass
            elif len(parts) >= 3:
                print(f"[taker] rejected {parts[2]}: {' '.join(parts[3:])}", flush=True)

    def apply_fill(self, side, qty, px):
        # Both branches used to be `qty`, so a sell was booked as a buy: position
        # and cash were wrong in opposite directions on every sale.
        signed = qty if side == "B" else -qty
        self.position += signed
        self.cash -= signed * px
        self.last_mark = float(px)   # a real trade is a valid mark

    def mark(self):
        """Price used to value inventory.

        Falls back to the side we would have to trade against, then to the last
        price seen. The old pnl() valued the position at zero whenever the mid was
        unavailable, which silently reported cash as profit -- and the sample
        market's book does go empty (see NOTES.md).
        """
        m = self.mid()
        if m is not None:
            return m
        if self.position > 0 and self.best_bid is not None:
            return float(self.best_bid)      # long: we would sell into the bid
        if self.position < 0 and self.best_ask is not None:
            return float(self.best_ask)      # short: we would buy from the ask
        return self.last_mark

    def pnl(self):
        m = self.mark()
        if m is None:
            return None          # cannot value the position; say so, do not guess
        return self.cash + self.position * m

    async def publish_status(self):
        p = self.pnl()
        s = (f"pos={self.position} cash={self.cash:.0f} "
             f"pnl={'n/a' if p is None else format(p, '.0f')} fills={self.fills}")
        print(f"[taker] {s}", flush=True)
        await self.nc.publish(f"strat.{SENDER}.status", s.encode())

    async def reporter(self):
        while True:
            await asyncio.sleep(1.0)
            await self.publish_status()


async def main():
    nc = await nats.connect(NATS_URL)
    t = Taker(nc)
    await nc.subscribe(f"ex.bbo.{FEED}", cb=t.on_bbo)
    # Our own fills, as the exchange recorded them.
    await nc.subscribe(f"ex.md.{FEED}.{SENDER}", cb=t.on_md)
    print(f"[taker] {SENDER} trading {FEED} clip={CLIP} thresh={THRESH} "
          f"for {RUN_S}s", flush=True)
    rep = asyncio.create_task(t.reporter())
    await asyncio.sleep(RUN_S)
    rep.cancel()
    await t.publish_status()
    p = t.pnl()
    print(f"[taker] final: position={t.position} "
          f"pnl={'n/a' if p is None else format(p, '.0f')} "
          f"fills={t.fills}", flush=True)
    await nc.drain()


if __name__ == "__main__":
    asyncio.run(main())
