#!/usr/bin/env bash
# T9 for the currently deployed build. Pass the build's start epoch seconds.
# Usage: ./scripts/t9_build.sh <build-start-epoch>
set -uo pipefail

BASE="${RAWPROXY_URL:-https://p01--weather-api--cn5bkwlzyqr9.code.run}"
START="${1:?build start epoch required}"
NOW=$(date +%s)
FROM=$((START + 1))
OUT="${TMPDIR:-/tmp}/t9_build_$$.json"
METRICS="${TMPDIR:-/tmp}/t9_build_$$.txt"
trap 'rm -f "$OUT" "$METRICS"' EXIT

curl -fsS --max-time 30 "$BASE/api/v1/stats?from=$FROM&to=$NOW" >"$OUT" || {
  echo "FAIL: /api/v1/stats unavailable"
  exit 1
}
curl -fsS --max-time 10 "$BASE/metrics" >"$METRICS" || {
  echo "FAIL: /metrics unavailable"
  exit 1
}

python3 - "$OUT" "$METRICS" "$FROM" "$NOW" <<'PY'
import json, sys, re
stats_path, metrics_path, start, now = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
d = json.load(open(stats_path))
metrics = open(metrics_path).read()

def metric(name):
    m = re.search(rf"^{re.escape(name)}(?:[^\s]*)\s+([0-9.eE+-]+)$", metrics, re.M)
    return float(m.group(1)) if m else -1

dropped = metric("dropped_ticks_total")
skew = metric("cycle_fetch_skew_ms")
bans = metric("api_bans_total")
window = now - start
print(f"build_start={start} now={now} window_sec={window}")
print(f"dropped_ticks_total={dropped:g} cycle_fetch_skew_ms={skew:g} api_bans_total={bans:g}")

ok = dropped == 0 and d.get("future_ok", False) and not d.get("aged_partitions", [])
print("symbol          rows   coverage%  dup  bid<ask  maxgap")
for s in d["symbols"]:
    cov = 100 * s["rows"] / window if window > 0 else 0
    print(f"{s['symbol']:<12} {s['rows']:>7} {cov:>10.4f} {s['duplicates']:>4} "
          f"{s['bid_lt_ask_ratio']:>8.4f} {s['max_gap_sec']:>6}")
    ok = ok and cov >= 99.5 and s["duplicates"] == 0 and s["bid_lt_ask_ratio"] >= 0.99
print(f"partitions: future={d.get('future_partitions', 0)} ok={d.get('future_ok', False)} "
      f"aged={d.get('aged_partitions', [])}")
print("RESULT:", "PASS" if ok else "FAIL")
sys.exit(0 if ok else 1)
PY
