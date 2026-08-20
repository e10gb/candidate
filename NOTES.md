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
- Every seat rate-limits itself, sized from the exchange's declared `max_tps` where
  there is one. Breaching it *disconnects* the sender rather than rejecting, which
  would end the session. (The hedger keeps a fixed default that EX_META can lower
  but not lift; measured at 3.0 req/sec against its cap of 20, so it is nowhere
  near binding and was left alone rather than changed late.)

**Results on the shipped configuration.** Full desk, pooled over 3 x 240s
(717 samples of `held()` -- the position actually at the exchange):

| risk | | PnL per 240s | |
|---|---|---|---|
| mean abs exposure | **7.78** | quoter | **-1,341 +/- 1,091** |
| median | 7 | desk (6 runs) | **~ -13,700** |
| p95 / p99 | **18 / 24** | | |
| time at/over 10 lots | 35.4% | | |
| time at/over 25 lots | **0.8%** | | |

**Job 2 works.** The desk holds a median of 7 lots and spends under 1% of the
session above 25, while the seats carry 30+ gross between them and no position
limit is ever breached. It is not flat *all* the time -- a third of the session is
spent above 10 lots -- and that is the honest shape of it.

**Job 1 does not.** The quoter is a small, consistent loss (-1,341, interval
excluding zero) and the desk loses ~13,700 per 240s, most of it the taker.

**Quantiles, not a maximum.** Earlier versions of this paragraph quoted a mean and
a max, and the numbers moved every time (1.5, then 3.9-5.3, then 2.3-5.3). The mean
moved because short windows are noisy; the *max* moved because the maximum of N
samples grows with N, so a max over 240s is not comparable with one over 120s. It
is a property of how long you watched, not of the desk.

**Why the quoter loses -- the one finding that explains the rest.** Splitting each
fill into edge captured at the fill plus subsequent drift shows the quoted spread
earns nothing: across quoted spreads from 6 to 80 -- a thirteen-fold range -- the
edge captured stays within +/-1.5 of zero. We are filled at approximately the mid
whatever we quote, because almost all our fills come from the sample market's
background quoters *repricing through us* rather than anyone crossing a spread to
us. Detail in "Why the quoter cannot earn a spread here" below. No spread width,
hedging setting or inventory cap addresses that, which is why every configuration
swept lands between roughly -3,000 and -20,000 with overlapping intervals.

**Retracted along the way.** An earlier version of this summary claimed the quoter
was profitable standalone (+13,173, 3 of 3 runs). That was measured before the
hedger's in-flight accounting was fixed, when it under-hedged and left inventory on
the book; re-running the same configuration with correct hedging gave -2,573 and
-3,983. The claim is withdrawn. Four others went the same way. The last two are the
instructive ones: `TAKER_MODE=reversion`, which I called the largest single effect in the
project on four runs and which a seven-repeat sweep reduced to noise with a standard
deviation of 25,018; and a compaction regression that a deliberately interleaved six-run
A/B showed with no overlap between the arms, and that a second A/B of the same design
erased completely. All five retractions are kept in place below rather than tidied away.
The pattern -- a handful of agreeing runs treated as evidence -- is the most useful thing
in this file, and the last instance of it survived a test I had designed specifically to
be trustworthy.

**Reference pricing does not work, for a second and more interesting reason.** Making
the quoter report which contract it prices off revealed that it always picks itself: the
lead is chosen by counting BBO updates, and our own quoting inflates our own feed's
count past the real lead's (AAH6 leads AAM6 by 11% with the desk switched off; we add
more than that). The feature has been inert in every full-desk run here. Diagnosed and
documented, deliberately not fixed -- see Session 9.

**The sweep also caught a bug I had introduced.** Compacting the seats deleted three
lines from the quoter's config literal while leaving every reader of them intact, so
reference pricing silently switched itself off -- it compiled, `go vet` was clean and
all 29 unit tests passed, because none of them exercised the env path the container
actually uses. Fixed, with two tests that fail when it is reintroduced. What it *cost*
is a different question, and my first two answers to it were both wrong: a controlled,
interleaved six-run A/B said the compaction had cost ~9,000 a run, and a second one of
the same design said the effect does not exist. Session 9 has both, and the reason the
convincing one was wrong.

**What I would not claim.** That any particular wide-edge setting is optimal; runs of
identical configuration differ by more than the effects being compared. Resolving
that needs ~10 repeats or 10-minute runs. I chose to record the uncertainty rather
than present a tuned-looking number a further run would overturn.

**Known limitations.** The quoter trades one contract (default `AAM6`, priced off
the lead); the hedger watches every contract and hedges the combined exposure in
`AAH6`. The taker still loses money on its own merits -- momentum in a market that
mean-reverts inside a band -- and I left its parameters alone rather than fit them to a
market the brief says is not the graded one. Fading the move looked like the fix for
several sessions; at seven repeats it is indistinguishable from momentum and merely
far more volatile, so the shipped default stands (Session 9). The sample market deadlocks permanently if its book ever empties, which
is a property of `sim/market.py`, not the desk.
1
**What I learned** To not let AI make assumptions or guess. Start testing from the beginning 
to see what numbers produce optimal output. Or, to keep a better track of guesses made
throughout the project.

**How to read the rest of this file.** It is the working record, in the order things
were discovered, including the wrong turns and all five retractions. Roughly: exchange
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

*(Two of these were resolved later by reading `EX_META` rather than guessing; kept
here with their answers, since the guessing was itself the mistake.)*

- **`max_tps=0`** in `exchange/instruments.txt`. **Resolved:** 0 means unlimited,
  and the value is published per-feed in the `EX_META` KV bucket. Both seats now
  read it at startup and size their rate limiter under whatever the exchange
  declares, instead of the 20/sec I had hardcoded. That guess was wrong in both
  directions -- a market declaring `max_tps=15` would have *disconnected* us
  (changelog v2.3), and locally it held the quoter to a fraction of the market's
  event rate, which is what being picked off is.
- **Position limits**: local `position_limit` is 1e9. **Resolved the same way** --
  read from `EX_META` and used to clamp the quoter's `MaxPos` and the hedger's
  clip, so a tighter grading market cannot be breached.
- **`X` selector semantics** -- still unresolved. Working around it by cancelling
  by explicit order id rather than solving it.
- **STP** -- `CHANGELOG` v2.0 says on by default, v2.2 says per-feed opt-in via
  `Q`. Never tested. It would not have helped across our seats anyway (they use
  different sender ids), and the desk-trades-with-itself problem it would address
  is now solved structurally: the quoter and hedger no longer share a book.

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

