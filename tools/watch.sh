#!/usr/bin/env bash
# Live, readable view of one contract: top-of-book changes and trades.
#
#   tools/watch.sh            # AAH6
#   tools/watch.sh AAM6
#
# Needs the `nats` CLI and the exchange running (docker compose --profile sim up -d).
# Prints a line only when the top of book actually CHANGES -- the raw feed
# republishes on every book event, so an unfiltered `nats sub` is mostly noise.
#
# One subscription, not two: two `nats sub` processes writing into the same pipe
# interleave mid-line and corrupt the output.
set -euo pipefail
FEED="${1:-AAH6}"
export NATS_URL="${NATS_URL:-nats://localhost:4222}"

printf '%-12s  %18s  %5s  %s\n' "time(utc)" "book" "spr" "event"
printf -- '---------------------------------------------------------------\n'

nats sub "ex.>" 2>/dev/null | awk -v feed="$FEED" '
  # ns timestamps exceed double precision, so slice the string instead of doing maths on it
  function clock(ns,   s, hh, mm, ss) {
    s  = substr(ns, 1, 10) + 0
    hh = int(s / 3600) % 24; mm = int(s / 60) % 60; ss = s % 60
    return sprintf("%02d:%02d:%02d.%s", hh, mm, ss, substr(ns, 11, 3))
  }
  NF == 0 { next }                            # nats prints a blank line after each body
  # header line carries the subject; the body is the next non-empty line
  /^\[#[0-9]+\] Received on/ {
    if (match($0, /"[^"]+"/)) subj = substr($0, RSTART + 1, RLENGTH - 2)
    next
  }
  subj == "ex.bbo." feed {
    # <ts> <feed> <bid_px> <bid_vol> <ask_px> <ask_vol>, "-" for an empty side
    bp = $3; bv = $4; ap = $5; av = $6
    line = sprintf("%6s x%-5s %6s x%-5s", bp, bv, ap, av)
    if (line == last) next                    # unchanged top of book: skip
    last = line
    spr = (bp == "-" || ap == "-") ? "--" : ap - bp
    printf "%-12s  %18s  %5s\n", clock($1), line, spr
    fflush()
  }
  subj ~ ("^ex\\.md\\." feed "\\.") && $2 == "T" {
    # <ts> T <incoming:17> <resting:17> <volume> <price> <matchid> <B|S>
    printf "%-12s  %18s  %5s  TRADE %4s @ %-6s (%s aggressed)\n", \
           clock($1), "", "", $5, $6, ($8 == "B" ? "buyer" : "seller")
    fflush()
  }
'
