#!/usr/bin/env bash
# Read-only pilot health check. Never touches the collector or the database.
# Usage: ./scripts/health_check.sh
set -uo pipefail

BASE="${RAWPROXY_URL:-https://p01--weather-api--cn5bkwlzyqr9.code.run}"
WINDOW="${HEALTH_WINDOW_SEC:-3600}"
OUT="${TMPDIR:-/tmp}/health_check_$$.json"
METRICS="${TMPDIR:-/tmp}/health_check_$$.txt"
trap 'rm -f "$OUT" "$METRICS"' EXIT

NOW=$(date +%s)
FROM=$((NOW - WINDOW))

health=$(curl -fsS --max-time 10 "$BASE/healthz" 2>/dev/null || echo "unavailable")
if [ "$health" != "ok" ]; then
  echo "FAIL: healthz = $health"
  exit 1
fi
curl -fsS --max-time 30 "$BASE/api/v1/stats?from=$FROM&to=$NOW" >"$OUT" || {
  echo "FAIL: /api/v1/stats unavailable"
  exit 1
}
curl -fsS --max-time 10 "$BASE/metrics" >"$METRICS" || {
  echo "FAIL: /metrics unavailable"
  exit 1
}

python3 - "$OUT" "$METRICS" "$FROM" "$NOW" <<'PY'
import json, re, sys
stats_path, metrics_path, start, now = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
d = json.load(open(stats_path))
metrics = open(metrics_path).read()

def metric(name):
    m = re.search(rf"^{re.escape(name)}(?:[^\s]*)\s+([0-9.eE+-]+)$", metrics, re.M)
    return float(m.group(1)) if m else -1

dropped = metric("dropped_ticks_total")
bans = metric("api_bans_total")
skew = metric("cycle_fetch_skew_ms")
buffered = metric("buffer_rows")
ticks = sum(float(m.group(1)) for m in re.finditer(r"^ticks_total\{[^}]*\}\s+([0-9.eE+-]+)$", metrics, re.M))

ok = (dropped == 0 and bans == 0 and 0 <= skew <= 250 and 0 <= buffered <= 16
      and ticks > 0 and d.get("future_ok", False) and not d.get("aged_partitions", []))
print(f"health=ok window={start}..{now} ({now-start}s)")
print(f"dropped={dropped:g} bans={bans:g} skew={skew:g}ms buffer={buffered:g} ticks={ticks:g}")
print("symbol          rows   coverage%  dup  bid<ask  maxgap")
for s in d["symbols"]:
    cov = 100 * s["rows"] / (now - start) if now > start else 0
    print(f"{s['symbol']:<12} {s['rows']:>7} {cov:>10.4f} {s['duplicates']:>4} "
          f"{s['bid_lt_ask_ratio']:>8.4f} {s['max_gap_sec']:>6}")
    ok = ok and cov >= 99.5 and s["duplicates"] == 0 and s["bid_lt_ask_ratio"] >= 0.99
print(f"partitions: future={d.get('future_partitions', 0)} ok={d.get('future_ok', False)} "
      f"aged={d.get('aged_partitions', [])}")
print("RESULT:", "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY
