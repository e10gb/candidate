#!/usr/bin/env python3
"""Measure adverse selection directly: where does the mid go after we trade?

    tools/markout.py [SECONDS] [SENDER] [FEED]

For every fill, compare the price we traded at against the mid a fixed time
later, signed so that positive means the market moved our way. A market maker
earning from uninformed flow has markouts around zero or positive; one being
picked off has them negative, and the horizon at which they go negative says how
fast the information arrives.

This exists because PnL is far too noisy to tune against -- runs of identical
configuration differ by thousands -- while markout is per-fill, so a single run
yields hundreds of observations instead of one.
"""
import asyncio
import bisect
import os
import sys

import nats

SECS = float(sys.argv[1]) if len(sys.argv) > 1 else 90
SENDER = sys.argv[2] if len(sys.argv) > 2 else "QUOTE001"
FEED = sys.argv[3] if len(sys.argv) > 3 else os.environ.get("QUOTER_FEED", "AAM6")
NATS_URL = os.environ.get("NATS_URL", "nats://127.0.0.1:4222")
HORIZONS = [0.1, 0.5, 2.0, 5.0]


async def main():
    nc = await nats.connect(NATS_URL)
    mids, fills = [], []          # (ts, mid) sorted by ts; (ts, side, vol, px, who)

    async def on_bbo(msg):
        f = msg.data.decode().split()
        if len(f) < 6 or f[2] == "-" or f[4] == "-":
            return
        ts = int(f[0]) / 1e9
        mids.append((ts, (int(f[2]) + int(f[4])) / 2))

    async def on_md(msg):
        f = msg.data.decode().split()
        if len(f) < 8 or f[1] not in ("E", "T"):
            return
        pre = SENDER + ":"
        if f[2].startswith(pre):
            side, them = f[7], f[3]          # we crossed
        elif f[3].startswith(pre):
            side, them = ("S" if f[7] == "B" else "B"), f[2]   # we rested
        else:
            return
        fills.append((int(f[0]) / 1e9, side, int(f[4]), int(f[5]), them.split(":")[0]))

    await nc.subscribe(f"ex.bbo.{FEED}", cb=on_bbo)
    await nc.subscribe(f"ex.md.{FEED}.{SENDER}", cb=on_md)
    print(f"measuring {SENDER} on {FEED} for {SECS:.0f}s...", file=sys.stderr)
    await asyncio.sleep(SECS)
    # Let the tail of the last fills' horizons fill in before cutting the feed.
    await asyncio.sleep(max(HORIZONS) + 1)
    await nc.drain()

    if not fills or not mids:
        print("no fills or no book -- is the desk running?")
        return
    times = [t for t, _ in mids]

    def mid_at(t):
        i = bisect.bisect_right(times, t) - 1
        return mids[i][1] if i >= 0 else None

    print(f"\n{len(fills)} fills, {sum(f[2] for f in fills)} lots on {FEED}\n")

    # Decomposition. For a buy at px with mid M0 at the fill and M1 later:
    #
    #     markout (M1 - px)  =  edge captured (M0 - px)  +  drift (M1 - M0)
    #
    # The two halves mean opposite things. A healthy edge eaten by drift means we
    # priced correctly and were run over afterwards. An edge near zero means we
    # were filled at a price that was already stale -- the fill itself is the
    # loss, and no amount of holding or hedging changes that.
    edge_tot = edge_lots = 0
    for ts, side, vol, px, _ in fills:
        m0 = mid_at(ts)
        if m0 is None:
            continue
        e = (m0 - px) if side == "B" else (px - m0)
        edge_tot += e * vol
        edge_lots += vol
    if edge_lots:
        print(f"edge captured at the fill: {edge_tot / edge_lots:>8.2f} per lot "
              f"({edge_tot:.0f} total)")
        print("  (how far inside the mid we traded -- what the quoted spread "
              "actually earned)\n")

    print(f"{'horizon':>8}  {'edge@fill':>10}  {'drift':>10}  {'= markout':>10}  {'n':>5}")
    print("-" * 54)
    for h in HORIZONS:
        tot = lots = n = 0
        dtot = 0
        for ts, side, vol, px, _ in fills:
            m, m0 = mid_at(ts + h), mid_at(ts)
            if m is None or m0 is None:
                continue
            # Positive = the market moved our way after we traded.
            edge = (m - px) if side == "B" else (px - m)
            drift = (m - m0) if side == "B" else (m0 - m)
            tot += edge * vol
            dtot += drift * vol
            lots += vol
            n += 1
        if lots:
            print(f"{h:>7.1f}s  {edge_tot / edge_lots:>10.2f}  {dtot / lots:>10.2f}  "
                  f"{tot / lots:>10.2f}  {n:>5}")

    print(f"\n{'counterparty':<12} {'lots':>6} {'markout/lot @2s':>16}")
    print("-" * 38)
    by = {}
    for ts, side, vol, px, who in fills:
        m = mid_at(ts + 2.0)
        if m is None:
            continue
        edge = (m - px) if side == "B" else (px - m)
        a = by.setdefault(who, [0, 0.0])
        a[0] += vol
        a[1] += edge * vol
    for who, (lots, tot) in sorted(by.items(), key=lambda kv: -kv[1][0]):
        print(f"{who:<12} {lots:>6} {tot / lots:>16.2f}")


if __name__ == "__main__":
    asyncio.run(main())
