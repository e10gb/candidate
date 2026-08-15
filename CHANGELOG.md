# Changelog

Two histories live in this file: the desk we built, and the exchange it trades
against. The exchange section below is the vendor's, kept as shipped apart from
one annotation where it is factually wrong.

# Desk changelog

Full reasoning, measurements and retractions are in `NOTES.md`.

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
