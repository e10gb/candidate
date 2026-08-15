#!/usr/bin/env bash
# The desk's test suite. Four layers, cheapest first:
#
#   1. Go unit tests      -- quoter pricing, marking, fill sides, token bucket
#   2. Python unit tests  -- hedger desk accounting, taker signal handling
#   3. Protocol tests     -- the exchange behaviours the code depends on, asserted
#                            against a live exchange on an empty book
#   4. End-to-end smoke   -- the full stack on the sample market, asserting the
#                            desk trades, stays flat, and logs no rejects
#
#   tests/run_tests.sh              # everything (~2 minutes)
#   tests/run_tests.sh unit         # layers 1-2 only, no Docker network needed
#   tests/run_tests.sh -d 90        # longer smoke window
#
# Everything runs in containers: no Go toolchain, no nats-py, no venv on the host.
set -uo pipefail
cd "$(dirname "$0")/.."

SMOKE_SECS=45
ONLY=""
while [ $# -gt 0 ]; do
  case "$1" in
    -d) SMOKE_SECS="$2"; shift 2 ;;
    unit|protocol|smoke) ONLY="$1"; shift ;;
    *) echo "usage: $0 [unit|protocol|smoke] [-d SECONDS]" >&2; exit 2 ;;
  esac
done

DC=(docker compose --profile sim --profile strategy)
fails=0
section() { printf '\n\033[1m== %s ==\033[0m\n' "$*"; }
ok()   { printf '  \033[32mPASS\033[0m  %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; fails=$((fails + 1)); }

want() { # want <description> <actual> <expected>
  if [ "$2" = "$3" ]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi
}

run_unit() {
  section "1. Go unit tests (quoter)"
  if docker run --rm -v "$PWD/strategy":/w -w /w golang:1.23-alpine \
       sh -c 'go test ./... 2>&1'; then ok "go test"; else bad "go test"; fi

  section "2. Python unit tests (hedger, taker)"
  docker build -q -t desk-test-py ./hedger >/dev/null || { bad "build test image"; return; }
  if docker run --rm -v "$PWD":/repo -w /repo desk-test-py \
       python3 tests/test_seats.py 2>&1 | tail -5; then ok "seat unit tests"
  else bad "seat unit tests"; fi
}

run_protocol() {
  section "3. Protocol assertions (live exchange, empty book)"
  docker compose up -d >/dev/null 2>&1
  sleep 5
  docker build -q -t desk-test-py ./hedger >/dev/null
  if docker run --rm --network candidate_default -e NATS_URL=nats://nats:4222 \
       -v "$PWD/tests":/t -w /t desk-test-py python3 test_protocol.py; then
    ok "protocol assertions"
  else
    bad "protocol assertions -- an assumption the seats rely on no longer holds"
  fi
}

run_smoke() {
  section "4. End-to-end smoke (${SMOKE_SECS}s on the sample market)"
  # A stale exchange silently kills the sample market (used-up order ids), and
  # `down` without profiles leaves seats running, so always reset with profiles.
  "${DC[@]}" down --remove-orphans >/dev/null 2>&1
  "${DC[@]}" up -d --build --force-recreate >/dev/null 2>&1
  sleep 12
  sleep "$SMOKE_SECS"

  local tmp; tmp="$(mktemp -d)"
  "${DC[@]}" logs --no-log-prefix --tail 2000 strategy > "$tmp/q.log" 2>&1
  "${DC[@]}" logs --no-log-prefix --tail 2000 hedger   > "$tmp/h.log" 2>&1
  "${DC[@]}" logs --no-log-prefix --tail 2000 taker    > "$tmp/t.log" 2>&1
  "${DC[@]}" down --remove-orphans >/dev/null 2>&1

  # The desk actually traded. A silent, flat, zero-fill run looks healthy and is
  # the single most misleading failure mode here -- it is what a dead sample
  # market looks like, and what the shipped taker looked like for its whole life.
  local qf; qf=$(grep -c 'fill ' "$tmp/q.log")
  if [ "$qf" -gt 0 ]; then ok "quoter traded ($qf fills)"; else bad "quoter never traded"; fi

  if grep -q 'pos=' "$tmp/t.log"; then ok "taker reporting"; else bad "taker silent"; fi
  if grep -q 'desk=' "$tmp/h.log"; then ok "hedger reporting"; else bad "hedger silent"; fi

  # The quoter quoted rather than only taking: it should have rested prices.
  if grep -qE 'bid=[0-9]+x[0-9]+' "$tmp/q.log"; then ok "quoter rested two-sided prices"
  else bad "quoter never had a resting quote"; fi

  # Risk: the desk stayed bounded. This is the headline claim of Job 2.
  local maxdesk
  maxdesk=$(grep -o 'desk=[-0-9]*' "$tmp/h.log" | cut -d= -f2 |
            awk '{a=($1<0?-$1:$1); if(a>m)m=a} END{print m+0}')
  if [ "${maxdesk:-999}" -le 30 ]; then ok "desk exposure bounded (max |desk| = $maxdesk)"
  else bad "desk exposure reached $maxdesk"; fi

  # No seat should be logging rejects or errors in a healthy run.
  local errs; errs=$(cat "$tmp"/*.log | grep -ciE 'reject|error|failed')
  want "no rejects or errors from any seat" "$errs" "0"

  # PnL must never be reported as bare cash while holding a position.
  local badpnl
  badpnl=$(grep 'pnl=' "$tmp/q.log" | awk '
    {for(i=1;i<=NF;i++){if($i~/^pos=/)p=substr($i,5); if($i~/^cash=/)c=substr($i,6); if($i~/^pnl=/)n=substr($i,5)}
     if(p+0!=0 && n==c) bad++} END{print bad+0}')
  want "position always marked into PnL" "$badpnl" "0"

  rm -rf "$tmp"
}

case "$ONLY" in
  unit)     run_unit ;;
  protocol) run_protocol ;;
  smoke)    run_smoke ;;
  *)        run_unit; run_protocol; run_smoke ;;
esac

printf '\n'
if [ "$fails" -eq 0 ]; then
  printf '\033[32mall checks passed\033[0m\n'
else
  printf '\033[31m%d check(s) failed\033[0m\n' "$fails"
fi
exit $((fails > 0))
