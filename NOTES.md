# Notes

## Summary

**What I built.** A quoter (`strategy/`, Go) that rests two-sided liquidity with an
edge sized from measured volatility, and a hedger (`hedger/`, Python) that keeps the
desk's combined position near zero by crossing the spread. The provided taker is
repaired but deliberately not retuned. Tests, a benchmark harness and a run
summariser are in `tests/` and `tools/`.

**The finding that shaped everything.** The documentation is wrong in several
places, and each error fails *silently* rather than loudly:

| claim | reality |
|---|---|
| `F` orders fill in full or reject (CHANGELOG v2.3, repeated in `sim/market.py`) | they fill partially |
| `T` duplicates `E` (implied by PROTOCOL) | `E` goes to the resting side, `T` to the aggressor; a seat needs both |
| order ids are yours to choose | they are consumed permanently, even after a cancel |
| `206`/`305` are interchangeable | different causes; both mean "not resting" |
| the shipped taker works | it had never traded a single lot |

So the desk's rule is: **every seat derives its position from the exchange's own
fill feed, never from what it asked for or what a sibling reports.** A hedger that
trusts its siblings' bookkeeping inherits their bugs.

**Design decisions worth knowing.**

- The quoter's edge is `EdgeVolMult * stdev(mid)`, not a tick count, so it adapts to
  a market with different absolute moves. The multiplier is a risk appetite; the
  resulting price is not a constant fitted to this sim.
- The quoter never quotes through a minimum margin. Clearing inventory at a loss is
  the hedger's job -- it can do it in one trade instead of waiting to be lifted.
- Cancel by explicit order id, never `X` (cancel-many), which does not reliably
  select by side and price.
- Every seat rate-limits itself. Breaching `max_tps` *disconnects* the sender rather
  than rejecting, which would end the session.

**Results on the sample market.** Desk exposure held to a max of 4-12 lots and a
mean near 1.5 while the seats carried 30+ gross -- Job 2 works, repeatably. On Job 1,
sizing the edge to volatility is worth roughly an order of magnitude against the
original fixed edge of 2. Absolute PnL is noisy enough that finer comparisons do not
survive repetition (see the retraction below), and the taker is the dominant loss and
the dominant variance.

**What I would not claim.** That any particular wide-edge setting is optimal; runs of
identical configuration differ by more than the effects being compared. Resolving
that needs ~10 repeats or 10-minute runs. I chose to record the uncertainty rather
than present a tuned-looking number a further run would overturn.

**Known limitations.** One instrument only (`AAH6`). The taker still loses money on
its own merits -- momentum in a market that mean-reverts inside a band -- and I left
its parameters alone rather than fit them to a market the brief says is not the
graded one. The sample market deadlocks permanently if its book ever empties, which
is a property of `sim/market.py`, not the desk.

**How to read the rest of this file.** It is the working record, in the order things
were discovered, including the wrong turns and two retractions. Roughly: exchange
probing and the reject-code catalogue; what the sample market looks like; the
taker's bugs; why the first quoter lost money; the hedger; the three-seat result;
the test suite; the edge sweeps and the methodology correction; clean-checkout
verification.

## Session 1 — probing the exchange before writing anything

Method: brought up `docker compose up -d` with **no** `--sim`, so the book starts
empty and every effect is something I caused. Listened on `ex.md.AAH6.*` and
`ex.bbo.AAH6` with `nats sub` in the background, sent orders with `nats req`, and
read replies plus the resulting market data. Findings below are all reproducible
against the shipped image.

### Confirmed by experiment

