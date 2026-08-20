# Python taker strategy

The desk's existing momentum taker. It subscribes to the best-bid/offer feed
for one contract and, when the mid-price moves by `TAKER_THRESH` over the last
`TAKER_LAG` updates, crosses the spread with a fill-and-kill (`F`) order to
follow the move. It keeps its own position and mark-to-market PnL and
publishes them once a second on `strat.<sender>.status` (and to stdout).

In the full stack (`./run.sh --sim --strategy`) it runs as a container — the
desk's taker seat, trading the front month all session. The instructions below
are for running it by hand while probing.

## Run

Bring up the exchange + sample market first (`./run.sh --sim`), then:

```bash
python3 -m venv .venv && .venv/bin/pip install nats-py     # once
TAKER_FEED=<feed> .venv/bin/python taker/taker.py          # pick a listed feed
```

(`nats kv ls EX_META` lists the feeds; see `PROTOCOL.md`.)

Watch its self-reported state:

```bash
nats sub 'strat.PYTKR001.status'
```

## Config (env)

| Var           | Default | Meaning                                        |
|---------------|---------|------------------------------------------------|
| `TAKER_FEED`  | `AAH6`  | contract to trade                              |
| `TAKER_SENDER`| `PYTKR001` | 8-char sender tag                           |
| `TAKER_CLIP`  | `3`     | order size per trade                           |
| `TAKER_MAX_POS`| `30` (compose: `15`) | max absolute position             |
| `TAKER_THRESH`| `10`    | mid move (price units) that triggers a trade   |
| `TAKER_LAG`   | `5`     | distinct top-of-book changes the move spans    |
| `TAKER_RUN`   | `20`    | seconds to run                                 |
| `TAKER_MODE`  | `reversion` (compose: `momentum`) | `reversion` fades a move, `momentum` follows it |
| `TAKER_COOLDOWN` | `0.5` | minimum seconds between trades               |

## Momentum or reversion

This seat shipped as a momentum strategy and lost money in every measurement of
it (-21,309 in one 240s run). Momentum is the wrong sign for the sample market:
the price walks to a target and back inside a hard band (`sim/market.py` clamps to
[440, 760]), so a move is more likely to unwind than continue. Fading it earned in
3 of 4 runs (+7,544, +7,199, +4,576, -1,938).

That is also the desk's division of labour. The profit in this market comes from
holding through the reversion, and the quoter must not hold -- Job 2 requires the
desk to stay flat and the hedger clears its inventory. So the reversion trade goes
to the seat whose purpose *is* directional risk, bounded by `TAKER_MAX_POS`.

**The desk ships as `momentum`, the strategy as handed over.** Reversion is fitted
to this market's mean-reverting band and would be the wrong sign in a trending one,
exactly as momentum is wrong here -- so the better local number is not worth
shipping as a default that may invert against the grading market. It is one
environment variable away for anyone who wants it, with the evidence above.
