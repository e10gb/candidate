#!/usr/bin/env bash
# Run a list of configurations unattended and collect them into one CSV.
#
#   tools/sweep.sh sweeps/example.txt [-d 240] [-r 5]
#
# Each non-comment line of the file is one configuration: a label, then any
# KEY=VALUE overrides.
#
#     baseline
#     wider-edge      QUOTER_MAX_EDGE=60
#     patient-hedger  HEDGE_THRESH=25
#
# This exists because the binding constraint on every tuning decision in this
# project was sample size, not ideas. Run-to-run spread is ~5,500 on a desk total
# of the same order, so three runs cannot separate configurations that differ by
# a few thousand -- and three times today a pair of agreeing runs was overturned
# by the next one. Sweeps are slow (a 240s run costs ~4.5 minutes with setup), so
# this is built to be started and left alone.
set -uo pipefail
cd "$(dirname "$0")/.."

DURATION=240
REPEATS=5
FILE=""
while [ $# -gt 0 ]; do
  case "$1" in
    -d) DURATION="$2"; shift 2 ;;
    -r) REPEATS="$2"; shift 2 ;;
    *) FILE="$1"; shift ;;
  esac
done
[ -n "$FILE" ] && [ -f "$FILE" ] || { echo "usage: $0 CONFIGFILE [-d SECS] [-r REPEATS]" >&2; exit 2; }

mkdir -p runs
OUT="runs/sweep-$(date +%Y%m%d-%H%M%S).csv"
echo "label,run,quoter,taker,hedger,desk,max_desk,mean_desk,hedger_lots" > "$OUT"
total=$(grep -cvE '^\s*(#|$)' "$FILE")
echo "sweep: $total configs x $REPEATS runs x ${DURATION}s -> $OUT" >&2
echo "estimated wall time: $(( total * REPEATS * (DURATION + 40) / 60 )) minutes" >&2

n=0
while read -r label rest; do
  case "$label" in ''|\#*) continue ;; esac
  n=$((n + 1))
  for r in $(seq 1 "$REPEATS"); do
    echo "[sweep] $label ($n/$total) run $r/$REPEATS" >&2
    line=$(./tools/bench.sh -d "$DURATION" -n "$label" $rest 2>/dev/null | grep '^CSV')
    # A run that produced nothing must say so. Writing a blank row let a whole
    # configuration report "mean 0" from three empty results, which reads as a
    # measurement rather than a failure.
    if [ -z "$line" ]; then
      echo "[sweep] WARNING: $label run $r produced no result -- skipping" >&2
      continue
    fi
    # CSV: label,secs,max,mean,>=10,>=25,quoter,taker,hedger,total,lots,...
    echo "$line" | awk -F, -v l="$label" -v r="$r" \
      '{printf "%s,%s,%s,%s,%s,%s,%s,%s,%s\n", l, r, $8, $9, $10, $11, $4, $5, $12}' >> "$OUT"
  done
done < "$FILE"

echo >&2
echo "=== $OUT ===" >&2
awk -F, 'NR>1 && $6 != "" {n[$1]++; s[$1]+=$6; ss[$1]+=$6*$6}
  END{ printf "%-24s %6s %12s %10s %22s\n", "config", "runs", "mean desk", "stdev", "95% CI"
       for (k in n) { m=s[k]/n[k]
         sd=(n[k]>1)?sqrt((ss[k]-n[k]*m*m)/(n[k]-1)):0
         se=(n[k]>1)?sd/sqrt(n[k]):0
         printf "%-24s %6d %12.0f %10.0f   [%8.0f, %8.0f]\n", k, n[k], m, sd, m-2*se, m+2*se } }' "$OUT"