**1. `F` (fill-and-kill) fills partially. The changelog is wrong.**
`CHANGELOG.md` v2.3 claims `F` orders "fill in full or reject" and that partial
executions no longer occur. Against a resting bid of 10 @ 595, an `F` sell of 25
replied `Y 10` — it traded 10 and killed the remaining 15. `PROTOCOL.md:37` is the
accurate description. Anything that assumes an `F` is all-or-nothing (a hedger
firing a clip and assuming it's flat) will silently carry residual risk.

**2. Order ids are consumed permanently, per sender — cancelling does not free them.**

```
A AAH6 RECYC001 B 5 585 L   -> Y 0     (rests)
C AAH6 RECYC001             -> Y 1     (cancelled)
A AAH6 RECYC001 B 5 585 L   -> N 203 re-used order id
```

A quoter that re-quotes on every BBO update burns ids continuously, so it needs a
monotonic id generator, never a recycled pool. 8 chars is plenty of space
(base36 counter), but the generator has to survive for the whole session.

**3. `X` (cancel-many) does not reliably select by side+price. Do not depend on it.**
With two of my own orders resting at 585, `X AAH6 B 585` replied `Y 0`; cancelling
each by id immediately afterwards replied `Y 1` each, proving they were resting and
mine. Other prices did cancel (`X AAH6 B 590` -> `Y 1`, `X AAH6 X 611` -> `Y 3`), so
it is inconsistent rather than simply unimplemented. I could not derive the rule
from the reply codes. **Design decision: cancel by explicit order id everywhere.**
Cost is one request per resting order, which is acceptable given `max_tps=0` locally
(see open questions).

Feed wildcards: `AA??` is accepted (never `N 100`) but matched nothing in my tests;
`AA*`, `AA`, and `*` are all rejected `100 malformed`. So the wildcard char is `?`,
but I have no positive confirmation it ever selects.

**4. Reject-code catalogue** (`PROTOCOL.md` documents only 100/202/203):

| Code | Text                | Triggered by                                  |
|------|---------------------|-----------------------------------------------|
| 100  | malformed request   | too few fields, volume 0, order id != 8 chars |
| 200  | bad sender identity | msg sender != sender in the subject           |
| 202  | bad feedcode        | unlisted feed                                 |
| 203  | re-used order id    | any id used before, even if cancelled         |
| 204  | bad order type      | type not in `M`/`L`/`F`                       |
| 205  | bad side            | side not in `B`/`S`                           |
| 206  | orderid not used    | cancelling an id this sender never used       |
| 300  | volume too high     | volume > `max_volume`                         |
| 302  | price out of range  | price outside `ref_price ± band`              |
| 305  | orderid not active  | cancelling an id that was used but is no longer resting (already cancelled, or filled) |

`306`/`307` (rate limiting, per the changelog) not yet reached — see below.

**206 vs 305 matters.** They look interchangeable and are not:

```
C AAH6 NEVER001   (never used)          -> N 206 orderid not used
C AAH6 LIVE0001   (live)                -> Y 1
C AAH6 LIVE0001   (again)               -> N 305 orderid not active
C AAH6 FILL0001   (after it filled)     -> N 305 orderid not active
```

Both mean "not resting", which is the state a cancel is trying to reach, so both
are success for a quoter. Only 206 was treated as benign at first, and 305 turns up
routinely in the full-stack run whenever a fill and a requote race — the fill
removes the order a moment before the cancel lands. That made `reconcile` abandon
the cycle while still holding a record of an order that no longer existed, so the
side stopped being requoted until an unrelated `C` message cleared it.

**5. Negative prices are legal.** `B 10 -50 L` was accepted (`Y 0`). The band is
`ref_price ± band` = 600 ± 5000, so the valid range is -4400..5600 and -50 sits
inside it. Not a bug, but a quoter that assumes price > 0 when computing a spread
around a low mid could send something it did not intend.

### Open questions

- **`max_tps=0`** in `exchange/instruments.txt`. Unknown whether 0 means unlimited
  or no transactions permitted (empirically it is not blocking me, so: unlimited
  locally). The changelog says exceeding the rate **disconnects the user**, which
  is a session-ending event, not a reject to retry. The grading market is not this
  market and may well set a real limit, so both seats need self-imposed rate
  limiting regardless of what the local file says.
- **`X` selector semantics** — unresolved, see above. Working around it rather than
  solving it.
- **STP** — `CHANGELOG` v2.0 says on by default, v2.2 says per-feed opt-in via `Q`.
  Not yet tested. Matters because three of our seats quote and take on one book;
  need to know whether our own seats can trade with each other, and whether that
  shows up as desk position moving with no net exposure change.
- **Position limits**: local `position_limit` is 1e9, effectively unlimited. The
  grading market will differ; do not build anything that relies on the limit being
  generous.

### What the sample market looks like

Watched with `tools/watch.sh` (written for this — one subscription on `ex.>`,
filtered to a feed, prints only genuine top-of-book changes).

Spread is typically 1-5 ticks with 5-15 lots a side, and trades arrive in bursts
from both directions. The important number is the drift: across three captures a
couple of minutes apart the mid went **622 -> 560 -> 454 -> 440**. The market
wanders enormously relative to its own spread — you can earn 2 ticks quoting and
lose 100 holding the inventory.

That sets the whole design. Symmetric two-sided quotes left resting through a move
like that lose far more to inventory than they earn in spread, so the quoter has to
skew against its position and the hedger has to cut accumulated exposure quickly.

Also: the BBO feed republishes on **every book event**, not only when the top
changes — I logged dozens of identical `622 5 625 13` messages microseconds apart.
Re-quoting on every BBO message would burn order ids (which are finite per sender,
see above) and TPS for nothing. Act on real top-of-book changes only.

### The taker is broken (three bugs)

`taker/` is "legacy code; you own it now", and it does not work at all.

1. **It has never traded.** `taker.py:85` sends orders to bare `ex.req`; the
   exchange listens on `ex.req.>`, which does not match a subject with no trailing
   token. Verified directly: `nats req ex.req ...` -> *"No responders are
   available"*, while `nats req ex.req.PYTKR001 ...` -> `Y 1`. The bare
   `except: return` on the next line swallows the timeout, so it reports
   `pos=0 pnl=0` and looks healthy.
2. **Sells increase its position.** `taker.py:101` is
   `signed = qty if side == "B" else qty` — both branches identical, so a sell is
   booked as a buy. Position and cash are both wrong the moment it trades.
3. **It assumes `F` fills in full.** `taker.py:96` hardcodes `filled = CLIP` and the
   comment dismisses `parts[2]` as "just an exchange-side detail field". That field
   is the traded volume, and `F` demonstrably fills partially (finding 1). Fixing
   only the subject would give it silently wrong positions.

**Design consequence for the hedger:** do not trust any seat's self-reported
`strat.<sender>.status`. Positions get derived from the exchange's own
`ex.md.<FEED>.<sender>` fill feed, which is ground truth and cannot drift from
whatever a seat believes about itself. This is worth doing even with a correct
taker — a hedger that depends on its siblings' bookkeeping inherits their bugs.

#### Fixed — and there were five, not three

The three above, plus two more that only became visible once it could trade:

4. **Fills were booked at the wrong price.** The reply gives the volume that
   traded but *not* the price, and a marketable order fills at the resting orders'
   prices, not at our limit — `F` buys fill at or below the limit, sells at or
   above. Booking `filled * limit_price` therefore overstated every purchase and
   understated every sale, both pushing the same way. Measured over 60s: it
   reported `cash=-22820` where the exchange's own feed said `-7731`, making PnL
   read -20,951 against a reality nearer -5,700. Fixed by booking fills in a new
   `on_md` handler from `ex.md.<FEED>.<SENDER>` — the same ground-truth rule the
   hedger already follows — and handling both `E` and `T` there.
5. **`pnl()` valued the position at zero whenever the mid was unavailable**, the
   identical bug the quoter had: it reported cash as profit. Now falls back to the
   side we would have to trade against, then to the last price seen, and returns
   `n/a` rather than guessing.

Also changed the default `TAKER_FEED` from `BTH6`, which is not a listed
instrument, to `AAH6`. An unconfigured local run previously got `202 bad feedcode`
on every order — another way this failed quietly rather than loudly. `taker/README.md`
updated to match.

**Verified against the exchange, not against itself.** 45s run, taker's own report
versus an independent recomputation from the raw fill feed:

```
taker reports : pos=-24 cash=2112 pnl=-8556 fills=398
exchange truth: pos=-24 cash=2112            fills=398
```

Before the fixes the same comparison was `pos=3 cash=-22820` against `pos=3
cash=-7731` — position right, cash wrong by 3x. Worth noting the shape of that:
the position error was the loud, obvious bug and the cash error was the quiet one
that still looked plausible.

### Why v1 of the quoter loses money

First working quoter: fair = BBO mid, quote at fair +/- EdgeTicks, skew both quotes
against inventory, cancel-replace only when the target price changes. Position
control worked immediately (held -9..+6 against a limit of 25, flattened itself
repeatedly). Profitability did not: about -270 over 15s, and -8,000 to -13,000 over
95s.

**First diagnosis, wrong-ish.** I had set `SkewTicks`(4) > `EdgeTicks`(2), so once
the position passed `MaxPos * Edge/Skew` (~12) the skew quoted *through* fair value
— deliberately selling below fair to get flat. Real bug, and the `fill B 5 @ 650 /
fill S 5 @ 650` pairs in the log are it. Fixed with a minimum-edge floor: skew may
make the flattening side more attractive but never past `MinEdgeTicks` of margin;
the discouraging side stays unbounded. Clearing inventory at a loss is the hedger's
job, and it can do it in one trade instead of waiting to be lifted.

**But the floor barely moved PnL**, which meant the real cause was elsewhere.

**Actual cause: the sample market is built to run over resting quotes.**
`sim/market.py:201` — the "mover" picks a target 20-60 points away every 2.5-4.5s
(doubled for ~1/3 of moves, so up to 120), then sweeps with an order sized
`1.2 x` everything resting in its path. `sim/market.py:142` — the background
quoters rest 2-5 ticks off centre. So an edge of 2 puts us permanently at the top
of the book: first in the queue for the casual flow we want, and first in the path
of a 40-point sweep every three seconds.

Arithmetic: ~30 sweeps per 95s x 5 lots x ~40 points ~= 6,000. That is the observed
loss, so the accounting is trustworthy and the strategy is simply mispriced. Fills
where we bought at 537 and sold at 496 two hundred milliseconds later are this.

**Evidence that edge width is the lever** (95s, sample market):

| EdgeTicks | fills | final PnL |
|-----------|-------|-----------|
| 2         | 164   | -12,946   |
| 15        | 53    | -7,538    |
| 35        | 54    | -3,655    |

Caveat on that table: the three ran **concurrently**, so the tight quoter absorbed
sweeps that would otherwise have reached the wider two, which flatters the wide
numbers. The ordering matches the theory but needs sequential runs to be
conclusive. Recording it as directional evidence, not a result.

**What I am not going to do: tune EdgeTicks to 35.** The grading market is not this
market (TASK.md), and 35 is fitted to this mover's step size. The generalisable
statement is *edge must scale with realised volatility* — so the quoter should
measure how far the mid actually moves over a rolling window and set its edge from
that, which adapts to whatever the grading market does.

Two notes found while reading the sim:

- `sim/market.py:179` repeats the changelog's false claim that `F` "executes
  atomically (fills in full or rejects), so there is no partial-execution handling
  here". Same myth in a third place. The sim uses `publish` rather than `request`
  for order entry, so it never sees a reject and never notices.
- The mover's target is clamped to [440, 760], so the sample market mean-reverts
  inside a band. Anything that appears profitable by leaning on that band is
  fitted to the sim and will not survive grading.

### `E` vs `T`: who gets told about a fill (undocumented)

PROTOCOL.md describes `E` as "an execution" and `T` as reporting "a trade --
useful for watching trade flow", which reads like `T` is a duplicate of `E` for
observers. It is not. Traced by subscribing to every `ex.md.AAH6.*` subject through
a single match:

```
ex.md.AAH6.MOVER001   E AGGR0003:aggsell3 MOVER001:18038059 6 574 90585 S
ex.md.AAH6.AGGR0003   T AGGR0003:aggsell3 MOVER001:18038059 6 574 90585 S
```

**A seat gets `E` on its own subject when it was the resting side of a match, and
`T` when it was the aggressor.** Same fields, different recipient, never both for
one fill. So:

- Deriving your position from `ex.md.<FEED>.<you>` requires handling **both**.
- Filtering to `E` only (which both of my seats did at first, on my wrong
  assumption that `T` was a duplicate) silently drops every fill made by crossing
  the spread — i.e. *every* fill the hedger makes. It reported `hedger=0` while
  actually trading.
- One `F` order also arrives as **several** messages (a sell of 10 came back as
  3@757, 5@756, 2@756). PROTOCOL's "accumulate them" is real.

Blind alley worth recording: an intermediate probe showed a resting order receiving
no fill message at all, which looked like it contradicted the above. It did not —
that order simply never traded, because the aggressor matched a better-priced order
first. I nearly drew a conclusion from it.

### The hedger

`hedger/hedger.py`. Watches all three seats' fills on `ex.md.<FEED>.<sender>`,
sums them into a desk position, and crosses the spread with `F` orders when the
total breaches `HEDGE_THRESH`.

Design points:

- **Positions come from the exchange, never from `strat.<sender>.status`.** The
  shipped taker reports `pos=0` while being incapable of trading; a hedger that
  trusted it would inherit that. Verified the principle on our own output too:
  recomputing HEDGE001's position independently from the raw feed gave -10 against
  a self-reported -10.
- **Partial fills are read back off the reply.** `F` fills partially, so the send
  applies `Y <n>`, not the requested size, and the remainder is re-hedged next tick.
- **In-flight bridge.** Between firing a hedge and seeing the fill echoed back, the
  desk would otherwise look unhedged and we would fire the same hedge again. A
  signed `inflight` counter covers the gap.
- **Price is a slippage limit, not a target.** The exchange matches at the resting
  orders' prices, so `best_bid - HEDGE_SLIP` sweeps from the touch while refusing
  to pay more than `SLIP` ticks through it.

**Bug found and measured.** The first version retired in-flight volume by magnitude
toward zero rather than by signed amount, which corrupted the count as soon as
hedges in both directions were outstanding. It fired 629 hedges and 15,225 lots in
60 seconds while the desk swung to +/-55. One-line fix (`inflight -= signed`), same
60-second workload:

| | before | after |
|---|---|---|
| max abs desk position | 55 | **5** |
| mean abs desk position | 23.55 | **0.32** |
| samples at/over threshold | 63% | **2%** |
| hedges fired | 629 | 14 |
| lots traded | 15,225 | 73 |

The quoter's inventory is now mirrored almost exactly by the hedger (quoter +25,
hedger -25, desk 0), which is what Job 2 asks for.