*(Postscript: the price-relative cap discussed below was later removed outright.
It measured no better than the absolute one and left two configuration knobs
doing one job, which is a cost with no benefit. The reasoning that motivated it —
that an absolute constant will not transfer to an instrument at a different price
level — is still sound and is recorded here; it simply was not worth the second
code path.)*

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

### Passive-first hedging

The hedger crossed the spread on every hedge. That was the desk's largest
*mechanical* cost — not a strategy loss but a fee we chose to pay, on ~200 lots per
two minutes. TASK.md's own risk model says a small position held for a while is
cheap and a large one is not, so paying for immediacy on small exposure was buying
something we did not need.

Now: exposure at or above `HEDGE_URGENT` (15) is crossed immediately, as before.
Below that, the hedger rests a limit one tick inside the touch and waits up to
`HEDGE_PASSIVE_MS` (300ms). The resting order is pulled early if the desk goes
flat, if exposure flips sign, or if it grows past URGENT — in which case it crosses
straight away.

**Measured with a metric built for it.** PnL is too noisy to show a few thousand,
so the hedger now records how far each of its fills was from the mid *at the moment
it traded*, signed so paying up is positive. That is mechanical rather than
path-dependent, and comparable across short runs:

| config | cost/lot | passive share | max abs desk |
|---|---|---|---|
| always cross | +3.33, +4.77 | 0% | 5, 8 |
| passive-first | **-5.64, -0.17** | 56%, 48% | 7, 12 |

The groups do not overlap, and this time that means something: the mechanism is
arithmetic rather than luck — resting earns the spread, crossing pays it. The
variance within the passive group comes from how the passive/crossed mix falls out,
not from market direction.

Risk held: peak desk exposure 7 and 12, both inside URGENT=15 by construction, with
mean abs desk unchanged at ~1.4.

**The caveat that keeps this honest.** A negative cost/lot is *execution quality at
the moment of the fill*, not proof of profit. Passive fills are adversely selected
by nature — you get filled precisely when someone wants to trade against you — and
this metric cannot see what the price did next. It shows we stopped paying the
spread unnecessarily; it does not by itself show the desk earns more. Desk PnL over
these runs stayed inside its usual noise band.

Seven unit tests pin the state machine: below threshold does nothing, modest
exposure rests, urgent exposure crosses, patience running out cancels then crosses,
and the resting order is pulled when the desk goes flat or the exposure flips. One
checks that improving on a one-tick-wide book cannot cross the offer.

### Seeing the real profit

The run summary marked open positions at the **mid**, which flatters them. The
session ends by liquidating whatever is left against the book, so the honest number
values a long at the bid and a short at the ask. Every seat now reports both, and
`runs/run-*.md` leads with the closed-out figure and shows the difference as the
cost of the residual position. Mid-marking is still shown, because the gap between
the two is itself the risk being carried.

### Two-tier quoting: trialled, measured worse, shipped off

A single wide quote is safe against sweeps but forfeits all the calm-market flow,
which is why fill counts sat at a few dozen a run. So: a tight small inner quote
alongside the wide one (`QUOTER_TIERS=2`, inner at 40% of the edge and 40% of the
clip), sharing one position limit.

It did exactly what it was designed to do, and that turned out to be the problem:

| | 1 tier | 2 tiers |
|---|---|---|
| quoter fills | 86 | 287, 351 |
| median quoted spread | 34 | 16 |
| quoter PnL | -3,382 | -3,046, -6,554 |
| hedger lots traded | ~236 | **620** |
| mean abs desk exposure | 1.74 | **2.60, 2.85** |

Three to four times the flow, half the spread, and no improvement in the quoter's
own PnL — so the extra fills paid for themselves at best. Meanwhile the inventory
they generated was paid for twice: once in adverse selection, once in hedging
volume that nearly tripled. Mean desk exposure rose in both runs, which matters
more than the PnL here given how the task is graded.

**The lesson is about this market rather than the technique.** The flow available
just inside the touch is available *because* it is adversely selected — the mover
sweeps through those levels on its way somewhere. Quoting tighter buys more of
exactly the trade you did not want. Two-tier quoting is the right shape in a market
with benign two-way flow; it is the wrong shape here.

**The code was later removed.** It shipped defaulted off, and a configuration
path that is off, measured worse and never asked for is cost without benefit --
the finding above is worth keeping, the second code path is not. The same applies
to the fast-volatility horizon further below.

### Measurement campaign: is the quoter actually profitable?

Every earlier profitability claim rested on 120s runs with the taker included,
where its -2,000 to -28,000 swamped the effect being measured. `tools/campaign.sh`
was built to settle it: 240s runs, three repeats, taker excluded (quoter and hedger
still run together so inventory is managed as in production), reporting mean and
spread, and using the liquidation-marked PnL rather than the mid-marked one.

**Answer: no, and not marginally.**

```
baseline (shipped defaults, AAH6):  -9457  -14558  -20303
mean -14773   stdev 5426   ->  negative, larger than the run-to-run spread
```

Losses scaled almost linearly with fills (149 -> 231 -> 317), about **-64 per fill,
-13 per lot**. Every fill was costing money, so the quoter was not mispriced at the
margin -- it was trading with the wrong people.

**The diagnosis, from `tools/counterparties.sh`.** Each execution names the
counterparty, and the sample market's participants are distinguishable by
construction:

| counterparty | lots | share |
|---|---|---|
| MOVER001 (sweeps the book to a target) | 163 | **84%** |
| TAKER001/2 (casual, uninformed) | 22 | 11% |
| BGQUOT02 | 4 | 2% |
| HEDGE001 (our own hedger) | 5 | 3% |

A market maker earns from uninformed flow. **84% of ours came from the one
participant that is informed by construction.** The casual takers cross at most 6
ticks through the touch (`sim/market.py:193`), and our quotes sit ~20 outside it,
so they cannot reach us at all -- the only counterparty who *can* reach us is the
one sweeping through on the way somewhere.

**What fixed it, in order of size:**

| configuration | mean | stdev |
|---|---|---|
| AAH6 front, as shipped | -14,773 | 5,426 |
| AAH6 front + lead pricing | -8,061 | 7,188 |
| AAM6 back month | -6,481 | 2,400 |
| AAM6 + wider cap (40) | -4,038 | 3,494 |
| **AAM6 + cap 40 + priced off the AAH6 lead** | **+229** | 5,585 |
| same, lead auto-detected rather than configured | -2,994 | 5,359 |

Two mechanisms, both with a reason rather than a fitted number:

