# Notes

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
| 206  | orderid not used    | cancelling an id that was never added         |
| 300  | volume too high     | volume > `max_volume`                         |
| 302  | price out of range  | price outside `ref_price ± band`              |

`306`/`307` (rate limiting, per the changelog) not yet reached — see below.

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