I also suspected the hedger was crossing into the desk's own quoter, which would be
churn for nothing. Checked before acting: **zero** of its fills matched QUOTE001.
The oscillation was entirely the in-flight bug.

Still open on the hedger:

- Only exercised against a dead taker (`taker=0` throughout), so the three-seat
  case is untested until the taker's order subject is fixed.
- Hedging costs spread by design — that is the price of the risk control — but the
  hedger's own PnL is not separated out yet, so the cost is unquantified.
- `HEDGE_THRESH=5` / `CLIP=25` / `SLIP=10` are unjustified starting values, not
  tuned.

### First full-stack run (`./run.sh --sim --strategy`)

All six containers came up and the three seats talked to the exchange with no
compose wiring problems. Three things came out of it.

**1. PnL was reported wrongly, and flatteringly.** The run ended `pos=-14
cash=9065 pnl=9065` — PnL exactly equal to cash, while short 14 lots. The old
`status()` added `position * mid` only when the book happened to be two-sided and
otherwise reported raw cash, silently valuing the inventory at zero. Marking those
14 lots against the last traded price (~675) gives about **-385**, not +9,065.
The hedger's `cash=-5147` with `pos=+8` is about **+253** on the same basis, so the
desk was roughly **-130** over 70 seconds, not wildly profitable.

