# Pair Spread Viewer — Design Spec

**Status:** design frozen; API core implemented with hermetic contract tests;
page, DB smoke test and deployment pending.
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
The stability rules that keep future development out of the collector are in
§12.

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

Data only exists since the live pilot started (`2026-11-27T17:44:11Z`): `7d` and
`45d` are partial, and `1d`/`8h`/`1h` are still filling. Tune on LIVE → 1h → 8h
first. Retention keeps at least a full rolling 45-day window (§2.1), so the
oldest visible buckets are limited by pilot start, not by partition drops.

---

## 5. HTTP API

### 5.1 Transport

- Default listen address: `127.0.0.1:9091` (env `SPREAD_ADDR`). Loopback only in
  the local phase; do not expose 9091 publicly.
- CORS: `Access-Control-Allow-Origin: *`, `Access-Control-Allow-Methods: GET`,
  `Access-Control-Allow-Headers: Content-Type`. This permits opening the page
  directly from `file://` (origin `null`) as well as via a local static server.
- No authentication in the local phase (loopback only). No credentials ever
  reach the browser.
- Deployed phase (§7.3): the service must not be publicly exposed without
  protection — either an ingress SSO/credential policy or a bearer token
  required by the API — and `Access-Control-Allow-Origin: *` must be narrowed
  to the page's origin. The wildcard CORS is local-only.
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
    AND b.ts_sec >= $3 AND b.ts_sec < $4
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

The range predicate is repeated on both legs so partition pruning is explicit
on both sides of the join. Equivalence propagation through `USING (ts_sec)`
would usually infer it, but stating it is free and removes the dependency.

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

`latest` is a separate bounded single-row query:

```sql
SELECT extract(epoch FROM a.ts_sec)::bigint AS t,
       a.ask::double precision AS ask_a,
       b.ask::double precision AS ask_b,
       (a.ask / b.ask)::double precision AS ratio
FROM book_l1 a
JOIN book_l1 b USING (ts_sec)
WHERE a.symbol = $1 AND b.symbol = $2
  AND a.ts_sec > now() - interval '1 hour'
  AND a.ask > 0 AND b.ask > 0
ORDER BY a.ts_sec DESC
LIMIT 1;
```

The lower bound avoids a backward scan across all 45 daily partitions and
protects the 3 s live timeout. All queries must use `statement_timeout` via
context.

---

## 7. Service structure

### 7.1 Files

| path | purpose |
|---|---|
| `internal/spread/types.go` | request/response structs, pair definitions, presets |
| `internal/spread/store.go` | read-only SQL, row scanning, epoch conversions |
| `internal/spread/http.go` | handlers, cache, cursor logic; `RegisterAPI` mounts only the two pair paths |
| `internal/spread/*_test.go` | fixture-backed contract tests (no live DB) |
| `internal/rawproxy/*` | bounded rows/stats/sessions API (`MaxRows`, `MaxWindow`, symbol allow-list) |
| `cmd/collector/main.go` | mounts both APIs on the existing ops server |
| `tools/spread-view/index.html` | self-contained page |

The APIs are mounted on the collector's ops server under `/api/v1/` via
`ops.Server.Mount`. They use a dedicated `pgxpool` with `MaxConns = 2` so public
query traffic cannot starve the writer's pool. Only `DB_DSN`/`DATABASE_URL` from
the existing injected secret is needed; no second process, port, image or new
credential is deployed. All SQL is read-only: no `migrate.Run`, DDL, or writes.

### 7.2 Local run

```
go test ./...
go run ./cmd/collector run            # with DB_DSN and METRICS_ADDR set
open tools/spread-view/index.html?api=http://127.0.0.1:9090
```

### 7.3 Deployment (shipped)

`northflank.json` defines one service (`weather-api`) with GitOps linked,
service CI disabled (`disabledCI: true`, `ciIgnoreFlagsEnabled: true`) and
template autorun disabled (`autorun: false`). Builds and deploys are manual.

Port 9090 is public and unauthenticated; it now exposes the whole `/api/v1/*`
data surface, not just counters. An in-process token bucket limits general API
traffic (20 req/s, burst 40) and `/api/v1/stats` (1 req/s, burst 2). This is a
mitigation only: all clients behind the ingress share the bucket and the data
is readable by anyone with the URL. A dedicated read-only DB role and platform
basic-auth/private port remain deferred accepted risks (§10, §12.3).

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
- `scripts/test.sh:55` runs `go test ./...`, so any test under `internal/spread`
  lands inside the **pilot T1 acceptance gate**. Spread tests must be hermetic,
  deterministic and sub-second; never skip on a missing DSN and never call the
  deployed DB. Put DSN-dependent checks in a separate local smoke script.
- Do **not** modify `scripts/test.sh` or any pilot acceptance test.

---

## 10. Risks and mitigations

| Risk | Mitigation |
|---|---|
| 45D query scans ~15.5 M rows across 45 partitions | On-demand only; 60 s cache; 10 s context timeout; PK `(ts_sec, symbol)` supports the join |
| Ask-to-ask differs from mid-to-mid | Documented in §2.4; accepted |
| `ln(avg r)` differs from `avg(ln r)` | Documented in §3.4; optional `avg(ln r)` field later |
| Browser local time + DST | Documented in §3.5; epoch seconds remain the wire format |
| Reading pilot data from a local process | All API SQL is read-only; dedicated 2-connection pool; no writes or DDL |
| Spread tests join pilot T1 (`go test ./...`) | Hermetic, sub-second, no DSN skips; DB checks only in the local smoke script |
| Public unauthenticated port 9090 | Rate-limited (20 req/s burst 40; `/stats` 1 req/s burst 2) with a dedicated API pool. Accepted risk: anyone with the URL can read bounded L1 data; platform auth/private port deferred (§7.3) |
| Last-stage-default Dockerfile build | No second Dockerfile; both APIs are compiled into the collector binary (§7.3) |
| Manual deploy only (`disabledCI: true`, `autorun: false`) | Defer Dockerfile/Northflank changes until after tuning; applying the template change itself may cause one final run |
| Repeated heavy polling | Cursor-based incremental query with `LIMIT 500`, no cache on live |

