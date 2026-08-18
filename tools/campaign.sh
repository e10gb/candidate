#!/usr/bin/env bash
# Measure the quoter properly: repeated long runs, with statistics.
#
#   tools/campaign.sh -r 3 -d 240 -n baseline
#   tools/campaign.sh -r 3 -d 240 -n "wide" QUOTER_EDGE_VOL=6
#
# Why this exists rather than repeating tools/bench.sh by hand:
#
#  * The taker is excluded. It contributed -2,000 to -28,000 per run, several
#    times the effect being measured, and it is not what we are tuning. The
#    quoter and hedger still run together, so the quoter's inventory is managed
#    the way it will be in production.
#  * Runs are long. Effects worth a few thousand cannot be seen against per-run
#    noise of ~7,000 in two-minute windows.
#  * It reports mean and spread across repeats, not a single number. Two runs
#    landing the same side of zero fooled me twice; the sample size is the whole
#    point of this script.
#  * PnL is the liquidation-marked figure (`liq`), i.e. after closing the
#    position against the book -- the number the session actually ends on.
set -uo pipefail
cd "$(dirname "$0")/.."

DURATION=240
REPEATS=3
LABEL=""
ENVS=()
while [ $# -gt 0 ]; do
  case "$1" in
    -d) DURATION="$2"; shift 2 ;;
    -r) REPEATS="$2"; shift 2 ;;
    -n) LABEL="$2"; shift 2 ;;
    *=*) ENVS+=("$1"); shift ;;
    *) echo "usage: $0 [-d SECS] [-r REPEATS] [-n LABEL] [KEY=VAL ...]" >&2; exit 2 ;;
  esac
done
[ -n "$LABEL" ] || LABEL="${ENVS[*]-defaults}"
[ -n "$LABEL" ] || LABEL="defaults"

log() { printf '[campaign] %s\n' "$*" >&2; }
DC=(docker compose --profile sim)
NET=""
results=()

docker build -q -t campaign-quoter ./strategy >/dev/null || exit 1
docker build -q -t campaign-hedger ./hedger   >/dev/null || exit 1

for r in $(seq 1 "$REPEATS"); do
  log "run $r/$REPEATS: $LABEL (${DURATION}s)"
  # A long-lived exchange silently kills the sample market: order ids are consumed
  # permanently per sender and the sim publishes fire-and-forget, so it never sees
  # its own 203s. Every repeat starts from a clean exchange.
  docker compose --profile sim --profile strategy down --remove-orphans >/dev/null 2>&1
  "${DC[@]}" up -d --force-recreate >/dev/null 2>&1
  sleep 12
  NET=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' \
        "$(docker compose ps -q nats)" 2>/dev/null)

  envargs=()
  if [ "${#ENVS[@]}" -gt 0 ]; then
    for e in "${ENVS[@]}"; do envargs+=(-e "$e"); done
  fi

  docker run -d --rm --name camp-q --network "$NET" -e NATS_URL=nats://nats:4222 \
    -e SENDER=QUOTE001 ${envargs[@]+"${envargs[@]}"} campaign-quoter >/dev/null
  docker run -d --rm --name camp-h --network "$NET" -e NATS_URL=nats://nats:4222 \
    -e HEDGER_SENDER=HEDGE001 -e SENDER=QUOTE001 ${envargs[@]+"${envargs[@]}"} campaign-hedger >/dev/null

  sleep "$DURATION"
  q=$(docker logs camp-q 2>&1 | tail -400)
  docker stop camp-q camp-h >/dev/null 2>&1
  docker compose --profile sim --profile strategy down --remove-orphans >/dev/null 2>&1

  liq=$(printf '%s\n' "$q" | grep -o 'liq=[-0-9]*' | tail -1 | cut -d= -f2)
  fills=$(printf '%s\n' "$q" | grep -o 'fills=[0-9]*' | tail -1 | cut -d= -f2)
  spread=$(printf '%s\n' "$q" | grep -oE 'bid=[0-9]+x[0-9]+ ask=[0-9]+x[0-9]+' |
    sed -E 's/bid=([0-9]+)x[0-9]+ ask=([0-9]+)x[0-9]+/\1 \2/' |
    awk '{n++; s+=$2-$1} END{if(n) printf "%.0f", s/n; else printf "-"}')
  log "  -> liq=${liq:-?} fills=${fills:-?} avg_spread=${spread:-?}"
  results+=("${liq:-0}")
done

printf '\n================ %s ================\n' "$LABEL"
printf 'runs: %s x %ss (quoter + hedger, no taker)\n' "$REPEATS" "$DURATION"
printf 'quoter PnL (closed out): %s\n' "${results[*]}"
printf '%s\n' "${results[@]}" | awk '
  {n++; s+=$1; a[n]=$1; if(n==1||$1<mn)mn=$1; if(n==1||$1>mx)mx=$1}
  END{ if(!n) exit
       m=s/n; for(i=1;i<=n;i++) v+=(a[i]-m)^2
       sd=(n>1)?sqrt(v/(n-1)):0
       printf "mean %.0f   stdev %.0f   min %.0f   max %.0f\n", m, sd, mn, mx
       # A mean smaller than the spread of the runs is not a result.
       if (sd>0 && (m<0?-m:m) < sd) print "VERDICT: indistinguishable from zero at this sample size"
       else if (m>0) print "VERDICT: positive, and larger than the run-to-run spread"
       else print "VERDICT: negative, and larger than the run-to-run spread" }'
