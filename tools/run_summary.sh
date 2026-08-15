#!/usr/bin/env bash
# Write a summary of how a desk run went, to runs/run-<timestamp>.md.
#
#   tools/run_summary.sh                 # pull logs from the compose stack
#   tools/run_summary.sh --logs DIR      # use q.log / h.log / t.log in DIR
#   tools/run_summary.sh -o report.md
#
# `run.sh` calls this on exit, so every run leaves a record without anyone
# remembering to look at the terminal.
#
# The WARNINGS section matters more than the numbers. Several failure modes here
# produce output that looks perfectly healthy: a dead sample market and a seat
# that cannot place orders both show a quiet, flat, zero-fill log, and a position
# valued at zero reads as profit. Those are checked explicitly.
set -uo pipefail
cd "$(dirname "$0")/.."

OUT=""
LOGDIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) OUT="$2"; shift 2 ;;
    --logs) LOGDIR="$2"; shift 2 ;;
    *) echo "usage: $0 [-o FILE] [--logs DIR]" >&2; exit 2 ;;
  esac
done

tmp=""
if [ -z "$LOGDIR" ]; then
  tmp="$(mktemp -d)"; LOGDIR="$tmp"
  DC=(docker compose --profile sim --profile strategy)
  "${DC[@]}" logs --no-log-prefix --tail 5000 strategy > "$LOGDIR/q.log" 2>/dev/null
  "${DC[@]}" logs --no-log-prefix --tail 5000 hedger   > "$LOGDIR/h.log" 2>/dev/null
  "${DC[@]}" logs --no-log-prefix --tail 5000 taker    > "$LOGDIR/t.log" 2>/dev/null
fi
cleanup() { [ -n "$tmp" ] && rm -rf "$tmp"; }
trap cleanup EXIT

Q="$LOGDIR/q.log"; H="$LOGDIR/h.log"; T="$LOGDIR/t.log"
for f in "$Q" "$H" "$T"; do [ -f "$f" ] || : > "$f"; done
if [ ! -s "$Q" ] && [ ! -s "$H" ] && [ ! -s "$T" ]; then
  echo "run_summary: no seat logs found (was the stack up?)" >&2
  exit 1
fi

mkdir -p runs
[ -n "$OUT" ] || OUT="runs/run-$(date +%Y%m%d-%H%M%S).md"

# --- metrics ------------------------------------------------------------- #
last_field() { grep -o "$2=[-0-9]*" "$1" | tail -1 | cut -d= -f2; }

qpnl=$(last_field "$Q" pnl);  qpos=$(last_field "$Q" pos);  qfills=$(last_field "$Q" fills)
tpnl=$(last_field "$T" pnl);  tpos=$(last_field "$T" pos);  tfills=$(last_field "$T" fills)
hpnl=$(last_field "$H" pnl);  hpos=$(grep -o 'hedger=[-0-9]*' "$H" | tail -1 | cut -d= -f2)
hedges=$(last_field "$H" hedges); hlots=$(last_field "$H" traded)
total=$(( ${qpnl:-0} + ${tpnl:-0} + ${hpnl:-0} ))

read -r dmax dmean dover <<<"$(grep -o 'desk=[-0-9]*' "$H" | cut -d= -f2 | awk '
  {n++; a=($1<0?-$1:$1); s+=a; if(a>mx)mx=a; if(a>=25)o++}
  END{ if(!n){print "- - -"; exit} printf "%d %.2f %.0f", mx, s/n, 100*o/n }')"

read -r spmed spmax <<<"$(grep -oE 'bid=[0-9]+x[0-9]+ ask=[0-9]+x[0-9]+' "$Q" |
  sed -E 's/bid=([0-9]+)x[0-9]+ ask=([0-9]+)x[0-9]+/\1 \2/' | awk '
  {w=$2-$1; n++; a[n]=w; if(w>mx)mx=w}
  END{ if(!n){print "- -"; exit}
       for(i=1;i<=n;i++)for(j=i+1;j<=n;j++)if(a[j]<a[i]){t=a[i];a[i]=a[j];a[j]=t}
       printf "%d %d", a[int(n/2)+1], mx }')"

