# Quoter (Go)

The desk's market-making seat. Rests two-sided liquidity on one contract and earns
the spread; it takes no directional view. `NOTES.md` in the repo root carries the
measurements and reasoning behind every choice here, including the ones that were
tried and rejected.

## How it prices

1. **Fair value.** The mid of the contract it quotes -- unless a *sibling contract
   is more actively traded*, in which case it prices off that one plus a learned
   basis. Contracts on the same underlying share a two-character prefix
   (`PROTOCOL.md`), so the leader is discovered at runtime rather than configured.
   Pricing off a quieter sibling would import lag, so the rule is "follow the
   busiest, unless that is us".

   **In the shipped configuration this resolves to "us", and that is a known bug.**
   Activity is counted in BBO updates per feed, and our own quoting generates BBO
   updates on our own feed. Measured with no desk running, AAH6 leads AAM6 by 11%
   (4,338 vs 3,897 in 25s) -- a margin our own requoting more than covers, so the
   quoter out-ticks the real lead and declares itself the leader. The status line
   reports `ref=own` for the whole run, and reference pricing is inert whenever the
   desk is actually trading. Fixing it means measuring activity in something we do
   not ourselves produce (counterparty trades rather than book churn); that change
   is not made here because run-to-run noise on this market is larger than any
   effect it could be shown to have in the runs available. See NOTES.md, Session 9.
2. **Edge.** `EdgeVolMult x stdev(mid)` over a rolling window, floored at
   `QUOTER_EDGE` and capped. A spread has to cover how far the price moves while
   you are holding, and that distance is a property of the market, not a constant:
   a fixed edge would be this sample market's move size memorised.
3. **Inventory skew.** Quotes lean against the position, as a fraction of the
   *current edge* so the lean keeps its strength at any width.
4. **Minimum-edge floor.** Skew may make the flattening side attractive, never
   unprofitable. Clearing inventory at a loss is the hedger's job -- it can do it
   in one trade instead of waiting to be lifted.
5. **Sizing.** Never quotes a size that could breach the position limit, which is
   itself clamped to the exchange's declared `position_limit`.

## Things that are deliberate

- **Position comes from the exchange, never from our own requests.** Fills are
  booked from `ex.md.<FEED>.<SENDER>`, handling both `E` (we rested) and `T` (we
  crossed) -- they are not duplicates, and handling only one silently loses fills.
- **Cancel by order id, never `X`.** Cancel-many does not reliably select by side
  and price (probed; see NOTES.md).
- **Order ids are never recycled.** They are consumed permanently per sender, so
  the generator is monotonic and seeded from the clock to survive a restart.
- **Limits are read, not guessed.** `EX_META` supplies `max_tps`, `ticksize` and
  `position_limit` at startup. A guessed rate cap risks *disconnection* one way
  and needless slowness the other.
- **Rate limiting is a token bucket, not fixed spacing.** Repricing both sides is
  four requests; forcing them apart left us on stale quotes while well under
  budget.
- **Requoting leaves correct orders alone**, preserving queue position, and acts
  only on genuine top-of-book changes -- the feed republishes constantly.

## Configuration

All optional; defaults in brackets. Empty values are treated as unset.
> **The shipped desk overrides some of these.** The table below is the code's
> own defaults, which apply when a variable is unset. `docker-compose.yml`
> sets several of them for the desk as configured, with the measurements
> that justify each in comments beside them. Where the two differ, compose
> wins at runtime.


| Variable | Default | Meaning |
|---|---|---|
| `NATS_URL` | `nats://127.0.0.1:4222` | exchange connection |
| `SENDER` | `QUOTE001` | our 8-char sender id |
| `QUOTER_FEED` | `$TAKER_FEED`, else `AAH6` (compose: `AAM6`) | contract(s) to quote; comma-separated for several, e.g. `AAM6,AAU6` |
| `QUOTER_CLIP` | `5` | size per quote |
| `QUOTER_MAX_POS` | `25` (compose: `10`) | max absolute position (clamped by `position_limit`) |
| `QUOTER_EDGE` | `2` | floor on the half-spread |
| `QUOTER_EDGE_VOL` | `4.0` | edge as a multiple of measured volatility; `0` = fixed edge |
| `QUOTER_MAX_EDGE` | `20` (compose: `40`) | absolute cap on the edge |
| `QUOTER_VOL_WINDOW_MS` | `2000` | volatility measurement window |
| `QUOTER_SKEW_FRAC` | `1.0` | inventory lean at full position, as a fraction of the edge |
| `QUOTER_MIN_EDGE` | `1` | margin every quote keeps against fair value |
| `QUOTER_USE_REF` | `1` | price off the busiest sibling contract; `0` disables. Currently inert in practice -- see the note under "How it prices" |
| `QUOTER_BASIS_ALPHA` | `0.05` | EWMA weight for the learned lead-to-ours offset |
| `QUOTER_REF_STALE_MS` | `2000` | ignore the lead if it has not updated within this |
| `QUOTER_PULL_MOVE` | `0` (off) | leave the book entirely when the price moves this far within `QUOTER_PULL_WINDOW_MS`. Pulling, not widening: widening changes the target price, so reconcile cancels and re-adds, and the re-add posts a fresh order into the move |
| `QUOTER_PULL_WINDOW_MS` | `200` | window over which that move is measured |
| `QUOTER_PULL_MS` | `300` | how long to stay out once triggered |
| `QUOTER_MAX_TPS` | `-1` (auto) | request rate cap; auto derives it from `EX_META` |
| `QUOTER_MAX_BURST` | `8` | requests allowed back-to-back |
| `QUOTER_MIN_REQUOTE_MS` | `0` | floor on time between requote cycles |

## Building and testing

Built from source inside the Dockerfile (`CGO_ENABLED=0`, hash-verified against
`go.sum`). There is no Go toolchain requirement on the host:

```bash
docker run --rm -v "$PWD":/w -w /w golang:1.23-alpine go test ./...
../tests/run_tests.sh unit          # this plus the Python seats' tests
```

## Quoting several contracts

`QUOTER_FEED` accepts a comma-separated list. Each contract gets its own quoter
with its own book, inventory and position limit; they share only the NATS
connection and an **order-id allocator**, because ids are consumed per *sender*
rather than per feed -- two allocators seeded from the wall clock would start
within a millisecond of each other and collide silently as reject 203.

The hedger needs no change: it already sums exposure across every contract via
`ex.md.*.<sender>`.

Verified live on `AAM6,AAU6`: both books quoted and filled, the hedger summed the
two positions correctly, zero 203s.

**It ships quoting one contract.** More books means more independent spread
capture, but whether it pays here is not established -- run-to-run noise on this
market is around 5,500 on a desk total of similar size, so it needs a proper
sweep (`tools/sweep.sh`, ~20 runs per configuration) rather than the two or three
that would fit in a session. The capability is there; the claim is not made.
