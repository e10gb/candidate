#!/usr/bin/env python3
"""Is toxic flow predictable *before* it hits us?

    tools/leadsignal.py [SECONDS] [SENDER] [FEED]

We cannot choose who trades with us on a central book, but we may be able to see
them coming. The sample market's background quoters cancel their whole ladder
before reposting at a new centre (sim/market.py:154), so a burst of cancels from a
liquidity provider is a leading indicator that it is about to reprice -- and
possibly cross us. Liquidity withdrawal ahead of a move is a real signal, not a
quirk of this simulator.

Every participant's adds and cancels arrive on ex.md.<FEED>.* tagged with their
sender, so the raw material is already on the wire.

This measures whether the signal has predictive power *before* any strategy is
written on it: for each of our fills, how much cancel activity preceded it, split
by whether the fill turned out toxic.
"""
import asyncio
import bisect
import os
import sys

import nats

SECS = float(sys.argv[1]) if len(sys.argv) > 1 else 120
ME = sys.argv[2] if len(sys.argv) > 2 else "QUOTE001"
FEED = sys.argv[3] if len(sys.argv) > 3 else os.environ.get("QUOTER_FEED", "AAM6")
NATS_URL = os.environ.get("NATS_URL", "nats://127.0.0.1:4222")
LOOKBACK = 0.20      # window before a fill in which we count cancels
HORIZON = 2.0        # markout horizon that defines "toxic"


async def main():
    nc = await nats.connect(NATS_URL)
    mids, cancels, fills = [], [], []

    async def on_bbo(msg):
        f = msg.data.decode().split()
        if len(f) < 6 or f[2] == "-" or f[4] == "-":
            return
        mids.append((int(f[0]) / 1e9, (int(f[2]) + int(f[4])) / 2))

    async def on_md(msg):
        f = msg.data.decode().split()
        if len(f) < 3:
            return
        ts, typ = int(f[0]) / 1e9, f[1]
        if typ == "C":
            cancels.append((ts, f[2].split(":")[0]))
            return
        if typ not in ("E", "T") or len(f) < 8:
            return
        pre = ME + ":"
        if f[2].startswith(pre):
            side, them = f[7], f[3]
        elif f[3].startswith(pre):
            side, them = ("S" if f[7] == "B" else "B"), f[2]
        else:
            return
        fills.append((ts, side, int(f[4]), int(f[5]), them.split(":")[0]))

    await nc.subscribe(f"ex.bbo.{FEED}", cb=on_bbo)
    await nc.subscribe(f"ex.md.{FEED}.*", cb=on_md)     # everyone, not just us
    print(f"watching all of {FEED} for {SECS:.0f}s...", file=sys.stderr)
    await asyncio.sleep(SECS)
    await asyncio.sleep(HORIZON + 1)
    await nc.drain()

    if not fills:
        print("no fills -- is the desk running?")
        return
    mt = [t for t, _ in mids]
    ct = sorted(t for t, _ in cancels)

    def mid_at(t):
        i = bisect.bisect_right(mt, t) - 1
        return mids[i][1] if i >= 0 else None

    toxic, benign = [], []
    for ts, side, vol, px, who in fills:
        m = mid_at(ts + HORIZON)
        if m is None:
            continue
        edge = (m - px) if side == "B" else (px - m)
        # cancels across the whole book in the window just before this fill
        n = bisect.bisect_right(ct, ts) - bisect.bisect_left(ct, ts - LOOKBACK)
        (toxic if edge < 0 else benign).append((n, edge, vol))

    def summarise(label, rows):
        if not rows:
            print(f"{label:<26} (none)")
            return
        n = sum(r[0] for r in rows) / len(rows)
        e = sum(r[1] * r[2] for r in rows) / sum(r[2] for r in rows)
        print(f"{label:<26} {len(rows):>6} {n:>18.1f} {e:>16.2f}")

    print(f"\n{'fill outcome':<26} {'count':>6} {'cancels in prior 200ms':>18} "
          f"{'markout/lot':>16}")
    print("-" * 70)
    summarise("toxic (markout < 0)", toxic)
    summarise("benign (markout >= 0)", benign)

    # Does a cancel burst actually separate them? Split fills by the signal and
    # compare outcomes -- that is the form the strategy would use it in.
    allrows = toxic + benign
    if allrows:
        thresh = sorted(r[0] for r in allrows)[int(len(allrows) * 0.75)]
        hi = [r for r in allrows if r[0] >= thresh]
        lo = [r for r in allrows if r[0] < thresh]
        print(f"\nsplitting on the signal (>= {thresh} cancels in prior 200ms):")
        print("-" * 70)
        summarise(f"quiet book (< {thresh})", lo)
        summarise(f"cancel burst (>= {thresh})", hi)


if __name__ == "__main__":
    asyncio.run(main())