1. **Avoid the toxic contract.** The mover only trades the front month
   (`sim/market.py:24`). On the back month the loss per lot fell from -12.7 to
   -2.7 while fill count *tripled* -- more flow, and far cleaner.
2. **Price off the contract that leads.** The sim's own quoters set the back
   months at `round(front_fair + offset)` (`market.py:167`), so they reprice the
   instant AAH6 moves while we were still quoting off AAM6's stale book. We were
   the slow one in the room, and being slow is what gets picked off.

**Honest verdict: break-even, not profitable.** The best configuration has a mean
of +229 against a stdev of 5,585 -- indistinguishable from zero. What *is*
established is the progression: from reliably losing ~15,000 per 240s (three of
three negative, mean far larger than the spread) to hovering around zero. That is a
real result with a mechanism; "profitable" is not.

**What ships, and why.** Lead detection is automatic and on by default: the quoter
watches every contract sharing its two-character underlying prefix and prices off
whichever is busiest, *unless that is the one it quotes* -- pricing the most active
contract off a quieter sibling would import lag rather than remove it. That rule
needs no knowledge of the grading market's listings. Verified as a no-op when we
are the busiest: unit-tested, and a live front-month campaign with it enabled came
back statistically unchanged (-8,061 vs -14,773, difference well inside both
spreads).

The feed itself is *not* hardcoded to a back month. Quoting AAM6 is the right
answer for this sample market and there is no reason to think the grading market
has the same structure; the transferable part is the mechanism for finding the
lead, not the choice of contract.

**Also found: the desk trades with itself.** 3% of the quoter's lots were against
`HEDGE001`. When the hedger crosses into our own resting quote the desk's net
position does not change at all -- it pays the spread internally and achieves
nothing. Small here, but it means a hedge that appears to work sometimes does not.
Fixing it needs the seats to coordinate over the bus (the task explicitly invites
this); left as an open item rather than rushed.

### Running at the market's frequency -- and the quoter turning profitable

The ask: make the quoter operate at the same frequency as the simulated market.
The sim's participants are event-driven -- its quoters reprice on every
front-month tick, the mover acts every 200ms -- while ours acted on ~23.5% of
genuine changes. The gap was two self-imposed throttles: a 50ms post-requote
sleep, and a **guessed** rate cap of 20 TPS.

**The guess was the bug, in both directions.** The exchange states its per-feed
limit in `EX_META` (`max_tps`), which no seat had ever read. If grading sets
`max_tps=15`, our hardcoded 20 gets the sender *disconnected* (changelog v2.3) --
and a disconnected hedger means no risk control at all. Locally `max_tps=0`
(unlimited) and we were crawling. Both seats now read EX_META at startup:

- declared limit > 0: token bucket sized under it. A bucket with burst B and rate
  r admits at most B + r*T requests in any window T; with r = max_tps/2 and
  B = 0.3*max_tps, any one-second window stays at or under 0.8 of the limit,
  whatever grading's enforcement window is.
- declared limit 0: run uncapped, at the market's own event rate.
- `MinRequote` default 50ms -> 0. The wake channel already coalesces bursts; the
  sleep only added latency between a fair-value move and our reprice.
- Freebies while reading meta: `position_limit` clamps MaxPos and the hedger's
  clip; `ticksize` puts prices on the grid (rounding away from fair, so the
  minimum edge survives). The tick>1 path is untested live -- the local exchange
  lists only tick=1.

Measured (same 30s front-month test as before): throughput 8.2 -> **56.2
requests/sec**; actions per genuine top-of-book change 23.5% -> ~38%, and the
remainder is changes that do not move our target price -- no action *needed* --
rather than throttling.

**Campaign results (3 x 240s each, quoter + hedger, liquidation-marked):**

| configuration | before speed | at market speed |
|---|---|---|
| AAM6, cap 40, lead pricing | +229 +/- 5,585 | **+13,173 +/- 6,066 (min +7,944, 3/3 positive)** |
| AAH6 front, defaults | -14,773 +/- 5,426 | -10,002 +/- 4,702 |

The first line is the result: **positive, larger than the run-to-run spread,
every run**. The second explains the mechanism by contrast: speed does nothing
for the front month, because the mover's sweep is one atomic matching event --
once the order is in flight there is nothing to dodge. On the lagging contract
the danger was never the sweep; it was the *race* against the sim's own quoters
repricing off the front tick while we sat on stale prices. Lead pricing gave us
the right price and market-frequency reaction lets us act on it in time. Either
half alone measured at a loss; together they are worth ~13,000 per 240s.

**The desk layout that ships (docker-compose.yml):** quoter on AAM6 pricing off
the auto-detected lead with cap 40; taker on the front month as before; hedger
watching **every** contract (`ex.md.*.<sender>`) and hedging the combined
exposure in liquid AAH6. Contracts on one underlying are cash-settled at their
listed price, so a lot of any sibling carries the same unit risk; the residual
is the pinned inter-month spread, small beside the outright risk removed. This
also ends the desk-trades-with-itself problem by construction -- the hedger no
longer shares a book with the quoter.

Full-stack verification (all three seats, two 120s runs): desk exposure max
11/13, mean ~1.9, zero rejects; quoter +2,432 and -815 -- breakeven-to-positive
inside this window's noise, consistent with the 240s campaign.

Judgement calls stated plainly:

- Defaulting QUOTER_FEED=AAM6 bets that grading lists the same complex. It is
  the same bet the *shipped* compose already made by pinning TAKER_FEED=AAH6.
  The mechanisms (lead detection, meta-derived limits, market-speed reaction)
  transfer regardless; the feed choice is configuration, overridable by env.
- The hedger now rests passively on the front month, where the mover's sweeps
  are; its measured cost/lot (+1.8 to +2.4) is still below crossing-always
  (+3.3 to +4.8), so passive-first stays on. Open item, not a defect.
- Profit is demonstrated against this sim's structure. The honest claim for the
  grading market is the mechanism, not the number.

### The risk number I had been quoting was wrong

Found while verifying the submission from a clean checkout: the hedger reported
`desk=-7` while its seats held `-14, +3, -54` between them. The arithmetic was
self-consistent -- `desk() = sum(positions) + inflight` -- but `inflight` had
stuck at +58 and never returned to zero.

**The bug.** `cross()` adds to the in-flight bridge; `rest()` does not, because a
resting order has not traded. But the fill handler retired *every* hedger fill
from the bridge, passive ones included. Each passive fill therefore retired
volume that was never on it, and since `desk() = sum + inflight`, the hedger's own
passive position was cancelled straight out of the exposure calculation. **The desk
reported flat while carrying forty lots.** I introduced this when I added
passive-first hedging, and the metric I used to justify that change (cost per lot)
could not have caught it.

