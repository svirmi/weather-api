#!/usr/bin/env bash
# T1–T8 driver for the pilot test matrix in binance-metals-docs/DEV_PLAN.md.
# Usage: VERIFY_WINDOW_SEC=60 DB_OUTAGE_SEC=45 ./scripts/test.sh
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
COMPOSE=(docker compose -f docker-compose.yml -f docker-compose.test.yml)
BASE_SYMBOLS="${SYMBOLS:-XPTUSDT,XPDUSDT,XAUUSDT,XAGUSDT}"
WINDOW="${VERIFY_WINDOW_SEC:-180}"
CHECK_WINDOW="${CHECK_WINDOW_SEC:-$WINDOW}"
OUTAGE="${DB_OUTAGE_SEC:-180}"
FAILED=0
START_TS=$(date -u +%Y-%m-%dT%H:%M:%SZ)

record() {
  local id="$1" ok="$2" detail="$3"
  if [ "$ok" = "1" ]; then
    printf 'PASS  %s  %s\n' "$id" "$detail"
  else
    printf 'FAIL  %s  %s\n' "$id" "$detail"
    FAILED=1
  fi
}

metrics() { curl -fsS --max-time 3 http://127.0.0.1:9090/metrics 2>/dev/null || true; }
mget() { metrics | grep -E "^$1([ {]|$)" | grep -v '^#' | tail -1 | awk '{print $NF}'; }
mcontains() { metrics | grep -F "$1" | grep -v '^#' | head -1 | awk '{print $NF}'; }
sum_ticks() { metrics | grep -E '^ticks_total\{' | awk '{s+=$NF} END {print s+0}'; }

wait_health() {
  for _ in $(seq 1 30); do
    if curl -fsS --max-time 2 http://127.0.0.1:9090/healthz >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  return 1
}

bring_up() {
  export FAKE_SCENARIO="${1:-normal}"
  export SYMBOLS="${2:-$BASE_SYMBOLS}"
  "${COMPOSE[@]}" up -d --build >/dev/null
  wait_health
}

verify() {
  local w="${1:-$WINDOW}"
  shift || true
  "${COMPOSE[@]}" exec -T collector /collector verify \
    --window-sec="$w" --fixtures --pause-sec="${VERIFY_PAUSE_SEC:-2}" "$@"
}

echo "metals-collector test matrix (started $START_TS)"

# ---- T1: unit -------------------------------------------------------------
if go test ./... >/tmp/metals-t1.log 2>&1; then
  record T1 1 "go test ./... passed"
else
  record T1 0 "go test ./... failed: $(tail -3 /tmp/metals-t1.log | tr '\n' ' ')"
fi

# ---- T2: normal pipeline --------------------------------------------------
bring_up normal
sleep $((WINDOW + 10))
if verify "$CHECK_WINDOW" >/tmp/metals-t2.log 2>&1; then
  record T2 1 "coverage >=99.5%/symbol, no duplicates"
else
  record T2 0 "$(grep -E 'FAIL' /tmp/metals-t2.log | tr '\n' ';' | head -c 400)"
fi

# ---- T3: fault scenarios --------------------------------------------------
for scenario in ban429 ban418 missing garbage timeout stale; do
  export FAKE_SCENARIO="$scenario"
  "${COMPOSE[@]}" up -d --force-recreate fake >/dev/null
  sleep 15
  healthy=0
  if curl -fsS --max-time 2 http://127.0.0.1:9090/healthz >/dev/null 2>&1; then healthy=1; fi
  bans="$(mget api_bans_total)"
  errs="$(metrics | grep -E '^api_errors_total\{' | awk '{s+=$NF} END {print s+0}')"
  ok=0
  case "$scenario" in
    ban429|ban418)
      awk -v b="$bans" 'BEGIN {exit !(b>0)}' && ok=1 ;;
    missing)
      ticks="$(mcontains 'ticks_total{symbol="XPDUSDT"}')"
      awk -v t="$ticks" 'BEGIN {exit !(t>0)}' && ok=1 ;;
    *)
      awk -v e="$errs" 'BEGIN {exit !(e>0)}' && ok=1 ;;
  esac
  ok=$((ok * healthy))
  record "T3/$scenario" "$ok" "healthy=$healthy bans=${bans:-0} errors=${errs:-0}"