Fixed with `markPrice()`: live mid, else the side we would actually have to trade
against to get flat (bid if long, ask if short), else the last price seen — with
staleness flagged in the output (`pnl=-3302?`) rather than hidden, and `n/a` if a
position genuinely cannot be valued. Verified over a 40s run: 34 status lines
holding a position, 0 where `pnl == cash`.

A valuation that silently drops the position is worse than no valuation, because it
reads as a profit. Worth remembering when reading the graders' numbers too.

**2. The sample market can deadlock permanently.** From 00:33:16 the book was empty
and never recovered; nothing traded for the last 50 of 70 seconds. This is
`sim/market.py`, not us: the mover skips its turn entirely if either side is
missing (`market.py:224`), and the background quoters only requote when
`fair_value` is non-`None`, which needs at least one side. BBO messages only fire
when the book changes, so an empty book generates no messages and nothing wakes
anybody up. **Once drained, the sample market stays dead.**

Our hedger is the likely drainer: `HEDGE_SLIP=10` lets each hedge sweep up to 10
ticks of depth. Consequences to keep in mind:

- Long local runs need watching; a flat, silent tail may mean a dead market rather
  than a calm one, and any statistics over that window are meaningless.
- Our quoter refuses to quote one-sided, so it cannot restart the market either.
  Seeding a one-sided market would be taking a directional position, which is
  exactly what Job 1 says not to do, so this is left deliberate rather than fixed.
- The hedger correctly declines to trade into an empty book (no ask to buy from),
  which left the desk sitting at -6. Right call, but worth knowing as a failure mode.

**3. The taker sat at `pos=0 fills=0` for the entire run**, as predicted. The desk
is really two seats until its order subject is fixed.

### Three live seats: risk control works, profitability does not

First run with a *working* taker, so the hedger finally had two seats generating
inventory instead of one. 90s window, `./run.sh --sim --strategy` equivalent.

**Risk control — good.** While absorbing a mean gross exposure of 31 lots
(`|quoter| + |taker|`, peaking at 45):

```
max |desk| 5 | mean |desk| 0.81 | at/over threshold 1% of samples
```

Zero rejects and zero errors across all three seats for the whole run.

**Profitability — bad, on all three seats.** Final state, marked at ~440:

| seat | position | PnL |
|---|---|---|
| quoter | -10 | -8,496 |
| taker | -27 | -4,895 |
| hedger | +37 | ~-9,600 |