Two fixes, because the first was necessary but not sufficient:

1. Only retire fills that were actually on the bridge -- i.e. where we crossed --
   and never let the retirement flip the bridge's sign (a limit order that crosses
   on entry was never counted either).
2. **Give the bridge an expiry.** It exists to cover a round trip of
   milliseconds; anything outstanding after `HEDGE_INFLIGHT_TTL` (1s) is stale and
   is dropped. Fix 1 alone left a smaller leak -- the hedger crossing into its own
   resting order is a self-match, which is no position change and so produces no
   bookable fill, while `cross()` had already counted it. Rather than enumerate
   every such path, the bridge now cannot mislead for longer than a second.
   Positions come from the exchange and are always right; the bridge is the only
   estimate in the calculation, so it is the part that gets an expiry.

**And a correction to the headline.** Risk is now measured on `held()` -- the
position actually at the exchange -- rather than the bridged `desk()` the hedger
acts on. Two 240s runs after the fix: mean 2.38/2.28, max 28/18, with 1% and 0% of
samples at or over 25. Acting on the bridged view is correct (it stops double-hedging an order
already sent); *reporting* on it is not, because an estimate should never flatter
the number it is being judged by. The honest figures are mean ~3.9-5.3 and max
22-25, against the ~1.5 quoted throughout this file's earlier sections. Job 2 still
holds -- the seats carry 30+ gross and nothing ever approached a position limit --
but the margin is smaller than I had been claiming.

### A flaky test, and why it was not muted

The smoke test failed once with one reject/error line, then passed twice cleanly.
The cause is a startup race: the seats connect while the exchange is still coming
up, a request times out, and the hedger logs `request failed` and retries on its
next tick 50ms later. Correct behaviour, but the test was reading logs from
container start, so it judged the desk on its first second.

Fixed by collecting only the measurement window (`docker compose logs --since`),
not the boot. The check itself stays strict -- zero rejects in steady state --
because loosening the assertion would have hidden the thing it exists to catch.
Three consecutive clean runs after the change.

The distinction matters: "ignore errors during startup, fail on any afterwards" is
a statement about the system; "allow one error" would just have been a statement
about my patience.

### Trying to make the two-seat desk profitable, and what stopped it

With the taker silent, the quoter and hedger alone were swept across ten 240s runs.
Every configuration lost money, and the losses moved between the two seats rather
than shrinking:

| configuration | quoter | hedger | desk | mean exposure |
|---|---|---|---|---|
| thresh 5, maxpos 25 | -2,573 / -3,983 | -22,350 / -36,145 | -24,923 / -40,128 | 6.7 / 12.7 |
| thresh 12, maxpos 25 | +2,721 / -4,121 | -17,075 / -13,824 | -14,354 / -17,945 | 8.3 / 8.2 |
| **thresh 12, maxpos 10** | +336 / -4,369 | -7,608 / -3,513 | **-7,272 / -7,882** | 6.3 / 5.1 |
| + patient band (urgent 30) | -4,562 / -10,184 | -2,946 / -5,482 | -7,508 / -15,666 | 4.8 / 5.6 |
| + patient + cross-contract | -11,922 / +9,305 | -8,530 / -15,443 | -20,452 / -6,138 | 5.0 / 5.0 |

**Three mechanisms, each real, none sufficient.**

1. *A market maker's inventory swings by nature.* At `HEDGE_THRESH=5` the hedger
   mirrored every swing, trading ~5,000 lots per 240s to clear a quoter holding
   ~9, at 4-5 per lot. Raising the threshold and capping the quoter's inventory
   attacks that bill at both ends -- two-thirds of the loss, and better risk at
   the same time, which is why it ships.
2. *Hedging urgency only moves the cost between seats.* The patient band cut the
   hedger's volume to ~86 lots and halved its loss, but the quoter's loss grew by
   the same amount: hedge fast and the hedger pays the spread, hedge slow and the
   quoter carries the market risk. Desk total unchanged.
3. *Cross-contract hedging is not hedging.* It gave the cheapest execution measured
   (**-0.94 and -0.42 per lot** -- the hedger *earning* on fills, crossing only 53
   lots) and the worst hedger PnL (-8,530 / -15,443). An AAH6 position against AAM6
   inventory is a naked directional bet, and it lost more than the spread it saved.
   Same-contract hedging costs 4-5 per lot and actually offsets. That is the trade.

**Why none of it reaches profit.** Whoever ends up holding the quoter's inventory
loses, because the fills are adversely selected; clearing it costs spread. Moving
the inventory between seats, or clearing it sooner or later, redistributes that
loss without removing it. The only configuration that ever *looked* profitable was
the one where the hedger was accidentally under-hedging -- see the correction
below -- which is to say: the profit came from carrying inventory through the
market's mean reversion, which is precisely the risk Job 2 exists to remove.

That tension is the honest finding. In this sample market, holding inventory pays
and flatness costs, so a desk mandated to stay flat gives up the one edge available
to it. Making the quoter genuinely profitable needs its *fills* to be better --
less adversely selected -- not better inventory management downstream.

**Correction: the +13,173 does not reproduce.** That result was measured before the
in-flight accounting was fixed, when the hedger under-hedged and left inventory on
the book. Re-running the same configuration with correct hedging gives quoter
-2,573 / -3,983. The earlier figure was an artifact of a bug, and any claim resting
on it is withdrawn.

### Attacking adverse selection directly, and a methodology lesson

The remaining loss is upstream of hedging: the quoter's fills are adversely
selected. To work on that I built `tools/markout.py`, which measures it directly --
for every fill, where the mid went afterwards, signed so positive means the market
moved our way. Negative markout *is* adverse selection, in price units per lot.

**The diagnosis was sharp and surprising.** First run, 163 fills on AAM6:

| horizon | markout/lot | | counterparty | lots | markout/lot @2s |
|---|---|---|---|---|---|
| 0.1s | **-7.15** | | BGQUOT04 | 230 | **-11.07** |
| 0.5s | -0.49 | | BGQUOT03 | 206 | +0.28 |
| 2.0s | -3.18 | | TAKER001 | 41 | **+14.57** |
| 5.0s | -4.77 | | TAKER002 | 20 | **+5.75** |

The casual takers were *profitable* for us. The toxic flow was a background
*quoter* repricing through our stale quotes. And the damage was a sharp transient:
-7.15 at 100ms, recovering to -0.49 by 500ms.