done
bring_up normal
sleep 20

# ---- T4: DB outage --------------------------------------------------------
"${COMPOSE[@]}" stop db >/dev/null
sleep "$OUTAGE"
buffered_during="$(mget buffer_rows)"
dropped_during="$(mget dropped_ticks_total)"
"${COMPOSE[@]}" start db >/dev/null
for _ in $(seq 1 30); do
  if "${COMPOSE[@]}" exec -T db pg_isready -U collector -d collector >/dev/null 2>&1; then break; fi
  sleep 1
done
sleep 30
buffered_after="$(mget buffer_rows)"
dropped_after="$(mget dropped_ticks_total)"
ok=0
# A 2 s flush interval leaves at most one unflushed batch (4 symbols x 2 s).
awk -v b="$buffered_during" -v d="$dropped_during" -v ba="$buffered_after" -v da="$dropped_after" \
  'BEGIN {exit !(b>0 && d==0 && ba<=8 && da==0)}' && ok=1
record T4 "$ok" "buffered_during=${buffered_during:-0} buffered_after=${buffered_after:-0} dropped=${dropped_after:-0}"

# ---- T5: graceful stop ----------------------------------------------------
ticks_before="$(sum_ticks)"
"${COMPOSE[@]}" stop collector >/dev/null
rows_after="$(docker compose exec -T db psql -U collector -d collector -Atc \
  "SELECT count(*) FROM book_l1 WHERE ts_sec >= '$START_TS';" 2>/dev/null)"
ok=0
awk -v t="$ticks_before" -v r="$rows_after" 'BEGIN {exit !(r>=t-8 && r<=t+16)}' && ok=1
record T5 "$ok" "ticks_before=$ticks_before rows_after=$rows_after"
"${COMPOSE[@]}" start collector >/dev/null
wait_health

# ---- T6: SIGKILL ----------------------------------------------------------
"${COMPOSE[@]}" kill -s SIGKILL collector >/dev/null
"${COMPOSE[@]}" up -d collector >/dev/null
wait_health
sleep 20
dups="$(docker compose exec -T db psql -U collector -d collector -Atc \
  'SELECT count(*) FROM (SELECT 1 FROM book_l1 GROUP BY ts_sec,symbol HAVING count(*)>1) d;' 2>/dev/null)"
ok=0
awk -v d="${dups:-1}" 'BEGIN {exit !(d==0)}' && ok=1
record T6 "$ok" "duplicates_after_kill=${dups:-unknown}"

# ---- T7: retention --------------------------------------------------------
docker compose exec -T db psql -U collector -d collector -c \
  "CREATE TABLE IF NOT EXISTS book_l1_20250101 PARTITION OF book_l1 FOR VALUES FROM ('2025-01-01 00:00:00+00') TO ('2025-01-02 00:00:00+00');" >/dev/null 2>&1
"${COMPOSE[@]}" exec -T collector /collector retention --once >/dev/null 2>&1
aged="$(docker compose exec -T db psql -U collector -d collector -Atc \
  "SELECT count(*) FROM pg_tables WHERE tablename='book_l1_20250101';" 2>/dev/null)"
ok=0
awk -v a="${aged:-1}" 'BEGIN {exit !(a==0)}' && ok=1
record T7 "$ok" "aged_partitions_remaining=${aged:-unknown}"

# ---- T8: symbol extension -------------------------------------------------
EXT_SYMBOLS="$BASE_SYMBOLS,XCUUSDT"
bring_up normal "$EXT_SYMBOLS"
sleep $((WINDOW + 10))
if verify "$CHECK_WINDOW" >/tmp/metals-t8.log 2>&1; then
  record T8 1 "5th symbol present; budget assertion passed"
else
  record T8 0 "$(grep -E 'FAIL' /tmp/metals-t8.log | tr '\n' ';' | head -c 400)"
fi

"${COMPOSE[@]}" down -v >/dev/null 2>&1
if [ "$FAILED" -ne 0 ]; then
  echo "RESULT: FAIL"
  exit 1
fi
echo "RESULT: PASS (T1-T8)"
