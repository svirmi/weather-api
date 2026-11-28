# Pair Spread Viewer — Design Spec

**Status:** design frozen, implementation not started.
**Date:** 2026-11-28.
**Owner:** local testing/tuning tool for the metals L1 pilot.

---

## 1. Goal and scope

Build a local HTML + JavaScript page that visualizes **ask-to-ask ratios** between
the two pairs captured by the 1 Hz L1 pilot:

- **Gold / Silver:** `XAUUSDT / XAGUSDT`
- **Platinum / Palladium:** `XPTUSDT / XPDUSDT`

The pilot's deployed Go collector is the data owner, but this tool must not modify
or endanger it. Data is served by a **new small read-only Go service** that is run
locally first and deployed as a separate container only after the page is tuned.

### In scope

- Ask-to-ask ratio series for the two pairs.
- Fixed range presets (45D, 7D, 1D, 8H, 1H) plus a polling LIVE view.
- Ratio as the primary chart, `ln(ratio)` as a subplot.
- Bucket min/max band, gap breaks, latest-ask cards, pair toggle.
- Local page with no CDN, no build step, no dependencies.

### Explicitly out of scope (v1)

- Mid-price or bid-based ratios (ask-to-ask only).
- Trade prints, traded volume, order-book depth beyond L1.
- Session-window shading / NO_TRADING overlays.
- Alerts, thresholds other than the red fetch-failure state, persistence.
- Authentication in the local phase; changes to the pilot collector or its
  `/healthz` and `/metrics` endpoints.
- Schema, retention, or pipeline changes.

---

## 2. Source data and invariants

### 2.1 Table

`book_l1` (`internal/migrate/schema.sql`):

| column | type | meaning |
|---|---|---|
| `ts_sec` | `timestamptz` | UTC second-truncated **cycle boundary**; PK with `symbol` |
| `symbol` | `text` | one of `XAUUSDT`, `XAGUSDT`, `XPTUSDT`, `XPDUSDT` |
| `exch_time` | `timestamptz` | exchange bookTicker transaction time; can be ~14–75 s stale for XPT |
| `bid`, `bid_qty`, `ask`, `ask_qty` | `numeric(20,8)` | resting L1 top-of-book price and size |

PK `(ts_sec, symbol)` guarantees one row per symbol per second. Retention is 45
days with daily partitions, so all presets fit inside retained data.

### 2.2 Alignment rule

The two legs of a pair are fetched concurrently in the same collector cycle
(observed `cycle_fetch_skew_ms = 4`). Therefore:

> **Join the two symbols on `ts_sec`, never on `exch_time`.**

`exch_time` is a payload field with per-instrument staleness; joining on it would
misalign the legs and create artificial spikes.

### 2.3 Gaps

Missing cycles leave absent seconds. Missing buckets are **omitted** from the API
response; the page breaks the line at gaps and **never interpolates**. The bucket
`n` (sample count) is returned so partial buckets can be inspected.

### 2.4 Price convention

- The plotted value is `ask_num / ask_den` (best offer of the numerator divided
  by best offer of the denominator), requiring both asks to be positive.
- Ask-to-ask is the executable cross rate. It sits slightly above a mid-to-mid
  ratio — by roughly half of the two quoted spreads. This bias is accepted by
  design.
- No trade data exists; `ask` is a resting quote, not a fill.

### 2.5 Volume and precision

- Raw volume: 4 symbols × 1 Hz × 45 d ≈ 15.5 M rows. Raw rows are never sent to
  the browser; all downsampling happens in SQL.
- `numeric(20,8)` values are cast to `double precision` in the API. Resulting
  values (~10² or less with ~8 significant digits) are safe as JavaScript
  `Number`.

---

## 3. Definitions

### 3.1 Pair identifiers

| API `pair` | Numerator | Denominator | UI label |
|---|---|---|---|
| `XAU-XAG` | `XAUUSDT` | `XAGUSDT` | Gold / Silver |
| `XPT-XPD` | `XPTUSDT` | `XPDUSDT` | Platinum / Palladium |