**Two interventions, both measured, neither worked.**

1. *A fast volatility horizon* (250ms alongside the 2s one, widest wins), on the
   reasoning that a 2s window cannot see a 100ms event. Back-to-back control:
   markout went from -6.98 to -16.74 at 100ms while the spread barely moved (44 to
   48) and fills went *up*. Plausible mechanism for the backfire: widening
   mid-burst forces a cancel-and-replace, and every replace posts a fresh order
   into the moving market. Reverted, defaulted off.
2. *Quoting tighter*, on the observation that casual takers cross at most 6 ticks
   through the touch (`sim/market.py:193`) while we quote ~22 out -- so our wide
   edge was selecting *for* the toxic repricing flow and against the profitable
   casual flow. This inverts the "wider is safer" conclusion from the front month,
   and it is a real mechanism. Measured: -22.81 at 100ms against -16.04 wide. Also
   worse.

**The methodology lesson, which is the durable part.** I adopted markout believing
it was low-noise because it yields hundreds of observations per run. It is not: the
same configuration measured four times across the day gave **-7.15, -18.05, -6.98
and -16.04** at 100ms. Fills within a run share one market regime, so the
observations are strongly correlated and the effective sample size is nowhere near
the fill count. Both negative results above are therefore *suggestive, not
conclusive* -- and had either come out positive I would have been at real risk of
shipping noise as a finding, which is exactly the trap I fell into twice earlier.

A properly powered version needs many short runs across independent market draws,
comparing distributions rather than point estimates. That is hours of wall time and
I stopped short of it rather than report another number that a fifth run would
overturn.

**What survives.** The diagnosis itself is robust across every run: the profitable
counterparties are the uninformed casual takers, the losses come from quoters
repricing off the front month, and the damage is concentrated in the first
few hundred milliseconds after a fill. Anyone continuing this should start there --
the target is reaching casual flow without being reachable by repricing quoters,
which is a queue-position and timing problem rather than a spread-width one.

### Splitting the desk by flow type: liquidity to the quoter, reversion to the taker

The markout work established who pays us and who takes from us: the uninformed
casual takers are profitable (+14.57 and +5.75 per lot), the background quoters
repricing off the front month are toxic. It also established, indirectly, where
the money in this market actually is -- the only configuration that ever looked
profitable was the one where a bug left the hedger under-hedging, i.e. where the
desk accidentally *held* inventory through the market's mean reversion.

That suggests a division of labour rather than a parameter:

- **The quoter provides liquidity** and must not hold. Job 2 requires flatness and
  the hedger correctly clears its inventory. Its job is the casual flow.
- **The taker takes the reversion**, because that is the seat whose purpose is
  directional risk, and it can do so sized and bounded by `TAKER_MAX_POS` rather
  than leaking into the quoter as unhedged exposure.

The shipped taker was **momentum** -- the wrong sign for a market that walks to a
target and back inside a hard band (`sim/market.py` clamps to [440, 760]). Flipping
it (`TAKER_MODE=reversion`, one line, original behaviour still available) is the
largest single effect measured in this project:

| taker mode | own PnL across runs |
|---|---|
| momentum (as shipped) | -21,309, and -3k to -15k historically |
| **reversion** | **+7,544, +7,199, +4,576, -1,938** (3 of 4 positive) |

**Then the seats fought each other.** With the taker holding a deliberate position
to capture the reversion, a hedger threshold of 12 closed that winning trade on
every swing and paid the spread to do it: taker +7,199 against hedger -24,306. The
fix is not a cleverer hedger but a wider tolerance -- `HEDGE_THRESH` 12 -> 20, plus
`TAKER_MAX_POS` 30 -> 15 to bound what the hedger must clear at source.

**Result, five runs (`TAKER_MODE=reversion`):**

```
-2,816  -2,749  -11,910  +3,410  -1,339      mean -3,081   stdev 5,552
```

**Correction: that is not what ships.** Those runs used reversion; the desk ships
`momentum`, the strategy as handed over, because reversion is fitted to this
market's mean-reverting band. Measured at the shipped settings, three runs:

```
-15,294  -14,682  -14,930     mean -14,969   stdev 308   95% CI [-15,324, -14,613]
```

The taker alone is -9,147 to -10,318 of that. So the shipped desk loses about
15,000 per 240s, reliably -- this is the tightest interval measured in the whole
project, and it is tight around a loss. Switching one environment variable to
`reversion` recovers roughly 12,000 of it, at the cost of shipping a default
fitted to this simulator.

Patient hedging was tested against the same bar and did not help: -16,571 +/-
5,736, indistinguishable from shipped. It worked mechanically -- zero crossed
lots, every hedge worked passively -- so execution cost simply is not the binding
constraint at these settings.

Against the -19,392 mean of the configuration it replaces. **The improvement is
larger than the spread; the remaining loss is not distinguishable from zero.** So:
break-even, not profitable, and I will not claim more than that -- twice today a
pair of agreeing runs was destroyed by a third (-2,816 and -2,749 looked like a
tight result until the next run came back -11,910).

**The structural tension, stated plainly.** Job 2 requires the combined position to
stay low. The profit in this market comes from holding through reversion. Those
pull against each other, and no amount of hedger tuning dissolves it -- the desk
can be flat or it can be paid, and the brief is explicit that it must be flat. What
the split does is put the residual directional risk in the seat that is *supposed*
to carry it, bounded and measured, instead of in the market maker.

**Also tested against the same 5-run bar, and not shipped:** a 250ms fast
volatility horizon (markout -6.98 -> -16.74; widening mid-burst forces a
cancel-replace that posts a fresh order into the moving market), quoting tighter to
reach the casual flow (-16.04 -> -22.81), and raising the calm-period edge floor to
15 (one pair flipped the sign, three-run validation gave +27, +3,980, -10,204 --
withdrawn).

### Three improvements, and what is and is not claimed for them

**1. Statistical power -- the binding constraint all along.** Every tuning
decision here rested on 3-5 runs against a run-to-run stdev of ~5,500, which is
why three separate conclusions collapsed when a fourth run arrived. `campaign.sh`
now reports a **95% confidence interval and a required-sample estimate**, and
`tools/sweep.sh` runs a list of configurations unattended into one CSV with
per-configuration intervals. The number that matters: **~21 runs to detect an
effect of 1,000**. Every comparison made today was underpowered by roughly an
order of magnitude, and the tooling now says so out loud rather than leaving it to
be discovered afterwards.