---

## 11. Implementation order

1. `internal/spread` types, presets, SQL store, HTTP handlers, unit tests.
2. `tools/spread-view/index.html` single-file page.
3. Both APIs mounted on the collector's ops server with a dedicated pool.
4. Local smoke tests against a temporary Postgres and fake exchange.
5. Manual build/deploy of the single `weather-api` service.

The collector was changed once by explicit decision to host the APIs (§7.1),
deviating from the original freeze. The §12 guardrails still apply: bounded
pool and timeouts, hermetic contract tests, API health/metrics, no schema or
pipeline changes, and no further collector changes without sign-off.

---

## 12. Stability foundation

Decided 2026-11-28. Purpose: keep future development from causing collector
restarts and therefore data gaps.

### 12.1 Freeze policy

- The collector was frozen from 2026-11-28. One explicit exception was granted
  to mount the read-only APIs on its existing ops server (§7.1) after the
  single-service limit made a second deployment impossible. That change added
  a dedicated 2-connection API pool, a bounded ring, a flush timeout and rate
  limits; it did not change the schema, writer semantics or retention.
- From now on the collector is frozen again. Any further collector change,
  including cosmetic or refactor work, requires explicit sign-off, a scheduled
  window, and a verification run afterwards. Feature work belongs in consumers.
- Budget: ≥99.5 % coverage per symbol-day allows at most
  `86400 × 0.005 = 432 s` (7.2 min) of gap per day. One redeploy costs roughly
  20–40 s (container swap, pool connect, `migrate`, `Ensure`, `AssertStartup`
  with up to 15 one-second retries). Frequent deploys are the failure mode.
- All change frequency moves to the page and the API consumers; neither can
  write to `book_l1`.

### 12.2 API contract

- The stable v1 surface is `/api/v1/spread` and `/api/v1/spread/live`, plus the
  read-only data endpoints `/api/v1/rows`, `/api/v1/stats` and
  `/api/v1/sessions` added later through the change procedure below.
- Versions live in the path (`/api/v1/...`). **Breaking changes are allowed**,
  but only through a coordinated upgrade:
  1. record the intended change in the change log (§12.5);
  2. update the APIs, the page and any other consumers in the same change;
  3. update the golden contract fixtures/tests in the same commit;
  4. run hermetic tests plus the local smoke test;
  5. manually build and deploy the single `weather-api` service.
- If any consumer cannot be upgraded in the same change, do **not** break v1 —
  publish the new shape under a new path version instead.
- Silent changes (response shape, field type, units, timestamps, error codes)
  are forbidden. Consumers may ignore unknown fields, but must never guess at
  changed ones.
- The storage contract (`book_l1`, `ts_sec` alignment, PK, retention) is part of
  the freeze: no schema or pipeline changes for this work.

### 12.3 Guardrails (all required)

- **Read-only DB access:** all API SQL is read-only; no `migrate.Run`, DDL, or
  writes. The dedicated `spread_ro` role was deferred: the APIs currently use
  the injected `DB_DSN`. This is an accepted, documented risk (§7.3, §10).
- **Query isolation:** a dedicated pgx pool (`MaxConns = 2`, separate from the
  writer pool), timeouts via context (10 s history, 3 s live, 10 s stats,
  3 s flush), explicit time bounds on every query (including `latest`),
  both-leg range predicates, and the 60 s history cache. Public queries must
  not starve the collector's writer.
- **Golden contract tests:** hermetic, sub-second, DSN-free tests under
  `internal/spread` and `internal/rawproxy` that assert the exact v1 JSON
  shapes with fixed fixture rows. They run inside pilot T1 (`go test ./...`)
  and must be updated in the same commit as an intentional breaking change.
- **API health and metrics:** collector `/healthz` and `/metrics`; API counters
  at `/api/v1/rawproxy-metrics` and `/api/v1/spread-metrics`, mirrored into
  the collector registry so `scripts/test.sh` can read them.

### 12.4 Deployment workflow

- One service, `weather-api`: manual build/deploy only, in a scheduled window,
  after verification. CI and template autorun remain disabled
  (`disabledCI: true`, `autorun: false`).
- Page: a local file in `tools/spread-view/`; change as often as needed, no
  deployment or restart involved.
- No second image, service or Dockerfile is part of v1.
### 12.5 Change log

| Date | Change | Contract impact |
|---|---|---|
| 2026-11-28 | Foundation defined: collector frozen, v1 = spread endpoints only, coordinated-breaking-change policy, guardrails above | none — API not yet implemented |
| 2026-11-28 | APIs mounted on collector ops server with dedicated pool; single-service deployment; rate limits | additive — existing v1 shapes unchanged |
| 2026-11-28 | Added `/api/v1/rows`, `/api/v1/stats`, `/api/v1/sessions` and API metrics endpoints | additive |
| 2026-11-28 | Review fixes: separate API pool, bounded ring, flush timeout, shutdown order, rate limiting | none |