The first symbol is always the numerator, matching the labels in the sketch.

### 3.2 Per-second ratio

```
r(t) = ask_num(t) / ask_den(t)
```

computed only for seconds where both legs exist and both asks are `> 0`.

### 3.3 Bucket aggregate (decision: average of 1 Hz ratios)

For a bucket of `B` seconds aligned to the UTC epoch:

```
n      = count(r)
ratio  = avg(r)
lo     = min(r)
hi     = max(r)
last   = r at the largest ts_sec in the bucket
askNum = avg(ask_num)
askDen = avg(ask_den)
```

All aggregates are cast to `double precision`. Bucket timestamps are returned as
**epoch seconds**.

This is `avg(askA/askB)`, **not** `avg(askA) / avg(askB)`. The two are identical
for 1-second buckets and differ only slightly at longer buckets.

### 3.4 Log subplot

```
logRatio = ln(ratio)
```

computed **in the browser** from the returned `ratio`. Note that
`ln(avg(r)) != avg(ln(r))`; the difference is second-order at these spreads and
acceptable for visualization. If rigorous deviation statistics are needed later,
the API can add `avg(ln(r))` without changing the contract.

### 3.5 Time zone

The API returns UTC epoch seconds. The page renders the x-axis and tooltips in
**browser local time**. DST shifts across a 45-day range are accepted.

---

## 4. Range presets

Only these exact `range` values are accepted; no arbitrary `from`/`to`/`bucket`
parameters in v1.

| `range` | Window | Bucket | Max points |
|---|---|---|---|
| `45d` | 45 days | 15 min | 4,320 |
| `7d` | 7 days | 5 min | 2,016 |
| `1d` | 24 h | 1 min | 1,440 |
| `8h` | 8 h | 15 s | 1,920 |
| `1h` | 1 h | 1 s | 3,600 |
| LIVE | last 300 s | 1 s | 300 + poll |

LIVE uses the live endpoint, not a `range` value. The default selected range on
page load is **LIVE** (matching the sketch).

---

## 5. HTTP API

### 5.1 Transport

- Default listen address: `127.0.0.1:9091` (env `SPREAD_ADDR`). Loopback only in
  the local phase; do not expose 9091 publicly.
- CORS: `Access-Control-Allow-Origin: *`, `Access-Control-Allow-Methods: GET`,
  `Access-Control-Allow-Headers: Content-Type`. This permits opening the page
  directly from `file://` (origin `null`) as well as via a local static server.
- No authentication in the local phase. No credentials ever reach the browser.
- Request context timeouts: 10 s for history, 3 s for live. Live returns at most
  500 points per call.

### 5.2 `GET /api/v1/spread`

Query parameters:

- `pair` (required): `XAU-XAG` or `XPT-XPD`.
- `range` (required): one of `45d`, `7d`, `1d`, `8h`, `1h`.

Response `200`:

```json
{
  "pair": "XAU-XAG",
  "range": "1h",
  "bucketSec": 1,
  "asOf": 1764340800,
  "points": [
    {
      "t": 1764340800,
      "n": 1,
      "ratio": 85.123456,
      "lo": 85.123456,
      "hi": 85.123456,
      "last": 85.123456,
      "askA": 4201.35,
      "askB": 49.36
    }
  ]
}
```

An empty range returns `200` with `"points": []` (the page shows
"no data in range").

### 5.3 `GET /api/v1/spread/live`

Query parameters:

- `pair` (required): as above.
- `after` (optional): epoch **seconds** cursor. Default: `now - 300`.

Response `200`:

```json
{
  "pair": "XAU-XAG",
  "cursor": 1764340800,
  "pollMs": 1000,
  "latest": {
    "t": 1764340800,
    "askA": 4201.35,
    "askB": 49.36,
    "ratio": 85.123456
  },
  "points": [
    { "t": 1764340800, "askA": 4201.35, "askB": 49.36, "ratio": 85.123456 }
  ]
}
```

- `cursor` is the largest `t` returned; when no new rows exist it equals `after`,
  and `points` is empty.
- The client polls `after=<cursor>` every `pollMs` (default 1000 ms from the
  server response).