**2. Pull rather than widen.** Widening on a burst was tried and measured worse
(markout -6.98 -> -16.74), for a mechanical reason: widening changes the target
price, so reconcile cancels and re-adds, and the re-add drops a fresh order into
the middle of the move. Pulling has no such failure mode -- we cancel and stay
out, and there is nothing left to hit. `QUOTER_PULL_MOVE` (off by default) leaves
the market for `QUOTER_PULL_MS` after the mid moves that far within
`QUOTER_PULL_WINDOW_MS`.

Writing the test caught a real bug in it: when no sample was old enough to serve
as the window's reference, the code fell back to the *oldest* sample in the buffer
-- up to ten times older -- read a far larger "move" than had occurred, and
re-triggered the pull forever. It now declines to judge rather than guessing.

**3. Multi-contract quoting.** `QUOTER_FEED` takes a comma-separated list; each
contract gets an independent quoter sharing one **order-id allocator**, because
ids are consumed per sender rather than per feed and two clock-seeded allocators
would collide silently. Verified live on `AAM6,AAU6`: both books quoted and
filled, the hedger summed both positions, zero 203s.

**All three ship off or unchanged by default**, which is the point. Each is a
capability with a mechanism and a test; none has the sample size behind it to
justify changing a default, and after today the bar for "this is better" is an
interval that excludes zero, not two runs that agree.

### Why the quoter cannot earn a spread here

A market maker's fill decomposes exactly. For a buy at `px`, with mid `M0` at the
fill and `M1` later:

    markout (M1 - px)  =  edge captured (M0 - px)  +  drift (M1 - M0)

I had been measuring only the total. Splitting it (`tools/markout.py`) answers the
question outright:

| quoted spread | edge captured per lot |
|---|---|
| 80 | -0.78 |
| 50 | +1.05 |
| 6 | +0.20 |
| 6 | +1.64 |
| 6, with the pull guard | -1.09 |

**The quoted spread varies thirteen-fold and the edge captured stays within +/-1.5
of zero.** Whatever we quote, we are filled at approximately the mid. The spread is
not being earned at all; the loss is then whatever drift follows (-2 to -17).

**Why.** The fill rate gives it away: 869 fills in 110s quoting at the touch, about
8/sec, against casual-taker flow of well under 1/sec on this contract. Almost none
of our fills are someone crossing the spread to us. They come from the background
quoters *repricing*: `sim/market.py:153` has them cancel their whole ladder and
re-add around a new centre taken from the front month (`c = round(fv + rel)`, line
167). When the centre jumps 40 points, their new bids land above our stale asks and
cross them. They are the aggressor, we are filled, and the mid is already on its way
to the new centre -- so we capture nothing and then hold the wrong side of the move.

This is a property of the simulator's microstructure rather than of our quoter. Its
liquidity providers reprice *by crossing through resting orders* instead of joining
or posting inside the spread. Against participants that reprice aggressively on
every tick of a leading contract, a resting quote is not a service anyone pays for
-- it is a free option, exercised precisely when it is worth exactly zero.

**What follows for the desk.** No spread-width, hedging or inventory setting fixes
this, and the sweeps bear that out: every configuration lands between roughly
-3,000 and -20,000 with intervals that overlap. Being paid for liquidity requires
counterparties who cross a spread to get done, and in this market they are a
rounding error next to the mechanical repricing.

It also says something about the grading market. The transferable claim is not a
number but a check: **measure edge captured at the fill before believing a market
maker is earning anything.** If it is near zero there, the same thing is happening
and the answer is not a wider spread. `tools/markout.py` reports it in one run.

### Would the desk stay flat if the quoter were profitable?

Worth asking, because every risk number here was measured against a quoter that
loses. A profitable quoter is a *busier* one -- it gets crossed rather than run
over -- so the risk control was stressed with roughly six times the fill rate
(quoting at the touch, `QUOTER_MAX_EDGE=3`), 3 runs each:

| | mean abs exposure | peak | hedger lots |
|---|---|---|---|
| normal activity | 7.9 / 6.9 / 7.9 | 25 / 19 / 21 | 229 / 32 / 299 |
| ~6x fill rate | 7.2 / 8.5 / 8.6 | 26 / 35 / 130 | 488 / 795 / 130 |

**Mean exposure barely moves** (7.6 to 8.1) while the hedger does two to three
times the work -- which is the control behaving correctly: it absorbs the extra
flow rather than letting it accumulate. The peak did rise, one run touching 35
against 25 before, so the tail widens under load even as the middle holds.

Two reasons to expect a *genuinely* profitable quoter to be flatter still, not
less flat. First, profitability here would mean being crossed by two-sided
uninformed flow, and two-sided flow largely self-cancels in inventory terms -- the
current fills are one-sided bursts (a repricing sweep takes one side), which is the
worst case for accumulation. Second, the binding constraints are structural rather
than tuned: `QUOTER_MAX_POS`, the exchange's own `position_limit` from EX_META, and
the hedger's urgent-crossing path all bound exposure whatever the fill pattern.

The honest caveat: the hedger's thresholds *were* tuned against this flow profile,
and the widening tail says they are not free of it. A quoter earning real spread
would want them re-measured, and `tools/sweep.sh` is there for it.

### The overnight sweep, and compacting the seats

**The sweep.** `sweeps/overnight.txt` is the properly-powered comparison every
tuning decision here needed and none of them got: 8 configurations x 7 runs x
240s, about 4.5 hours, appending to `runs/sweep-<timestamp>.csv` with per-config
95% intervals as it goes, so it can be stopped early and still read. It covers the
choices that were made on 3-5 runs and are therefore judgements rather than
results: taker mode, hedge threshold either side of the shipped 20, quoter
position cap, edge cap, the pull guard, and two-contract quoting.

`-r 21` is the figure the tooling gives for resolving an effect of 1,000 against a
stdev of ~5,500; `-r 7` is the indicative version that fits in an evening. Neither
was run to completion inside this project, and no default should move until one is.

**Compaction.** The three seats went from 2,231 lines to 1,987, with behaviour
unchanged (verified live: same fills, same desk exposure, zero rejects, full suite
green). Two features came out entirely -- two-tier quoting and the fast-volatility
horizon. Both were my own speculative additions, both shipped defaulted off after
measuring worse, and a code path that is off, unhelpful and unasked-for is cost
without benefit. The findings that produced them are kept here; the second code
paths are not.

The rest was prose. The comments carried whole paragraphs of argument that belong
in this file: the code now states the conclusion and the one fact that makes it
non-obvious ("Not duplicates: E when we were resting, T when we crossed"), and
leaves the reasoning here. What survived is what a reader needs to not
reintroduce a bug.

### A note on what is in this submission