The hedger's loss is the new information. It traded **1,311 lots in 90 seconds**
across 215 hedges, and every one of those crosses the spread — that is the cost of
the risk control, and at this churn it is the largest single loss on the desk.

**This challenges a design decision I made earlier.** I capped the quoter's
inventory skew so it never gives up margin, on the argument that clearing inventory
is the hedger's job. But flattening via skew is *free* — the quoter still earns its
edge, just biased to one side — whereas flattening via the hedger *costs* the
spread every time. So the cheap flattener is doing less work than the expensive one.

`HEDGE_THRESH=5` compounds it: with mean gross exposure of 31 the hedger is
reacting almost continuously. TASK.md's own risk model says "a small open position
for a long time is relatively low risk; a very large open position even for just a
second is high risk" — which argues for a *higher* threshold that ignores small
exposure and only cuts large ones. The current setting buys a mean desk position of
0.81 at a price we now know is roughly 9,600 per 90 seconds.

Next experiment: sweep `HEDGE_THRESH` and let the quoter's skew do more of the
work, measuring both max |desk| and PnL, rather than assuming tighter is better.

### Benchmark harness and the `HEDGE_THRESH` sweep

`tools/bench.sh` runs the full desk against the sample market for a fixed window
and reports **risk and PnL together**, because optimising either alone gives you a
perfectly flat desk that loses money, or a profitable quoter that blows up. Risk is
reported as a distribution (max, mean, % of time over 10 and over 25), not an
endpoint — a desk averaging zero by oscillating between -40 and +40 is not flat.

Each run starts from `docker compose down`, for the stale-exchange reason below.
`SIM_SEED` is pinned in compose so runs face a comparable price path; the sim is
threaded, so this is closer-but-not-identical, not reproducible.

**The first sweep I ran was invalid.** `docker compose down` does not stop services
that belong to a profile — it removed only nats and exchange, then failed to remove
the network ("resource is still in use") while four seats kept running. The
following `up` then *reused* any seat whose config was unchanged, so a repeat of the
same setting silently continued from the previous run's positions and PnL. Fixed by
passing the profiles to every compose call and adding `--force-recreate`. The
corrected numbers below differ from the first attempt by a factor of two to three,
and reverse its conclusion.

60s runs, clean isolation, repeats where it mattered:

| HEDGE_THRESH | max abs desk | mean abs desk | >=10 | >=25 | hedger lots | quoter | taker | hedger | total |
|---|---|---|---|---|---|---|---|---|---|
| 2  | 10 | 0.97 | 1%  | 0% | 1,147 | -3,386 | -7,540 | **-4,701** | -15,627 |
| 5  | 12 | 1.19 | 1%  | 0% | 683   | -4,009 | -6,219 | +3,824 | -6,404 |
| 5  | 6  | 0.96 | 0%  | 0% | 617   | -1,066 | -7,135 | +3,100 | -5,101 |
| 15 | 15 | 4.82 | 12% | 0% | 222   | -4,246 | -6,143 | +2,774 | -7,615 |
| 15 | 18 | 5.15 | 16% | 0% | 152   | -3,126 | -8,328 | +4,016 | -7,438 |
| 30 | 28 | 11.43| 55% | 5% | 75    | -3,420 | -5,053 | +2,512 | -5,961 |

**Conclusion: keep `HEDGE_THRESH=5`.** It gives essentially the best risk profile
measured (mean abs desk ~1, nothing above 25 ever) at a PnL indistinguishable from
the best. Threshold 30 reaches similar PnL while spending 55% of the session above
10 lots and 5% above 25 — ten times the mean exposure for no measurable gain, which
is a bad trade under TASK.md's risk model. Threshold 2 is clearly worse on both
axes: 1,147 lots of churn and the only negative hedger PnL in the set.

**I retract the "raise the threshold" recommendation** from the contaminated run.
The reasoning behind it — that skew flattens for free and hedging costs spread — is
still sound in principle, but the data it rested on was wrong, and the real
measurements do not support it.

**The hedger makes money, which I did not expect.** +2,500 to +4,000 in every run
except the over-churning threshold 2. The mechanism is plausible rather than lucky:
the hedger systematically takes the opposite side of whatever the other seats have
accumulated, and the taker is a momentum strategy that loses. Fading a loser is
profitable. This is worth stating carefully — it means the hedger's measured
"profit" is really the taker's loss being partly recovered, not an independent edge,
and it would not survive the taker being fixed or removed.

**The taker is the dominant loss and the dominant variance**: -5,053 to -8,328
across every run, larger than the quoter's. It is a momentum strategy in a market
that mean-reverts inside a band (`sim/market.py` clamps its target to [440, 760]).
We own it, so its parameters are fair game.

Caveat on all of the above: n=1 or 2 per setting at 60s. Within-setting spread at
threshold 5 is ~1,300, so differences smaller than that are noise.

### Making the quoter profitable

**First, a latency bug of my own making.** Measured what the quoter actually does
per market update:

| | rate |
|---|---|
| BBO messages | 154/sec |
| genuine top-of-book changes | 24.8/sec |
| our order actions | 8.2/sec (~4 repositions/sec) |

Most of that filtering is correct — the dedupe drops messages carrying no
information, and `reconcile` does nothing when the recomputed target is unchanged,
which is most of the time since prices round to integer ticks. But the rate limiter
was implemented as a **fixed 50ms gap between every request**, so repricing both
sides (cancel+add twice) serialised into ~200ms of forced waiting *while sitting on
stale quotes* — despite the sustained rate being 8/sec against a budget of 20.
Neither throttle was saturated; the shape of the limiter was simply wrong.

