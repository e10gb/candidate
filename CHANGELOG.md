# Changelog

Two histories live in this file: the desk we built, and the exchange it trades
against. The exchange section below is the vendor's, kept as shipped apart from
one annotation where it is factually wrong.

# Desk changelog

Full reasoning, measurements and retractions are in `NOTES.md`.

## v4.1 — 2026-08-19

- `TAKER_MODE` ships as `momentum`, the strategy as handed over. Reversion
  measured far better here but is fitted to this market's mean-reverting band and
  would invert in a trending one; it stays one variable away, with the evidence.
- **Statistics**: `campaign.sh` reports a 95% CI and how many runs an effect of a
  given size needs (~21 for 1,000). `tools/sweep.sh` runs configuration lists
  unattended into a CSV with per-config intervals. Every comparison made before
  this was underpowered by about an order of magnitude.
- **Pull rather than widen** (`QUOTER_PULL_MOVE`, off by default): leaves the book
  entirely after a sharp move instead of repricing into it. Widening measured
  worse because a reprice posts a fresh order into the move; pulling cannot.
- **Multi-contract quoting**: `QUOTER_FEED` accepts a comma-separated list, one
  quoter per contract sharing a sender-wide order-id allocator. Verified on
  `AAM6,AAU6` with zero id collisions.
- All three default to off or unchanged: each has a mechanism and tests, none has
  the sample size to justify moving a default.

## v4.0 — 2026-08-19

- **Desk split by flow type.** The quoter provides liquidity to uninformed flow and
  stays flat; the taker takes the reversion trade, which is where the money in this
  market is and which the quoter is forbidden to hold.
- `TAKER_MODE=reversion` (new, default; `momentum` restores the shipped behaviour).
  Momentum was the wrong sign for a market that mean-reverts inside a hard band:
  taker PnL went from -21,309 to +7,544/+7,199/+4,576/-1,938.
- `HEDGE_THRESH` 12 -> 20 and `TAKER_MAX_POS` 30 -> 15: with the taker holding a
  deliberate position, a tight threshold made the hedger close its winning trade
  every swing (taker +7,199, hedger -24,306). Wider tolerance, bounded source.
- `HEDGE_URGENT` is clamped up to `HEDGE_THRESH` and says so: below it, every hedge
  is urgent on arrival and the passive path is silently dead.
- Desk PnL over 5 runs: mean **-3,081** (stdev 5,552) against -19,392 for the
  configuration replaced. Break-even, not profitable -- stated as such.
- Tools added: `markout.py` (adverse selection per fill) and `leadsignal.py`
  (whether toxic flow is predictable before it arrives).

## v3.8 — 2026-08-18

- Completed the 2x2 the v3.7 correction called for: with the taker running the
  quoter is break-even *regardless of the hedger's feed* (-274 on AAM6, +1,092 on
  AAH6), killing the self-following-via-hedger hypothesis. The gap to the
  standalone +13,173 belongs to the taker's presence.
- Tested the one defensible taker lever, sizing it down (clip 1, max position 10):
  no PnL recovery (-240 +/- 12,145) and *worse* desk risk (34.9% of the session
  at/over 10 lots vs 18.1%). Reverted, nothing shipped. The failure is itself
  informative: a 3x size cut changing nothing means the damage is not
  proportional impact -- the taker trades at the same moments the market moves.
- No code changes; configuration ships as in v3.7. The claim stands as: risk
  control works; the quoter is profitable standalone and break-even in the
  shipped desk.

## v3.7 — 2026-08-18

- **Corrected the Job 1 headline.** The quoter's +13,173 was measured without the
  taker; the shipped desk runs it. Full desk, same layout, 3 x 240s: +1,092 +/-
  5,010 -- break-even, not profitable. Standalone profitability still holds and is
  reported alongside it.

- Risk is now reported as pooled quantiles rather than a mean and a max. A max
  grows with the number of samples, so it measured how long we watched rather than
  how the desk behaved -- which is why the quoted figure moved every time it was
  re-measured. Shipped configuration, 3 x 240s, 717 samples: mean 4.58, median 2,
  p95 20, p99 25; 18.1% of the session at/over 10 lots, 1.4% at/over 25.
- `tools/campaign.sh --full` runs the whole desk and pools risk across repeats.
  Two bugs in it fixed: it reported "no taker" when the taker was running, and it
  takes the seats' *code* defaults rather than the compose layout, so measuring
  what actually ships needs the env passed explicitly (now documented in the tool).
- Taker order ids: clock-seeded base36, matching the other two seats. A random
  start only made a restart collision unlikely, and the collision is silent (203);
  the old 8-digit decimal format would also have emitted a 9-character id past
  99,999,999. The guarantee is conditional on consuming ids more slowly than the
  clock advances -- documented, with ~30x headroom at current rates.
- Documented `HEDGE_INFLIGHT_TTL`; measured the hedger's rate cap at 3.0 req/sec
  against its limit of 20, so it is not binding and was left alone.

## v3.6 — 2026-08-18

- **Fixed a risk-reporting bug that hid real exposure.** The hedger retired
  *passive* fills from its in-flight bridge, which only `cross()` ever adds to,
  cancelling its own position out of the desk calculation: it reported `desk=-7`
  while the seats held -65 between them. Aggressive fills only, the retirement can
  no longer flip sign, and the bridge now expires after 1s so no unconfirmed path
  can mislead indefinitely.
