#!/usr/bin/env bash
# Run the exchange, optionally with the sample market and/or your strategy.
#   ./run.sh                       exchange only
#   ./run.sh --sim                 + sample market
#   ./run.sh --strategy            + your strategy (from ./strategy)
#   ./run.sh --sim --strategy      everything (this is what grading runs)
set -e
cd "$(dirname "$0")"
./setup.sh
profiles=()
for a in "$@"; do
  case "$a" in
    --sim)      profiles+=(--profile sim) ;;
    --strategy) profiles+=(--profile strategy) ;;
    *) echo "unknown option: $a" >&2; exit 1 ;;
  esac
done
# Leave a record of every run in runs/, so a session's outcome does not depend on
# anyone having watched the terminal. Runs on exit, including Ctrl-C: `up` without
# -d stops the containers but does not remove them, so their logs are still
# readable. Never allowed to affect the exit status of the run itself.
summarise() {
  local status=$?
  [ -x ./tools/run_summary.sh ] && ./tools/run_summary.sh || true
  return $status
}
trap summarise EXIT

# --build: always rebuild from source so edits take effect (compose would
# otherwise happily reuse a stale image built from older code).
# Not `exec`: that would replace this shell and the summary would never run.
docker compose "${profiles[@]}" up --build