Replaced with a **token bucket** (burst 8, refill at MaxTPS): a burst goes through
immediately, sustained rate is still bounded, which is the thing that actually
risks the disconnect. Normalised by market activity, we went from acting on 16.5%
of top-of-book changes to 23.5%, with zero rejects.

Worth fixing *before* tuning the edge: a wider edge compensates for being slow, so
tuning edge on top of an artificial 200ms delay would have tuned it to the wrong
number.

**Fixed-edge sweep** (120s each, full desk):

| EdgeTicks | quoter | taker | hedger | total | max abs desk |
|---|---|---|---|---|---|
| 2  | **-8,405** | -16,885 | +5,624 | -19,666 | 6 |
| 10 | -3,874 | -15,300 | +1,179 | -17,995 | 10 |
| 20 | -1,962 | -8,131  | +3,589 | -6,504  | 5 |
| 35 | **+2,276** | -10,246 | +1,873 | -6,097 | 4 |

Monotonic in the quoter's own PnL across four points, and risk is unaffected —
the hedger keeps the desk flat regardless of how the quoter is priced. `edge=35`
is the first configuration where the quoter makes money.

**But 35 is this sim's mover step size memorised**, and TASK.md says grading uses a
different market. So the edge is now sized from measured volatility:

    edge = clamp(EdgeVolMult * stdev(mid over VolWindow), EdgeTicks, MaxEdgeTicks)

Calibrating the multiplier from data rather than guessing: rolling 2s stdev of the
mid on the sample market has median **7.5**, mean 11.1, max 30.5. My first guess of
1.5 therefore produced an edge of ~11 — far too tight, and it lost (-1,279).
Reaching the empirically good ~35 needs a multiplier near 4.6.

| setting | quoter PnL |
|---|---|
| adaptive, mult 1.5 | -1,279 |
| adaptive, mult 3.0 | **+5,659** |
| adaptive, mult 4.5 | **+3,494** |
| fixed 35 (for comparison) | +2,276, +4,408 |