read -r qposmean qposmax <<<"$(grep -o 'pos=[-0-9]*' "$Q" | cut -d= -f2 | awk '
  {n++; a=($1<0?-$1:$1); s+=a; if(a>mx)mx=a}
  END{ if(!n){print "- -"; exit} printf "%.2f %d", s/n, mx }')"

errs=$(cat "$Q" "$H" "$T" | grep -ciE 'reject|error|failed')
first_ts=$(grep -oE '[0-9]{2}:[0-9]{2}:[0-9]{2}' "$Q" | head -1)
last_ts=$(grep -oE '[0-9]{2}:[0-9]{2}:[0-9]{2}' "$Q" | tail -1)

# --- report -------------------------------------------------------------- #
{
  echo "# Desk run -- $(date '+%Y-%m-%d %H:%M:%S')"
  echo
  echo "Window: ${first_ts:-?} to ${last_ts:-?} (seat clocks)."
  echo
  echo '## Seats'
  echo
  echo '| seat | position | PnL | fills |'
  echo '|---|---|---|---|'
  printf '| quoter | %s | %s | %s |\n' "${qpos:--}" "${qpnl:--}" "${qfills:--}"
  printf '| taker  | %s | %s | %s |\n' "${tpos:--}" "${tpnl:--}" "${tfills:--}"
  printf '| hedger | %s | %s | %s hedges, %s lots |\n' \
     "${hpos:--}" "${hpnl:--}" "${hedges:--}" "${hlots:--}"
  printf '| **desk** | | **%s** | |\n' "$total"
  echo
  echo '## Risk'
  echo
  printf -- '- Desk exposure: max |desk| **%s**, mean **%s**, %s%% of samples at/over 25\n' \
     "${dmax:--}" "${dmean:--}" "${dover:-0}"
  printf -- '- Quoter inventory: mean |pos| %s, max %s\n' "${qposmean:--}" "${qposmax:--}"
  echo
  echo '## Quoting'
  echo
  printf -- '- Quoted spread: median %s, max %s\n' "${spmed:--}" "${spmax:--}"
  printf -- '- Rejects/errors logged across all seats: %s\n' "$errs"
  echo
  echo '## Warnings'
  echo
  warned=0
  note() { echo "- $*"; warned=1; }
  [ "${qfills:-0}" -eq 0 ] 2>/dev/null && \
    note '**Quoter never traded.** A quiet zero-fill run is what a dead sample market and a seat that cannot place orders both look like. Check the book is alive (`tools/watch.sh`) before reading anything else here.'
  [ "${tfills:-0}" -eq 0 ] 2>/dev/null && \
    note '**Taker never traded.** It shipped with an order-entry bug that produced exactly this, silently.'
  [ "${errs:-0}" -gt 0 ] && \
    note "Seats logged $errs reject/error lines -- a healthy run logs none."
  [ "${dmax:-0}" -ge 25 ] 2>/dev/null && \
    note "Desk exposure reached ${dmax}; the hedger is meant to keep this small."
  grep -q 'pnl=n/a' "$Q" "$H" "$T" 2>/dev/null && \
    note 'A seat could not mark its position at some point (`pnl=n/a`) -- usually an empty book.'
  # Only status lines carry pos, cash and pnl together. Fill lines carry pos=
  # alone, and awk variables persist across records, so without resetting them a
  # fill line inherits the previous status line's cash/pnl -- which are equal
  # whenever the position was flat. That produced a false alarm on every run.
  unmarked=$(awk '{p=""; c=""; n="";
      for(i=1;i<=NF;i++){if($i~/^pos=/)p=substr($i,5); if($i~/^cash=/)c=substr($i,6); if($i~/^pnl=/)n=substr($i,5)}
      if(p!="" && c!="" && n!="" && p+0!=0 && n==c) bad++} END{print bad+0}' "$Q")
  [ "${unmarked:-0}" -gt 0 ] && \
    note "Quoter reported PnL equal to cash on $unmarked lines while holding a position -- inventory is not being marked."
  [ "$warned" -eq 0 ] && echo '- None. Seats traded, desk stayed bounded, nothing rejected.'
} > "$OUT"

echo "run summary written to $OUT" >&2