- Risk is now measured and reported on `held()` (position actually at the
  exchange), not the bridged view the hedger acts on. Honest figures: mean 3.9-5.3,
  max 22-25 -- earlier notes quoted ~1.5, which was the bridged number.
- Removed the redundant price-relative edge cap: two knobs for one job, and it
  measured no better than the absolute one.
- Added `strategy/README.md` and `hedger/README.md`; documented all six tools.

## v3.5 — 2026-08-18

- Both seats read `EX_META` at startup: rate limit derived from the exchange's
  declared `max_tps` with headroom (a guessed cap risks disconnection one way and
  slowness the other), `position_limit` clamps sizing, `ticksize` puts prices on
  the grid. Locally `max_tps=0`, so the quoter runs at the market's own event
  frequency; the 50ms requote sleep is gone (8.2 -> 56.2 req/s measured).
- Quoter campaign at market speed, 3 x 240s: **+13,173 +/- 6,066, all runs
  positive** on the quieter sibling priced off the auto-detected lead -- the
  first measured profit. (Qualified in v3.7: that was measured without the taker.
  With it, as shipped, the quoter is +1,092 +/- 5,010 -- break-even.) Front month stays negative (-10,002): speed cannot
  dodge an atomic sweep; it wins the repricing race on the lagging contract.
- Desk layout ships accordingly: quoter on AAM6, taker on AAH6, hedger watching
  every contract (`ex.md.*.<sender>`) and hedging combined exposure in AAH6.
  Verified full-stack: desk exposure max 11-13, mean ~1.9, zero rejects.

## v3.4 — 2026-08-15

Submission state. Quoter and hedger complete, taker repaired, tests and tooling in
place.

- **Quoter** (`strategy/`, Go): rests two-sided liquidity with an edge sized from
  measured volatility, capped; inventory skew as a fraction of that edge; a
  minimum-edge floor so skew can never quote through fair value. Position and
  fills taken from the exchange's own feed. Cancel-by-id only. Token-bucket rate
  limiting. Pulls quotes on shutdown.
- **Hedger** (`hedger/`, Python): keeps the combined desk position near zero by
  crossing the spread with `F` orders. Derives every seat's position from
  `ex.md.<FEED>.<sender>` rather than any seat's self-report, tracks in-flight
  volume so it cannot double-hedge, and reads partial fills back off the reply.
- **Taker** (`taker/`): five bugs fixed (order subject, sell sign, partial fills,
  fill prices, PnL marking) plus three structural fixes (BBO dedupe, trade
  cooldown, self-imposed rate limit). It had never traded a single lot.
- **Tests** (`tests/`): 47 checks in four layers -- Go unit, Python unit, protocol
  assertions against a live exchange, and an end-to-end smoke test.
- **Tooling** (`tools/`): market watcher, benchmark harness, run summariser,
  transcript exporter.
- `run.sh` now writes `runs/run-<timestamp>.md` on exit.

Measured on the sample market: desk exposure max 4-12 and mean ~1.5 lots while the
seats carried 30+ gross; the quoter's edge change was worth roughly an order of
magnitude against the original fixed edge of 2. Absolute PnL remains noisy and the
taker is the dominant loss.

## v3.3 — 2026-08-15

- Hedger built and the desk-flat result established. Fixed an in-flight accounting
  bug that had let desk exposure reach 55.
- Quoter: minimum-edge floor, `E`/`T` fill handling, reject 305 treated as success.

## v3.2 — 2026-08-14

- First working quoter: fair value from the BBO mid, fixed edge, inventory skew,
  queue-preserving requoting, monotonic order ids.

## v3.1 — 2026-08-14

- Exchange probed from an empty book before any code was written. Established the
  behaviours the desk depends on, several of which contradict the documentation.


# Exchange changelog

## v2.4 — 2026-05

- Multi-shard matching: instruments are partitioned across matching shards
  (`EX_SHARDS`). Ordering guarantees are per-instrument, as before.
- Hot-path allocation removed; per-event latency counters added.
- Minor perf imperoments using boost containers

## v2.3 — 2026-02

- Matching: `F` (fill-and-kill) orders now execute atomically — they fill in
  full or reject. Partial executions no longer occur; internal strategies have
  been simplified accordingly. 
- Per-feed transaction rate limiting (`max_tps`, reject codes `306`/`307`).
  Exceeding rate will disconnect user

## v2.2 — 2025-11

- Self-trade prevention: per-feed opt-in via `Q`, opt-out via `W`.
- `X` (cancel-many) accepted across instrument groups.
- split `E` into `E` and `T`

## v2.1 — 2025-09

- Market data and best-bid/offer moved to JetStream (`ex.md.<feed>.<sender>`,
  `ex.bbo.<feed>`); instrument metadata published to the `EX_META` KV bucket.

## v2.0 — 2025-08

- Protocol v2: space-separated ASCII over NATS request/reply. Order types
  `L`/`M`/`F`; reject-code overhaul.
- Added STP; on by default

## v1.0 — 2025-06

- inital mvp; can enter cancel and trade orders
