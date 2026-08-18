#!/usr/bin/env bash
# Who is actually trading with our quoter, and at what cost to us?
#
#   tools/counterparties.sh [SECONDS] [SENDER]
#
# A market maker earns from uninformed flow and loses to informed flow, so the
# identity of the counterparty is the diagnosis. In the sample market they are
# distinguishable by construction (sim/market.py):
#
#   MOVER001   walks the price and sweeps the book to get there -- informed, and
#              the flow that runs us over
#   TAKER001/2 casual takers crossing at most 6 ticks through the touch -- the
#              uninformed flow a market maker wants
#   BGQUOT*    background quoters resting liquidity
#
# The hypothesis this was written to test: with the edge capped at 20 our quotes
# sit far outside the touch, further than a casual taker's limit price can reach,
# so the only counterparty that can fill us is the one we least want.
set -uo pipefail
cd "$(dirname "$0")/.."

SECS="${1:-60}"
SENDER="${2:-QUOTE001}"
FEED="${FEED:-AAH6}"
export NATS_URL="${NATS_URL:-nats://localhost:4222}"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
echo "listening to ${SENDER}'s fills for ${SECS}s..." >&2
nats sub --raw "ex.md.${FEED}.${SENDER}" > "$tmp/f.log" 2>&1 &
subpid=$!
sleep "$SECS"
kill "$subpid" 2>/dev/null

awk -v me="$SENDER" '
  $2=="E" || $2=="T" {
    inc=$3; rest=$4; vol=$5; px=$6; side=$8
    # our side: aggressor side if we came in, the opposite if we were resting
    if (index(inc, me ":")==1) { us=side; them=rest }
    else if (index(rest, me ":")==1) { us=(side=="B"?"S":"B"); them=inc }
    else next
    split(them, p, ":"); who=p[1]
    lots[who]+=vol; n[who]++
    # signed cash: buying spends, selling receives
    cash[who] += (us=="B" ? -vol*px : vol*px)
    pos[who]  += (us=="B" ?  vol    : -vol)
    total+=vol
  }
  END{
    if (!total) { print "no fills seen -- is the desk running?"; exit }
    printf "%-12s %8s %8s %10s   %s\n", "counterparty", "fills", "lots", "share", "net cash (unmarked)"
    for (w in lots)
      printf "%-12s %8d %8d %9.0f%%   %+d\n", w, n[w], lots[w], 100*lots[w]/total, cash[w]
    printf "\ntotal lots: %d\n", total
  }' "$tmp/f.log"