- `latest` is the most recent row at or before the current time, used to update
  the cards. If no row exists, `latest` is `null`.

### 5.4 `GET /healthz`

Returns `ok\n` with `200`. Independent of the presence of cached data; reports
DB failure only if a query is attempted and fails (default: always ok when the
process is up).

### 5.5 Errors

`400` for unknown pair/range or malformed `after`; `500` for DB failures. Body:

```json
{"error": "unknown range: 30d"}
```

### 5.6 Caching

- History responses are cached in memory per `(pair, range)` for **60 s**
  (default), bounded to the 10 possible keys.
- Live responses are never cached.
- Cache hit does not touch the DB; this makes repeated UI tuning fast.

---

## 6. SQL

History (parameters: `$1` numerator symbol, `$2` denominator symbol, `$3` from
`timestamptz`, `$4` to `timestamptz`, `$5` bucket interval):

```sql
WITH legs AS (
  SELECT a.ts_sec,
         a.ask AS ask_a,
         b.ask AS ask_b,
         a.ask / b.ask AS r
  FROM book_l1 a
  JOIN book_l1 b USING (ts_sec)
  WHERE a.symbol = $1 AND b.symbol = $2
    AND a.ts_sec >= $3 AND a.ts_sec < $4
    AND a.ask > 0 AND b.ask > 0
)
SELECT extract(epoch FROM date_bin($5::interval, ts_sec, to_timestamp(0)))::bigint AS t,
       count(*)::int AS n,
       avg(r)::double precision AS ratio,
       min(r)::double precision AS lo,
       max(r)::double precision AS hi,
       (array_agg(r ORDER BY ts_sec DESC))[1]::double precision AS last,
       avg(ask_a)::double precision AS ask_a,
       avg(ask_b)::double precision AS ask_b
FROM legs
GROUP BY 1
ORDER BY 1;
```

Live (parameters: `$1` numerator, `$2` denominator, `$3` cursor epoch seconds):

```sql
SELECT extract(epoch FROM a.ts_sec)::bigint AS t,
       a.ask::double precision AS ask_a,
       b.ask::double precision AS ask_b,
       (a.ask / b.ask)::double precision AS ratio
FROM book_l1 a
JOIN book_l1 b USING (ts_sec)
WHERE a.symbol = $1 AND b.symbol = $2
  AND a.ts_sec > to_timestamp($3)
  AND a.ask > 0 AND b.ask > 0
ORDER BY a.ts_sec
LIMIT 500;
```

`latest` is a separate single-row query using `ORDER BY ts_sec DESC LIMIT 1`
with no lower bound. All queries must use `statement_timeout` via context.

---

## 7. Service structure

### 7.1 Files

| path | purpose |
|---|---|
| `internal/spread/types.go` | request/response structs, pair definitions, presets |
| `internal/spread/store.go` | pgx queries above, row scanning, epoch conversions |
| `internal/spread/http.go` | handlers, CORS, validation, cache, cursor logic |
| `internal/spread/*_test.go` | unit tests (no live DB required) |
| `cmd/spreadapi/main.go` | config, pgx pool, HTTP server, signal handling, slog logging |

Configuration is read from `DB_DSN` (or `DATABASE_URL` alias, as in
`internal/config`) and `SPREAD_ADDR` (default `127.0.0.1:9091`). No new
credentials or config files in the repo.

The service uses a separate `pgxpool`; it does not import or modify the
collector's pipeline, ops server, writer, or coordinator. Shutdown on SIGINT /
SIGTERM closes the HTTP server and pool.

### 7.2 Local run

```
DB_DSN='postgres://...' go run ./cmd/spreadapi
curl 'http://127.0.0.1:9091/api/v1/spread?pair=XAU-XAG&range=1h'
```

### 7.3 Later deployment (after tuning)

- Add a second build target for `cmd/spreadapi` in the existing Dockerfile's
  `build` stage and a distroless runtime stage.
- Add a second Northflank service on its own port/ingress, linked to the same
  Postgres addon secret, with a **read-only DB role** (`SELECT` only).