**Shipping `QUOTER_EDGE_VOL=4.0`.** The multiplier is a risk appetite ("quote four
sigma wide"), not a price level, which is the part that transfers to a market with
different absolute moves. The higher end of the profitable range is the calmer one
— less churn, lower mean desk exposure — which suits "low-risk" in the brief.

Confirmation run at the shipped defaults, 120s, full desk:

```
RISK  max|desk| 4   mean|desk| 0.69   >=10: 0%   >=25: 0%
PNL   quoter +1,074   taker -9,881   hedger -1,004
```

**Both jobs now demonstrably work**: the quoter earns its money quoting two-sided
(profitable in 5 of 5 runs at multiplier >= 3 or fixed edge >= 35, against -8,405
at the shipped-by-me default of 2), and the desk stays flat while it does.

Caveats I would not want glossed over:
- Quoter PnL among profitable configurations ranges +1,074 to +5,659, so the
  *ranking* within that group is noise. What is solid is the gap between an edge
  sized to volatility and the old fixed 2.
- n=1 per setting at 120s. The direction is consistent across seven runs; the
  magnitudes are not precise.
- **The taker is now the whole problem**: -9,881 at the shipped default and as bad
  as -28,860 in one run. It dwarfs the quoter's profit and dominates desk variance.
  Deliberately not tuned — it is a momentum strategy losing in a market that
  mean-reverts inside a hard band, and fitting it to that would be exactly the
  mistake avoided above.

### The taker: structural fixes, and what they did not fix

Three defects that are wrong in *any* market, so fixing them is not fitting to the
sample. Deliberately did not touch `TAKER_THRESH` or `TAKER_LAG`.

1. **It never deduped the BBO feed.** `mids` was appended on every message, but the
   feed republishes on every book event: 154 messages/sec against 24.8 genuine
   top-of-book changes/sec. So `TAKER_LAG=5`, documented as "BBO updates back the
   move is measured over", actually spanned ~32ms of mostly-identical quotes. The
   momentum signal was measuring noise over a window six times shorter than
   intended. Now records only distinct tops, and the docs say so.
2. **No cooldown.** `maybe_trade()` ran on every message and kept firing for as
   long as the signal held, turning one price move into a burst of orders.
3. **No rate limiting of any kind** — the only seat without it. Exceeding `max_tps`
   *disconnects* the sender (changelog v2.3), which is session-ending for a desk
   seat. `TAKER_COOLDOWN` (default 0.5s) now serves as both.

**Measured effect on behaviour — large.** Same isolated 45s test, before and after:

| | before | after |
|---|---|---|
| fill messages | 398 | **10** |
| lots traded | 966 | **26** |
| PnL (that run) | -8,556 | -317 |

It was trading roughly forty times more often than its own parameters intended.

**Measured effect on desk PnL — none I can demonstrate.** Two full-desk runs after
the fix gave taker PnL of -13,248 and -4,960, against a -9,881 baseline. That
straddles the baseline, and two runs of *identical* configuration differing by
8,300 is the real finding: this seat's PnL variance is larger than any effect I
have been able to produce in it.

The mechanism is consistent, though: it now takes far fewer trades but holds larger
directional positions for longer, so the loss per lot goes up as the lot count goes
down. Holding 30 lots through one of this market's 300-point excursions is ~9,000
regardless of how few orders it took to get there.

**Conclusion: the remaining loss is the strategy, not the implementation.** Momentum
in a market whose target mean-reverts inside a hard band (`sim/market.py` clamps to
[440, 760]) loses by construction. The implementation is now correct and honest —
it measures what it claims to measure and reports what actually happened.

I am not tuning it further. `TAKER_MAX_POS` is the one defensible knob if we want
to bound its damage, because capping worst-case exposure does not require believing
anything about direction — but the current 30 is already fully absorbed by the
hedger, so it is a PnL question rather than a desk-risk question, and any value I
picked would be fitted to a market that is explicitly not the graded one.

### Two ways this nearly fooled me

**1. A stale exchange silently kills the sample market.** The first full-stack
attempt showed all three seats at `pos=0 fills=0` for 100 seconds. The exchange
container had been up for hours across many sim restarts. Order ids are consumed
permanently per sender, the sim picks random starting ids for fixed senders
(`MOVER001` etc.) and publishes fire-and-forget, so it never sees `203 re-used
order id` — its seed orders were silently rejected and no market ever formed.
`docker compose down` between runs fixes it. Grading starts from fresh containers,
so this is a local-testing hazard, but a flat quiet log is worth two seconds of
suspicion before it is worth a conclusion.

**2. `timeout` does not exist on macOS.** Several diagnostic checks of the form
`timeout 5 nats sub ... 2>/dev/null` printed nothing, which I read as "no market
data". The command was not found; the error went to the suppressed stderr. I
briefly concluded the exchange had stopped publishing BBO and started theorising
about JetStream state. Re-checked with a backgrounded subscriber writing to a file
— the method that had worked all along — and found **3,766 BBO messages in 8
seconds**. The market had been fine the whole time. Suppressing stderr on a
diagnostic is how you turn a broken tool into a false finding.

### Test suite

`tests/run_tests.sh` — four layers, cheapest first, everything in containers so
there is no Go toolchain, nats-py or venv needed on the host.

| layer | what it covers | count |
|---|---|---|
| 1. Go unit | quoter pricing, skew floor, sizing, volatility window, marking, fill sides, token bucket, order-id uniqueness | 12 |
| 2. Python unit | hedger desk accounting and in-flight arithmetic, taker dedupe/cooldown/fill booking | 13 |
| 3. Protocol | the exchange behaviours the seats depend on, asserted against a live exchange on an empty book | 15 |
| 4. Smoke | full stack on the sample market: seats trade, desk stays bounded, no rejects, PnL marked | 7 |

`tests/run_tests.sh unit` runs layers 1-2 in seconds with no network.

**Layer 3 is the one I would keep if I could keep only one.** Every assertion in it
was discovered by experiment and contradicts or is missing from the shipped docs:
`F` fills partially (the changelog says otherwise, twice), order ids are consumed
permanently, 206 and 305 mean different things, and `E` goes to the resting side
while `T` goes to the aggressor. The seats' correctness rests entirely on these.
The grading exchange may be a different build — if one of these assumptions stops
holding there, a seat goes *silently* wrong rather than loudly broken, which is the
failure mode that cost the most time in this project. A failing protocol test says
which assumption died.

**The smoke test asserts the desk actually traded**, not merely that it ran without
crashing. A quiet, flat, zero-fill session looks perfectly healthy in the logs and
is exactly what both a dead sample market and the shipped taker looked like. It
also re-checks the two bugs whose symptom was *plausible-looking output*: desk
exposure staying bounded, and PnL never being reported as bare cash while a
position is open.

**Verified the suite can fail.** Reintroducing the sells-booked-as-buys bug
(`signed = qty if side == "B" else qty`) turns `test_sell_reduces_position` red;
reverting turns it green. A suite that has never failed is not evidence of anything.

### Clean-checkout verification

TASK.md requires `./run.sh --sim --strategy` to work from a clean checkout, and
everything up to this point had been run in a working directory with images
already built. Tested the way a grader would actually see it: extracted the
pristine original repo (`git archive 657d59d`), overlaid **only** the deliverables
— `strategy/ hedger/ taker/ tools/ tests/ NOTES.md TRANSCRIPT.txt
docker-compose.yml CHANGELOG.md` — and ran it in a differently-named directory.

Result: all three seats came up and traded, `desk=0`, **zero** error, reject or
missing-file lines. The deliverable list is complete: nothing the desk needs is
sitting untracked in the working directory.

It did catch one portability bug: `tests/run_tests.sh` hardcoded the Docker
network as `candidate_default`, but compose names the network after the
*directory*, so the protocol tests could never have run from a grader's copy. Now
derived from the running nats container. This is exactly the class of failure the
clean-checkout test exists to find, and nothing in the working directory could
have revealed it.

### The adaptive edge runs wider than I estimated

Measured the quoter's actual quoted spread over the clean-checkout run:

```
samples 89 | min 4 | median 78 | mean 99 | max 240
```

Against a market spread of typically 1-5 ticks. I had estimated ~30 per side from
`4.0 x median volatility 7.5`; the realised median is 78 *total*, and the maximum
of 240 is exactly twice `MaxEdgeTicks=120`, so **the cap is binding regularly**.

The reason is a sampling effect I did not think about: volatility is measured at
the moments we requote, and we requote *when the market moves*. So the edge is
priced off conditional volatility, not the unconditional median I calibrated
against. The quoter is therefore acting as liquidity of last resort — it trades
rarely (32 fills in 90s) and only into large sweeps.

That is profitable here, and it is a defensible strategy in a jumpy market, but it
is not what I intended to build and the parameters were chosen against the wrong
number. `MaxEdgeTicks` and `EdgeVolMult` should be re-swept now that realised
quoted spread can be measured directly, rather than inferred.

### Re-sweeping the edge cap — and a methodology correction I have to own

Added realised quoted spread and quoter fill count to `tools/bench.sh`, so the
edge is now *measured* rather than inferred from the multiplier.

The sweep, all 120s, quoter PnL:

| config | runs |
|---|---|
| cap 20 (absolute) | +2,071, +1,213, **-4,736** |
| cap 40 | -5,256 |
| cap 80 | +255 |
| cap 120 | +880, -4,434 |
| fixed edge 20 (no adaptivity) | -2,530, -3,142 |
| cap as 3.3% of price | -2,834, -3,404 |

**What I concluded after two runs each, and why it was wrong.** With n=2 the pairs
looked cleanly separated — cap 20 positive twice, fixed-20 negative twice, the
fractional cap negative twice — and I wrote up "the adaptivity earns its place" and
"follow the measurement, not the aesthetic" as though they were established. The
third run of cap 20 came back at -4,736, i.e. the within-config spread is ~6,800,
which is **as large as every between-config difference I had been interpreting**.
Two runs landing on the same side of zero is roughly a coin flip landing twice; I
treated it as a signal because it agreed with a mechanism I found plausible.

So the following are *not* established, and I am withdrawing them:

- that adaptive-capped-at-20 beats a fixed edge of 20;
- that the fractional cap is worse than the absolute one (the mechanism I gave —
  this market's moves being absolute rather than proportional — is still sound
  reasoning, but the measurement does not support the conclusion);
- any ranking among the wide-edge configurations.

**What survives.** The large effect is real and repeated across many runs: an edge
sized to volatility beats the shipped fixed edge of 2 by roughly an order of
magnitude (-8,405 at edge 2 against a scatter around zero for every wide setting).
Risk control is also unaffected by all of this — max abs desk stayed <= 12 and mean
<= 2 in *every* run of the sweep, which is the one thing measured consistently
enough to state plainly.

**Shipping `QUOTER_MAX_EDGE=20`**, not because it is proven best but because it
sits inside the plausible band and bounds the pathological case: uncapped, the
realised quoted spread reached 240 against a market spread of 1-5, which is a
quoter that has effectively left the market.

**What it would take to do this properly**: the effect sizes worth chasing here are
a few thousand against per-run noise of ~7,000, so distinguishing them needs either
much longer runs (10+ minutes) or ~10 repeats per setting, and ideally isolating
the quoter from the taker, which contributes most of the variance. That is hours of
wall time. I would rather ship a defensible default and record the uncertainty
honestly than present a tuned-looking number that a fourth run would overturn.

### Inventory skew: fixed the parameter, and it changed nothing

`QUOTER_SKEW` was an absolute 4 ticks, chosen when the edge was a fixed 2 — a 200%
lean at full inventory. Once the edge became volatility-sized and capped at 20, the
same 4 was a 20% lean, so the parameter silently stopped meaning what it said.
Replaced with `QUOTER_SKEW_FRAC`, a fraction of the *current* edge, default 1.0:
at full inventory the flattening side is pulled a whole edge toward fair and then
held off it by `MinEdgeTicks`. Unit-tested as a property (the lean scales 10x when
the edge scales 10x), so it does not depend on resolving PnL noise.

**Then I measured whether it actually controls inventory, and it does not.** Added
the quoter's own position distribution to the harness — a distribution over the
run, far less noisy than a path-dependent PnL sum:

| SkewFrac | mean abs quoter position | max |
|---|---|---|
| 1.0 (a full-edge lean) | 8.67 | 25 |
| 0.2 (roughly the old inert lean) | 8.85 | 25 |

Indistinguishable, and both run to the position limit.

**Why, and it should have been obvious earlier:** skew works by making one side
more attractive than the other to a counterparty who is *choosing* between them.
The dominant flow here is the mover sweeping the book — it takes everything in its
path up to a target price, and does not care that our bid is more attractive than
our ask. You cannot lean your way out of being run over. Inventory here is imposed,
not chosen.

That is the real reason the hedger has to exist and has to be prompt, and it
retires the theory I had been carrying since the first quoter: that the quoter
should flatten itself cheaply via skew and the hedger should only catch the tail.
In a sweep-driven market the quoter *cannot* flatten itself, whatever the lean.

**Keeping the change** — the parameter now means what it says at any edge width,
which is a correctness fix, and skew does work in a market with two-sided
price-sensitive flow. But it buys nothing measurable here and I am not claiming it
does.

### Tooling written along the way

- `tools/watch.sh` — readable live view of one contract: top-of-book changes only
  (the raw feed republishes constantly) plus trades. One `nats sub`, not two: two
  subscribers writing into the same pipe interleave mid-line and corrupt output.
- `tools/export_transcript.py` — renders `TRANSCRIPT.txt` from Claude Code's own
  session logs in `~/.claude/projects/`, concatenating multiple sessions in
  timestamp order per TASK.md. Re-run at any point; it is a raw export, not a
  write-up composed afterwards, so the wrong turns stay in.
  Note: the model's internal reasoning is *not* recoverable — those blocks are
  persisted with an empty body and only a signature. Prompts, replies, every tool
  call and every tool result are all there.

### Environment note

Developing on Apple Silicon (arm64); the exchange image is `linux/amd64` and runs
emulated, so local latency numbers are not representative. Grading runs amd64
natively. Anything I conclude about *timing* here needs re-checking before I trust it.


###

commands

docker compose up -d              # start exchange in background, get prompt back
docker compose ps                 # what's running
docker compose logs exchange      # what it's saying
nats sub 'ex.bbo.AAH6'            # watch top-of-book (Ctrl-C to stop)
nats req 'ex.req.PROBE001' 'PROBE001 A AAH6 ORD00001 B 10 595 L'

UPDATE TRANSCRIPT
python3 tools/export_transcript.py

docker run --rm -v $PWD/strategy:/w -w /w golang:1.23-alpine gofmt -l
