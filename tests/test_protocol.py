#!/usr/bin/env python3
"""Assert the exchange behaviours the desk's code depends on.

Every one of these was discovered by experiment and contradicts, or is absent
from, the shipped documentation (see NOTES.md). They are encoded as tests because
the desk's correctness rests on them and the grading exchange may not be the same
build: if one of these starts failing, a seat is silently wrong rather than loudly
broken.

Runs against a live exchange. Needs no sample market -- it makes its own trades on
an empty book, so every effect is caused by the test.

    docker compose up -d
    tests/run_tests.sh          # or, directly, inside a container with nats-py:
    NATS_URL=nats://localhost:4222 python3 tests/test_protocol.py
"""
import asyncio
import os
import random
import sys

import nats

NATS_URL = os.environ.get("NATS_URL", "nats://127.0.0.1:4222")
FEED = os.environ.get("TEST_FEED", "AAH6")
# Order ids are consumed permanently per sender, so every run needs fresh senders
# or it fails with 203 on the second execution.
RUN = f"{random.randint(0, 99999):05d}"
A = f"TSTA{RUN[:4]}"
B = f"TSTB{RUN[:4]}"

failures = []
passes = 0


def check(name, got, want):
    global passes
    if got == want:
        passes += 1
        print(f"  PASS  {name}")
    else:
        failures.append(f"{name}: got {got!r}, want {want!r}")
        print(f"  FAIL  {name}: got {got!r}, want {want!r}")


class Ex:
    """Minimal order-entry client for the tests."""

    def __init__(self, nc, sender):
        self.nc, self.sender = nc, sender
        self.n = 0

    def oid(self):
        self.n += 1
        return f"{self.sender[:2]}{self.n:06d}"[:8]

    async def req(self, body):
        msg = await self.nc.request(f"ex.req.{self.sender}", body.encode(), timeout=2.0)
        return msg.data.decode().split()

    async def add(self, side, vol, px, typ="L", oid=None):
        oid = oid or self.oid()
        parts = await self.req(f"{self.sender} A {FEED} {oid} {side} {vol} {px} {typ}")
        return oid, parts

    async def cancel(self, oid):
        return await self.req(f"{self.sender} C {FEED} {oid}")


def code(parts):
    """'Y n' -> ('Y', n); 'N code text' -> ('N', code)."""
    if len(parts) >= 2 and parts[1] == "Y":
        return ("Y", int(parts[2]) if len(parts) > 2 else 0)
    if len(parts) >= 3:
        return ("N", int(parts[2]))
    return ("?", -1)


async def main():
    nc = await nats.connect(NATS_URL)
    a, b = Ex(nc, A), Ex(nc, B)

    # Where a fresh, quiet price sits: far from the sample market if it is running,
    # but inside the price band (ref 600 +/- 5000).
    px = 300

    print("\nF (fill-and-kill) semantics")
    # CHANGELOG v2.3 claims F is atomic ("fills in full or rejects"). It is not.
    oid, r = await a.add("B", 10, px)
    check("resting bid accepted", code(r), ("Y", 0))
    _, r = await b.add("S", 25, px, typ="F")
    check("F sell of 25 into a bid of 10 fills 10, not 0 or 25", code(r), ("Y", 10))
    _, r = await b.add("S", 5, px, typ="F")
    check("F does not rest: nothing left to trade against", code(r), ("Y", 0))

    print("\nOrder ids are consumed permanently")
    oid, r = await a.add("B", 1, px - 1)
    check("add accepted", code(r), ("Y", 0))
    check("cancel removes it", code(await a.cancel(oid)), ("Y", 1))
    _, r = await a.add("B", 1, px - 1, oid=oid)
    check("re-using a cancelled id is rejected 203", code(r), ("N", 203))

    print("\n206 vs 305 -- both mean 'not resting', for different reasons")
    check("cancelling an id never used -> 206",
          code(await a.cancel("zzzzzzzz")), ("N", 206))
    check("cancelling an already-cancelled id -> 305",
          code(await a.cancel(oid)), ("N", 305))
    filled, r = await a.add("B", 4, px - 2)
    await b.add("S", 4, px - 2, typ="F")
    check("cancelling a filled id -> 305", code(await a.cancel(filled)), ("N", 305))

    print("\nE goes to the resting side, T to the aggressor")
    seen = {"E": [], "T": []}

    async def collect(who):
        async def cb(msg):
            f = msg.data.decode().split()
            if len(f) >= 2 and f[1] in ("E", "T"):
                seen[f[1]].append((who, f[2], f[3]))
        return cb

    await nc.subscribe(f"ex.md.{FEED}.{A}", cb=await collect("A"))
    await nc.subscribe(f"ex.md.{FEED}.{B}", cb=await collect("B"))
    await asyncio.sleep(0.3)
    await a.add("B", 6, px - 3)          # A rests
    await b.add("S", 6, px - 3, typ="F")  # B crosses
    await asyncio.sleep(0.8)

    check("resting side receives E", [w for w, _, _ in seen["E"]], ["A"])
    check("aggressor receives T", [w for w, _, _ in seen["T"]], ["B"])

    print("\nRejects we rely on classifying")
    _, r = await a.add("B", 1, 999999)
    check("price outside the band -> 302", code(r), ("N", 302))
    _, r = await a.add("B", 0, px)
    check("zero volume -> 100 malformed", code(r), ("N", 100))
    _, r = await a.add("X", 1, px)
    check("bad side -> 205", code(r), ("N", 205))
    r = await a.req(f"{B} A {FEED} aaaaaaaa B 1 {px} L")  # wrong sender for subject
    check("sender not matching the subject -> 200", code(r), ("N", 200))

    # Leave the book as we found it.
    for ex in (a, b):
        for i in range(1, ex.n + 1):
            await ex.req(f"{ex.sender} C {FEED} {ex.sender[:2]}{i:06d}"[:40])
    await nc.drain()

    print(f"\n{passes} passed, {len(failures)} failed")
    for f in failures:
        print(f"  - {f}")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
