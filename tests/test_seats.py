#!/usr/bin/env python3
"""Unit tests for the Python seats: the hedger's desk accounting and the taker's
signal handling. No network -- these exercise the logic that produced real bugs.

Run inside an image that has nats-py (the seats import it at module scope):

    tests/run_tests.sh
"""
import asyncio
import pathlib
import sys
import time
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
sys.path[:0] = [str(ROOT / "hedger"), str(ROOT / "taker")]

import hedger  # noqa: E402
import taker   # noqa: E402


def run(coro):
    loop = asyncio.new_event_loop()
    try:
        return loop.run_until_complete(coro)
    finally:
        loop.close()


class Msg:
    def __init__(self, payload):
        self.data = payload.encode()


class HedgerPositions(unittest.TestCase):
    def setUp(self):
        self.h = hedger.Hedger(nc=None)
        self.quoter = hedger.SEATS["quoter"]
        self.me = hedger.SEATS["hedger"]

    def feed(self, seat, sender, payload):
        run(self.h.make_md_handler(seat, sender)(Msg(payload)))

    def test_resting_seat_gets_E_and_takes_the_opposite_side(self):
        # Buyer aggressed against the quoter's resting order -> the quoter sold.
        self.feed("quoter", self.quoter, f"1 E OTHER001:aaaaaaaa {self.quoter}:bbbbbbbb 5 650 1 B")
        self.assertEqual(self.h.positions["quoter"], -5)

    def test_aggressor_gets_T_and_takes_the_aggressor_side(self):
        self.feed("hedger", self.me, f"1 T {self.me}:bbbbbbbb OTHER001:aaaaaaaa 7 650 1 B")
        self.assertEqual(self.h.positions["hedger"], 7)

    def test_a_seat_trading_with_itself_is_not_a_position_change(self):
        self.feed("quoter", self.quoter,
                  f"1 E {self.quoter}:aaaaaaaa {self.quoter}:bbbbbbbb 5 650 1 B")
        self.assertEqual(self.h.positions["quoter"], 0)

    def test_desk_is_the_sum_of_seats_plus_inflight(self):
        self.h.positions["quoter"] = -10
        self.h.positions["taker"] = 4
        self.h.inflight = 3
        self.assertEqual(self.h.desk(), -3)

    def test_inflight_retires_by_signed_amount(self):
        """The bug: retiring by magnitude toward zero corrupted the count as soon
        as hedges in both directions were outstanding. Desk exposure hit 55."""
        self.h.inflight = -25                      # a sell hedge is outstanding
        self.feed("hedger", self.me, f"1 T {self.me}:bbbbbbbb O:aaaaaaaa 25 650 1 S")
        self.assertEqual(self.h.positions["hedger"], -25)
        self.assertEqual(self.h.inflight, 0, "confirmed fill should clear in-flight")

    def test_inflight_with_opposite_hedges_outstanding(self):
        self.h.inflight = 10                       # a buy hedge outstanding
        self.feed("hedger", self.me, f"1 T {self.me}:bbbbbbbb O:aaaaaaaa 10 650 1 B")
        self.assertEqual(self.h.inflight, 0)
        self.assertEqual(self.h.desk(), 10, "position confirmed, no longer in flight")

    def test_pnl_never_values_a_live_position_at_zero(self):
        self.h.positions["hedger"] = 10
        self.h.cash = -6000
        self.h.best_bid, self.h.best_ask = 640, 660
        self.assertEqual(self.h.pnl(), -6000 + 10 * 650)
        self.h.best_bid = self.h.best_ask = None    # book gone
        self.h.last_mark = 640.0
        self.assertEqual(self.h.pnl(), -6000 + 10 * 640)
        self.h.last_mark = None
        self.assertIsNone(self.h.pnl(), "unmarkable must be None, not cash")


class TakerSignal(unittest.TestCase):
    def setUp(self):
        self.t = taker.Taker(nc=None)

    def bbo(self, bid, ask):
        return Msg(f"1 {taker.FEED} {bid} 5 {ask} 5")

    def test_repeated_identical_tops_are_not_recorded(self):
        """The BBO feed republishes unchanged: 154 msgs/sec vs 24.8 real changes.
        Recording all of them made TAKER_LAG span ~32ms of duplicates."""
        self.t.last_trade_at = time.monotonic()   # suppress trading
        for _ in range(50):
            run(self.t.on_bbo(self.bbo(600, 604)))
        self.assertEqual(len(self.t.mids), 1, "duplicates must not fill the window")

    def test_distinct_tops_are_recorded(self):
        self.t.last_trade_at = time.monotonic()
        for bid in range(600, 610):
            run(self.t.on_bbo(self.bbo(bid, bid + 4)))
        self.assertEqual(len(self.t.mids), self.t.mids.maxlen)

    def test_cooldown_suppresses_a_burst(self):
        sent = []

        async def fake_take(side):
            sent.append(side)
            self.t.last_trade_at = time.monotonic()

        self.t.take = fake_take
        for bid in range(600, 700, 5):            # a strong sustained move
            run(self.t.on_bbo(self.bbo(bid, bid + 4)))
        self.assertLessEqual(len(sent), 1,
                             "one cooldown window must not produce a burst of orders")

    def test_sell_reduces_position(self):
        """Both branches of apply_fill used to be `qty`, booking sells as buys."""
        self.t.apply_fill("B", 10, 600)
        self.t.apply_fill("S", 4, 610)
        self.assertEqual(self.t.position, 6)
        self.assertEqual(self.t.cash, -10 * 600 + 4 * 610)

    def test_fills_come_from_the_exchange_feed(self):
        run(self.t.on_md(Msg(f"1 T {taker.SENDER}:aaaaaaaa O:bbbbbbbb 3 642 1 B")))
        self.assertEqual(self.t.position, 3)
        self.assertEqual(self.t.cash, -3 * 642, "booked at the traded price")

    def test_pnl_is_none_when_nothing_can_be_marked(self):
        self.t.position = 5
        self.assertIsNone(self.t.pnl())


if __name__ == "__main__":
    unittest.main(verbosity=2)