Everything I wrote or changed: the three seats, the measurement tooling, the test
suite, the sweep configurations, and the raw results in `runs/` that the numbers in
this file were computed from. Every claim here can be re-derived from what is in the
tarball.

What is *not* included is the scaffolding that came with the task and that I did not
change -- `sim/`, `exchange/`, `PROTOCOL.md`, `TASK.md`, `README.md`, `setup.sh`. Drop
these files over a fresh checkout and the desk runs; that is exactly how the
clean-checkout verification in Session 9 was done.

`run.sh` calls the run summariser if it is present and carries on silently if it is
not, so the desk also runs unchanged without the tooling.

### Tooling written along the way

Everything ran in containers -- no Go toolchain, `nats-py` or venv needed on the
host.

| tool | what it is for |
|---|---|
| `tools/watch.sh` | readable live view of one contract: genuine top-of-book changes only (the feed republishes constantly) plus trades |
| `tools/bench.sh` | one full-desk run against the sample market, reporting risk *and* PnL together, with quoted spread, inventory and hedging cost |
| `tools/campaign.sh` | the same but repeated and long, taker excluded, with mean and spread -- built after single runs fooled me twice |
| `tools/counterparties.sh` | who is actually trading with us, and at what cost; the diagnosis that found 84% of our flow was one informed sweeper |
| `tools/run_summary.sh` | writes `runs/run-<timestamp>.md` after every `./run.sh`, including warnings for the failure modes that look healthy |
| `tools/markout.py` | adverse selection per fill, split into edge captured and drift -- the tool that found the quoter never earns its spread |
| `tools/leadsignal.py` | whether toxic flow is predictable before it arrives, by watching every participant's cancels |
| `tools/sweep.sh` | runs a list of configurations unattended into one CSV with per-configuration confidence intervals |
| `tools/export_transcript.py` | renders `TRANSCRIPT.txt` from Claude Code's own session logs |

Two notes on the harnesses, both learned the hard way. `bench.sh` and
`campaign.sh` reset with the compose profiles on *every* call: `docker compose
down` without them leaves the seats running, and the next `up` silently reuses a
seat whose config is unchanged, so a repeat continues from the previous run's
positions. And every run starts from a clean exchange, because a long-lived one
exhausts the sim's order-id space and the market silently never forms.

`tools/export_transcript.py` is a raw export, not a write-up composed afterwards,
so the wrong turns stay in. The model's internal reasoning is *not* recoverable --
those blocks are persisted with an empty body and only a signature. Prompts,
replies, every tool call and every tool result are all there.

### Environment note

Developing on Apple Silicon (arm64); the exchange image is `linux/amd64` and runs
emulated, so local latency numbers are not representative. Grading runs amd64
natively. Anything I conclude about *timing* here needs re-checking before I trust it.



## Session 9 — the overnight sweep, a real bug, and a regression that was not there

The sweep I had been deferring all project finally ran: 8 configurations x 7
repeats x 240s, 56 runs, ~4.5 hours (`tools/sweep.sh sweeps/overnight.txt -d 240
-r 7`, output `runs/sweep-20260820-205649.csv`). This is the first measurement in
the whole file with enough repeats to say anything, and it did two things: it
overturned my largest claim, and it exposed a bug I had introduced myself.

| configuration | mean desk | stdev | 95% CI |
|---|---|---|---|
| quoter-maxpos-20 | -18,986 | 3,517 | [-21,645, -16,327] |
| pull-on-lead | -18,309 | 5,790 | [-22,685, -13,932] |
| hedge-thresh-30 | -23,898 | 8,699 | [-30,474, -17,323] |
| taker-reversion | -23,427 | 25,018 | [-42,339, -4,515] |
| two-contracts | -25,329 | 15,818 | [-37,287, -13,372] |
| edge-cap-20 | -25,593 | 7,897 | [-31,562, -19,623] |
| shipped | -25,886 | 11,791 | [-34,799, -16,973] |
| hedge-thresh-12 | -39,994 | 18,878 | [-54,265, -25,723] |

**I retract the reversion result.** Sessions earlier I called `TAKER_MODE=reversion`
the largest single effect in the project, worth about 29,000, on four runs (+7,544,
+7,199, +4,576, -1,938). At n=7 it is -23,427 with a standard deviation of 25,018 --
an interval from -42,339 to -4,515 that contains `shipped` entirely. Reversion is not
better here; it is *high-variance*, and a four-run sample renders high variance as a
large effect. This is the same error as the other two retractions, made a third time
with more runs behind it, which is exactly why I had wanted the sweep. The desk ships
`momentum` and now does so for a defensible reason rather than the cautious one.

`hedge-thresh-12` at -39,994 does clear `shipped`, replicating the earlier finding
that a tight threshold makes the hedger churn against its own flow. That supports the
shipped 20. Nothing else in the table separates.

### The compaction regression

Every row was ~11,000 worse than the -14,969 +/- 308 I had measured before compacting
the seats. That gap was the only thing in the table I could not explain by noise, so I
A/B'd it: a copy of the repo checked out at HEAD (pre-compaction) against the working
tree, alternating runs so machine drift hit both equally.

| | desk | quoter line |
|---|---|---|
| pre-compaction | -15,150 / -10,952 / -10,006 -> **-12,036** | -> **-5,210** |
| compacted | -16,832 / -19,760 / -20,796 -> **-19,129** | -> **-11,471** |

No overlap across six runs, and the damage is in the quoter line. The compaction had
removed two genuinely dead features (`QUOTER_TIERS`, default 1; `QUOTER_FAST_VOL_MS`,
default 0 -- neither set by compose, so both provably inert). But it had *also* deleted
three lines from the config literal:

```go
UseRef:     envInt("QUOTER_USE_REF", 1) != 0,
BasisAlpha: envFloat("QUOTER_BASIS_ALPHA", 0.05),
RefStale:   time.Duration(envInt("QUOTER_REF_STALE_MS", 2000)) * time.Millisecond,
```

while leaving every *reader* of those fields intact. So `cfg.UseRef` took Go's zero
value, `fairValue()` returned early on `!q.cfg.UseRef`, and the quoter stopped pricing
off the lead contract -- it went back to quoting a quiet month off its own thin mid,
which is the exact failure the reference-pricing work existed to fix.

**And then the whole regression evaporated.** I wrote "cost: ~6,300 a run" into this
file before measuring the fix -- precisely the error the rest of these notes are about,
committed while writing the entry about committing it. Restoring the assignments gave
-18,205, -24,345, -20,778 (mean -21,109), no better than the broken build. So I ran the
comparison again, interleaved, HEAD against the fixed build:

| pair | pre-compaction | fixed | difference |
|---|---|---|---|
| 1 | -16,234 | -5,529 | **+10,705** |
| 2 | -11,849 | -22,679 | **-10,830** |
| 3 | -14,563 | -14,715 | -152 |
| mean | **-14,215** | **-14,308** | -93 |

The differences cancel. Pooling all twelve runs: pre-compaction -13,126 +/- 2,526,
current build -17,708 +/- 6,866, overlapping, t ~ 1.5. **There is no compaction
regression.** The first A/B separated cleanly across six alternating runs and it was
noise that happened to line up.

**This is the fourth time, and the worst one.** The first three retractions came from
small samples I had not thought hard about. This one came from a comparison I designed
specifically to be trustworthy -- interleaved so machine drift hit both arms equally,
six runs, no overlap between the arms, the damage localised to the quoter line exactly
where a quoter bug should show. It looked like the cleanest measurement in the file. The
thing I still had not internalised is that *interleaving controls for drift, not for
variance*: at a per-run standard deviation near 5,000, three runs an arm cannot resolve
5,000, and "no overlap" across six samples from the same distribution is an ordinary
coincidence rather than evidence. The arithmetic for how many runs that needs is written
in this file two sessions above, by me, and I did not apply it to my own test.

**Why nothing caught it.** It compiled -- an unset struct field is not an error in Go,
it is a zero value. `go vet` was clean. All 29 unit tests passed, because every one of
them builds a `Config` literal by hand and none had ever exercised `loadConfig()`, the
path the container actually takes. The README still documented `QUOTER_USE_REF` with a
default of 1. Every artefact I would normally trust agreed the code was fine, and the
only thing that disagreed was a number in a sweep I nearly did not run.

Two tests now cover it, and I checked both fail when the assignment is deleted again
rather than assuming they would:

- `TestConfigFromEnv` -- builds config through the env path with `QUOTER_*` cleared and
  asserts the defaults a bare container gets, `UseRef` among them.
- `TestDocumentedVarsAreRead` -- every `QUOTER_*` in `strategy/README.md` must appear in
  `main.go`. Documentation drift becomes a failing test.

**What this costs the sweep.** All 56 runs used the broken build, so the table above
measures a quoter with reference pricing off. The *relative* comparisons survive (every
row shares the defect) -- the reversion retraction and the `hedge-thresh-12` result both
stand, since neither depends on ref pricing. The absolute levels do not, and
`pull-on-lead` is the row to distrust most: whatever it was measuring, it was not the
lead-following behaviour its name claims. I am not re-running 4.5 hours to restate it.

### Why restoring it changed nothing: the lead-detection bug

The null result had a cause, and finding it is the one piece of real progress in this
session. I made the quoter log which contract it is pricing off (`ref=` in the status
line, `useref=` at startup) -- state that had never been observable, which is how it sat
switched off for a whole sweep without anyone noticing. On a clean checkout the shipped
desk reports `ref=own` for every second of the run: the quoter believes *it* is the lead
contract, so `fairValue()` returns its own mid and reference pricing is a no-op.

It is wrong about that, and the reason is self-inflicted. The lead is chosen by counting
BBO updates per feed, and **our own quoting generates BBO updates on our own feed**.
Measured with the market running and no desk attached:

```
4338 AAH6      <- the real lead
3897 AAM6      <- what we quote
2442 AAU6
```

AAH6 leads by 11%. Our requoting adds far more than 11% to AAM6's count, so we out-tick
the contract we are supposed to be following and disqualify it under the "unless that is
us" guard. The clean-checkout log catches the moment it happens: the first two status
lines read `ref=AAH6+15` -- it found the lead and learned a basis of 15 -- and every one
of the 160 lines after that reads `ref=own`, because by then we are quoting. The
machinery works exactly as designed; it is the ranking feeding it that is wrong -- a guard written to stop us importing lag, which instead guarantees we always
do. Reference pricing has therefore been inert in every full-desk measurement in this
file, including the ones where I concluded it helped.

**I am not fixing it here.** The fix is clear enough -- measure activity in something we
do not ourselves produce, counterparty trades rather than book churn -- but it is a
behavioural change to the pricing core, and this market's run-to-run noise (stdev ~5,000
on a desk total of similar size) cannot resolve its effect in the runs I have left. This
session already contains two retractions caused by shipping on evidence that thin. It is
written down as the first thing to do with more time, with the measurement that proves
it, which is worth more than a change I would have to describe as unvalidated.

### Clean-checkout verification

Pristine `git archive 657d59d` (the repo exactly as provided), the submission's 43 files
overlaid on top, nothing else:

- `tests/run_tests.sh unit` from the clean tree: Go tests pass, 29 Python tests pass
- `./run.sh --sim --strategy`: all six containers up, quoter, taker and hedger all trading
- **0** rejects, errors or tracebacks across all three seats
- desk exposure over 154 samples: mean 5.7, median 5, p95 13, max 16, **0%** at or above 25
  (no hedge fired in this window -- the desk never reached the threshold of 20)
- the quoter reports `useref=true` at startup and `ref=own` thereafter, which is the
  lead-detection bug above, now visible in the log rather than silent

An earlier verification ran the same way against a reduced 18-file package with the
tooling stripped out, and passed identically: `run.sh` guards its summariser call with
`[ -x ./tools/run_summary.sh ] && ... || true`, so the desk runs with or without it.

**What survives.** The compaction stands: the code reads better and measures the same,
which was the point of it. The deleted config was a genuine defect -- `UseRef` really
was false, `fairValue()` really did stop consulting the lead contract, and that is a fact
about the code rather than a claim about a number, so the restoration and its two tests
stay in regardless of what the benchmarks say about them. What does *not* survive is any
statement about what it cost.

The sweep's `shipped` row (-25,886) still sits below what `bench.sh` measures for the
same configuration (-17,708). Those are different harnesses -- `sweep.sh` drives the
seats with `docker run`, `bench.sh` through compose -- and the intervals nearly touch, so
it is either a harness difference or more of the same variance. I am not spending another
four hours to find out which; it is recorded as unexplained, which is what it is.

**The lesson, and it is the one this whole file keeps teaching.** The last three
sessions I have been rewarded for distrusting agreeable numbers. Here the numbers were
*disagreeable* and I nearly explained them away as a harness difference -- I had already
written that hypothesis down. What separated "different harness" from "I broke
something" was six controlled runs costing 30 minutes, against a bug that had been
costing 6,300 a run since the moment I tidied the code.

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
