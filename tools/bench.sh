#!/usr/bin/env bash
# Run the full desk against the sample market for a fixed window and report the
# two numbers that matter together: how much risk the desk carried, and what it
# cost. Tuning either one alone is how you end up with a perfectly flat desk that
# loses money, or a profitable quoter that blows up.
#
#   tools/bench.sh                                   # defaults, 90s
#   tools/bench.sh -d 120 HEDGE_THRESH=20
#   tools/bench.sh -n "wide" QUOTER_EDGE=15 HEDGE_THRESH=20
#
# Any KEY=VALUE is exported for the run; docker-compose.yml passes the tunables
# through to the seats. Each run starts from `docker compose down` because a
# long-lived exchange silently breaks the sample market: order ids are consumed
# permanently per sender, and the sim publishes fire-and-forget, so once it
# collides with its own earlier ids its orders vanish without any error and no
# market ever forms.
set -euo pipefail
cd "$(dirname "$0")/.."

DURATION=90
LABEL=""
while [ $# -gt 0 ]; do
  case "$1" in
    -d) DURATION="$2"; shift 2 ;;
    -n) LABEL="$2"; shift 2 ;;
    *=*) export "${1?}"; shift ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done
[ -n "$LABEL" ] || LABEL="$(printenv | grep -E '^(QUOTER|HEDGE)_' | grep -v '=$' | paste -sd, - || echo defaults)"
[ -n "$LABEL" ] || LABEL="defaults"

log() { printf '[bench] %s\n' "$*" >&2; }

# `docker compose down` without the profiles leaves profile services running: it
# only removes nats and exchange, and the network removal then fails with
# "resource is still in use". Worse, the following `up` reuses any still-running
# seat whose config is unchanged, so a repeat of the same setting silently
# continues from the previous run's positions. Profiles on every call, and
# --force-recreate so identical configs still start from zero.
DC=(docker compose --profile sim --profile strategy)

log "run: $LABEL  (${DURATION}s)"
"${DC[@]}" down --remove-orphans >/dev/null 2>&1 || true
"${DC[@]}" up -d --build --force-recreate >/dev/null 2>&1
# Let the sim seed a book and the seats connect before the measurement window.
sleep 15
log "measuring..."
sleep "$DURATION"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
"${DC[@]}" logs --no-log-prefix --tail 2000 hedger   > "$tmp/h.log" 2>&1
"${DC[@]}" logs --no-log-prefix --tail 2000 strategy > "$tmp/q.log" 2>&1
"${DC[@]}" logs --no-log-prefix --tail 2000 taker    > "$tmp/t.log" 2>&1
"${DC[@]}" down --remove-orphans >/dev/null 2>&1 || true

# Risk: the distribution of desk exposure, not just its endpoint. A desk that
# averages zero by oscillating between -40 and +40 is not flat.
risk="$(grep -o 'desk=[-0-9]*' "$tmp/h.log" | cut -d= -f2 | awk -v d="$DURATION" '
  {n++; a=($1<0?-$1:$1); s+=a; if(a>mx)mx=a; if(a>=10)o10++; if(a>=25)o25++}
  END{ if(!n){print "no-data"; exit}
       printf "%d %.2f %.0f %.0f", mx, s/n, 100*o10/n, 100*o25/n }')"

# PnL: each seat marks its own inventory; "n/a" means it could not be valued.
pnl_of() { grep -o 'pnl=[-0-9]*' "$1" | tail -1 | cut -d= -f2; }
qp="$(pnl_of "$tmp/q.log")"; tp="$(pnl_of "$tmp/t.log")"; hp="$(pnl_of "$tmp/h.log")"
churn="$(grep -o 'traded=[0-9]*' "$tmp/h.log" | tail -1 | cut -d= -f2)"

read -r mx mean o10 o25 <<<"$risk"
total=$(( ${qp:-0} + ${tp:-0} + ${hp:-0} ))

printf '\n%s\n' "----------------------------------------------------------------"
printf 'run            : %s (%ss)\n' "$LABEL" "$DURATION"
printf 'RISK  max|desk|: %-6s mean|desk|: %-7s >=10: %s%%  >=25: %s%%\n' "$mx" "$mean" "$o10" "$o25"
printf 'PNL   quoter   : %-8s taker: %-8s hedger: %-8s\n' "${qp:-n/a}" "${tp:-n/a}" "${hp:-n/a}"
printf 'PNL   TOTAL    : %-8s   (hedger lots traded: %s)\n' "$total" "${churn:-0}"
printf '%s\n' "----------------------------------------------------------------"

# Machine-readable, for collecting a sweep into one table.
# label,secs,max,mean,pct>=10,pct>=25,quoter,taker,hedger,total,hedger_lots
printf 'CSV,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s\n' \
  "$LABEL" "$DURATION" "$mx" "$mean" "$o10" "$o25" "${qp:-}" "${tp:-}" "${hp:-}" "$total" "${churn:-0}"