- Do not commit a changed `northflank.json` until the local version is validated:
  GitOps tracks `main` and commits deploy automatically.

---

## 8. Page design

### 8.1 File

`tools/spread-view/index.html` — one self-contained file with inline CSS and
JavaScript, plain `<canvas>` rendering, no CDN, no build step. Open via
`file://` or a local static server (CORS allows both).

### 8.2 Layout (matches the sketch)

1. Header: title `Pair Spread · 1 Hz L1 feed`; pair toggle buttons
   `Gold / Silver` and `Platinum / Palladium`; status dot at top right.
2. A compact current-ratio badge for the active pair, e.g. `XAU/XAG 85.123456`.
3. Four cards: `XAUUSDT`, `XAGUSDT`, `XPTUSDT`, `XPDUSDT`. Each shows the latest
   **ask** and its age from `ts_sec`; empty state shows `no data` /
   `no rows in table`.
4. Chart panel: range buttons `45D 7D 1D 8H 1H LIVE`, main ratio line with
   min/max band, then a smaller `ln(ratio)` subplot sharing the x-axis.
5. Tooltip on hover: local time, ratio, log ratio, lo/hi, both asks, `n`.

### 8.3 Behavior

- Switching pair or range fetches the corresponding series (LIVE starts from
  the last 300 s, then polls).
- In LIVE mode, `latest` from each poll updates the cards and ratio badge; in
  history mode the cards use the last point of the loaded series.
- Gap detection: if `t[i] - t[i-1] > 1.5 × bucketSec`, start a new path segment
  so the line is broken; never draw across missing data.
- Red status dot with `spread fetch failed` on HTTP/parse failure; `no data in
  range` when the response is empty. No amber/stale state in v1.
- Canvas is scaled for `devicePixelRatio` and redrawn on window resize.
- Formatting defaults: ratio with 6 decimals, log with 4 decimals, "nice" axis
  ticks, UTC epoch converted to browser local time for display.

### 8.4 Defaults chosen here (not prior decisions)

- Default range on load: **LIVE**.
- Ratio 6 decimals, log 4 decimals.
- Gap threshold: 1.5 × bucket duration.
- Poll interval: taken from `pollMs` in the response (1000 ms default).

---

## 9. Testing plan

- `go test ./internal/spread/...`: pair/preset validation, bucket boundary and
  epoch conversion, ratio/log math, gap detection helper, cursor handling,
  JSON marshalling, cache expiry with an injectable clock.
- Local smoke test: run the service against the pilot DB with a read-only DSN;
  `curl` each endpoint for both pairs and all ranges; verify counts, gap
  behavior, empty-range handling, and CORS headers.
- Page smoke test: open `tools/spread-view/index.html`, exercise pair toggle,
  all ranges, hover tooltip, LIVE polling, and both error states.
- Do **not** modify `scripts/test.sh` or any pilot acceptance test.

---

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| 45D query scans ~15.5 M rows across 45 partitions | On-demand only; 60 s cache; 10 s context timeout; PK `(ts_sec, symbol)` supports the join |
| Ask-to-ask differs from mid-to-mid | Documented in §2.4; accepted |
| `ln(avg r)` differs from `avg(ln r)` | Documented in §3.4; optional `avg(ln r)` field later |
| Browser local time + DST | Documented in §3.5; epoch seconds remain the wire format |
| Reading pilot data from a local process | Read-only role before any deployment; no writes in SQL |
| GitOps deploys on commit to `main` | Defer Dockerfile/Northflank changes until after tuning |
| Repeated heavy polling | Cursor-based incremental query with `LIMIT 500`, no cache on live |

---

## 11. Implementation order

1. `internal/spread` types, presets, SQL store, HTTP handlers, unit tests.
2. `cmd/spreadapi` main with config, pool, server, graceful shutdown.
3. `tools/spread-view/index.html` single-file page.
4. Local smoke tests against the deployed DB.
5. Only after validation: Dockerfile target, read-only DB role, Northflank
   service.

No step modifies the pilot collector, schema, retention, or acceptance tests.
