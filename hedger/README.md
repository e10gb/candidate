# Hedger (Python)

Keeps the desk's **combined** position -- quoter + taker + hedger, summed across
every contract they trade -- near zero. This is the desk's risk control: a small
position held for a while is cheap, a large one held for a second is not.

`NOTES.md` in the repo root carries the measurements behind every choice here.

## How it works

1. **Watches every seat's fills** on `ex.md.*.<sender>`, for all three senders.
   The wildcard is on the feed token because the seats do not all trade the same
   contract: the quoter sits on a quieter sibling of the taker's front month.
2. **Sums them into one desk position.** Contracts on one underlying are
   cash-settled at their listed price, so a lot of any of them carries the same
   unit risk. The residual after hedging is the inter-month spread, which is small
   beside the outright risk being removed.
3. **Hedges in `HEDGER_FEED`**, the liquid contract, when the total breaches
   `HEDGE_THRESH`.
4. **Passive first, then patient no longer.** Modest exposure rests one tick
   inside the touch for up to `HEDGE_PASSIVE_MS`; exposure at or above
   `HEDGE_URGENT` crosses immediately. The resting order is pulled early if the
   desk goes flat, if exposure flips sign, or if it grows urgent.

## Things that are deliberate

- **Positions come from the exchange, never from a seat's self-report.** The
  shipped taker reported `pos=0` while being unable to trade at all, and booked
  sells as buys once it could. A hedger that trusts its siblings' bookkeeping
  inherits their bugs. `E` (rested) and `T` (crossed) are both handled -- they are
  not duplicates of each other.
- **`F` orders fill partially**, despite what `CHANGELOG.md` v2.3 and
  `sim/market.py` both claim. Every send reads the traded volume back off the
  reply; the remainder is re-hedged next tick.
- **In-flight volume is tracked** between firing a hedge and seeing it echoed
  back, otherwise the desk looks unhedged and the same hedge fires repeatedly.
  Retired by *signed* amount -- doing it by magnitude let desk exposure reach 55.
- **Price is a slippage limit, not a target.** The exchange matches at the resting
  orders' prices, so `best_bid - HEDGE_SLIP` sweeps from the touch while refusing
  to pay more than `SLIP` through it.
- **Limits are read from `EX_META`, not guessed.** A rate cap guessed too high
  gets the sender *disconnected*, and a disconnected hedger means the desk has no
  risk control at all.

## Configuration


> **The shipped desk overrides some of these.** The table below is the code's
> own defaults, which apply when a variable is unset. `docker-compose.yml`
> sets several of them for the desk as configured, with the measurements
> that justify each in comments beside them. Where the two differ, compose
> wins at runtime.

| Variable | Default | Meaning |
|---|---|---|
| `NATS_URL` | `nats://127.0.0.1:4222` | exchange connection |
| `HEDGER_SENDER` | `HEDGE001` | our 8-char sender id |
| `SENDER` | `QUOTE001` | the quoter's sender, to watch its fills |
| `TAKER_SENDER` | `PYTKR001` | the taker's sender, to watch its fills |
| `HEDGER_FEED` | `$TAKER_FEED`, else `AAH6` (compose: `AAM6`) | contract we hedge *in* |
| `HEDGE_THRESH` | `5` (compose: `20`) | desk position that triggers a hedge. Raised because the taker now holds a deliberate reversion position, and a tight threshold made the hedger close its winning trade every swing |
| `HEDGE_URGENT` | `15` | exposure at/above which we cross immediately. Clamped up to `HEDGE_THRESH` if set below it -- underneath the threshold every hedge is urgent on arrival and the passive path is silently dead |
| `HEDGE_PASSIVE_MS` | `300` | how long to rest before giving up and crossing |
| `HEDGE_PASSIVE_IMPROVE` | `1` | ticks to improve on the touch when resting |
| `HEDGE_CLIP` | `25` | max size per hedge order (clamped by `position_limit`) |
| `HEDGE_SLIP` | `10` | ticks through the touch we will pay when crossing |
| `HEDGE_INTERVAL` | `0.05` | seconds between checks |
| `HEDGE_MAX_TPS` | `20` | request rate cap; lowered if `EX_META` declares tighter. Measured at 3.0 req/sec against it, so it is not binding |
| `HEDGE_INFLIGHT_TTL` | `1.0` | seconds a fired-but-unconfirmed hedge may keep suppressing further hedging |

## Reporting

Prints once a second and publishes to `strat.<HEDGER_SENDER>.status`:

```
desk=0 quoter=-11 taker=9 hedger=2 inflight=0 hedges=16 traded=92
cash=-3440 pnl=-2025 liq=-2049 passive=241 crossed=251 cost/lot=1.77
```

`cost/lot` is how far from the mid its fills landed, signed so paying up is
positive. That is the price of the risk control, and it is mechanical rather than
path-dependent, so it is comparable across short runs where PnL is not.

## Testing

```bash
../tests/run_tests.sh unit      # 23 unit tests, no network needed
```
