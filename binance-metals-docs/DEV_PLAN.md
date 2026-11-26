# DEV_PLAN.md — Metals L1 Collector v1.0 (pilot)

## 0. Scope
Poll Binance USDT-M futures L1 (best bid/ask + **quoted size at those levels**) at 1 Hz
for XPTUSDT, XPDUSDT, XAUUSDT, XAGUSDT. Persist to PostgreSQL. Purge >45 days.
Whole stack in Docker, `docker compose up`. Go single binary.

In scope: poll, persist, purge, verify, graceful shutdown.
Out of scope v1.0: order placement, other venues, historical backfill, alerting stack,
mark/index/funding, sub-second resolution.

**"Volume at L1" = `bidQty` / `askQty` (resting size), NOT traded volume.**
Traded volume would need WS `@aggTrade`; REST `aggTrades` is weight 20 → 4800/min at
1 Hz × 4 = 200% of budget. Not viable. Do not substitute.

## 1. Key decisions

| Decision | Rationale |
|---|---|
| Batched `INSERT … ON CONFLICT DO NOTHING`, **not COPY** | COPY has no `ON CONFLICT` clause; at 4 rows/s a 2 s flush is 8 rows. COPY buys nothing and breaks idempotency (FR-3). |
| One coordinator, second-aligned, 4 **concurrent** per-symbol GETs | Independent per-symbol tickers do not produce comparable timestamps. Concurrency from one cycle boundary minimises skew; `ts_sec` is then the cycle key. |
| Per-symbol requests, not the all-symbol call | All-symbol is weight 5 flat (cheaper at N≥4) but ~60–100 KB/response ≈ 7–8 GB/day. Reject on **bandwidth**, not weight. |
| Prices/sizes stay **strings** end-to-end, cast `$n::numeric` in SQL | Exact decimal, zero float contamination, no decimal dependency. |
| `numeric(20,8)`, no generated `spread` column | `ask−bid` is the bid-ask spread, not the pairs spread. Derive in a view; saves ~11 B/row. |
| Daily range partitions, purge = `DROP TABLE` partition | Instant, no bloat, no bulk DELETE path. |
| In-memory bounded ring buffer only | Pilot accepts ≤10 min loss on DB outage and ≤2 s on SIGKILL. No disk WAL in v1.0. |
| Session windows in a side table, not a per-tick flag | `tradingSchedule` polled hourly; join at read time. Keeps tick rows lean. |
| Symbols extensible via env; **removal requires volume wipe** | Adding a symbol needs no DDL. Removing leaves orphan rows → `down -v && up`. Accepted. |
| Built-in `fake` + `verify` subcommands | Whole pipeline testable offline; fault drills reproducible; no rate-limit risk. |

## 2. API contract

Base `https://fapi.binance.com`. Public, no API key.

| Purpose | Endpoint | Weight | Cadence |
|---|---|---|---|
| L1 quote | `GET /fapi/v1/ticker/bookTicker?symbol=XPTUSDT` | **2** (5 if `symbol` omitted) | 1 s × N |
| Startup: tickSize/stepSize + rateLimits | `GET /fapi/v1/exchangeInfo` | 1 | once |
| Clock drift | `GET /fapi/v1/time` | 1 | 60 s |
| Commodity session windows | `GET /fapi/v1/tradingSchedule` | 5 | 1 h |

`bookTicker` response (single-symbol variant):
`symbol`, `bidPrice`, `bidQty`, `askPrice`, `askQty`, `time`.

- Prices/qty are **JSON strings**. Parse as strings, never float.
- `time` is documented as **Transaction time** (not "event time"). Name the column `exch_time`.
- **RPI orders are excluded** from this response → L1 size can understate the true touch. Known limitation.
- No sequence/`pu` field → no gap detection from the payload. Not needed for polling.

**Weight budget** (IP `REQUEST_WEIGHT` 2400/min, confirmed from `exchangeInfo.rateLimits`):

~~~
bookTicker  4 × 2 × 60      = 480/min  (20.0%)
time        1 × 1           =   1/min
tradingSchedule 5 / 60      ≈   0.1/min
                       total ≈ 481/min
~~~

Scales linearly: `weight/min ≈ N × 120`. **Assert `N ≤ 16` at startup** and fail fast above it.
On **429/418: pause ALL symbols** for `Retry-After` seconds. Limits and bans are IP-scoped;
pausing one symbol leaves the others extending the ban.

## 3. Data model

~~~sql
CREATE TABLE IF NOT EXISTS book_l1 (
  ts_sec    timestamptz   NOT NULL,          -- cycle boundary, UTC, second-truncated
  symbol    text          NOT NULL,
  exch_time timestamptz   NOT NULL,          -- bookTicker 'time'
  bid       numeric(20,8) NOT NULL,
  bid_qty   numeric(20,8) NOT NULL,
  ask       numeric(20,8) NOT NULL,
  ask_qty   numeric(20,8) NOT NULL,
  PRIMARY KEY (ts_sec, symbol)
) PARTITION BY RANGE (ts_sec);

CREATE TABLE IF NOT EXISTS session_window (
  start_time timestamptz PRIMARY KEY,
  end_time   timestamptz NOT NULL,
  kind       text        NOT NULL            -- REGULAR | NO_TRADING
);

CREATE OR REPLACE VIEW book_l1_spread AS
SELECT ts_sec, symbol, bid, ask, bid_qty, ask_qty,
       ask - bid AS spread,
       (bid + ask) / 2 AS mid
FROM book_l1;
~~~

PK is `(ts_sec, symbol)` — cross-symbol reads at aligned seconds are the point of the
system. Includes the partition key, so the unique index is valid on a partitioned table.

Write path (one transaction per flush, pgx batch):

~~~sql
INSERT INTO book_l1 (ts_sec, symbol, exch_time, bid, bid_qty, ask, ask_qty)
VALUES ($1,$2,$3,$4::numeric,$5::numeric,$6::numeric,$7::numeric)
ON CONFLICT (ts_sec, symbol) DO NOTHING;
~~~

Partitions: `book_l1_YYYYMMDD`, one per UTC day. Maintain 7 ahead; drop older than
`RETENTION_DAYS`. Create-on-demand repair path if an insert hits a missing day.

Skew metric per cycle: `max(exch_time) − min(exch_time)` across symbols. Export it; it is
the evidence that legs are comparable.

Capacity: ~99 B/row → 345,600 rows/day → 15.55 M rows/45 d → ~1.5 GB heap + ~0.5 GB index
≈ **2 GB steady state**.

## 4. Architecture

~~~
coordinator (1 Hz, aligned to second boundary)
  ├─ fan out N concurrent GET bookTicker (errgroup, 3 s total timeout)
  ├─ slow cycle → SKIP next, never burst
  └─ rows → buffered channel
writer: flush every 2 s OR 64 rows, one txn, ON CONFLICT DO NOTHING
  └─ DB error → bounded ring (BUFFER_MAX_MINUTES); retry next flush; drop oldest + counter
nightly: partition manager (create ahead / drop aged)
hourly:  tradingSchedule → session_window upsert
ops:     /healthz, /metrics (plain text), daily completeness report
~~~

One shared `http.Client`, keep-alive, `MaxIdleConnsPerHost: 8`, 2 s connect / 3 s total.
`pgxpool` max 4 conns. No broker, no intermediate storage.

### Graceful shutdown
The trap: **never flush with the cancelled context.**

~~~go
ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
defer stop()
<-ctx.Done()

sched.Stop()                       // stop intake; in-flight bounded by 3 s HTTP timeout

fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()                     // FRESH context — ctx is already cancelled
if err := writer.Flush(fctx); err != nil {
    log.Error("final flush failed", "err", err, "buffered", writer.Buffered())
}
_ = opsSrv.Shutdown(fctx)
pool.Close()
~~~

Order: stop intake → drain in-flight → flush → close HTTP → close pool.
`stop_grace_period: 30s` covers 3 s + 10 s. SIGKILL loses ≤1 unflushed batch (≤2 s).

## 5. Configuration (env only)

| Var | Default | Note |
|---|---|---|
| `SYMBOLS` | `XPTUSDT,XPDUSDT,XAUUSDT,XAGUSDT` | adding needs no DDL; removing → volume wipe |
| `POLL_INTERVAL_MS` | `1000` | |
| `BINANCE_BASE_URL` | `https://fapi.binance.com` | point at `fake` for tests |
| `DB_DSN` | required | |
| `FLUSH_INTERVAL_MS` | `2000` | |
| `BUFFER_MAX_MINUTES` | `10` | |
| `HTTP_TIMEOUT_MS` | `3000` | |
| `RETENTION_DAYS` | `45` | |
| `PARTITION_AHEAD_DAYS` | `7` | |
| `METRICS_ADDR` | `:9090` | |
| `LOG_LEVEL` | `info` | |

Startup asserts: parse `exchangeInfo` for tick/step per symbol; read `rateLimits`;
compute weight budget; fail fast if over limit or a symbol is absent.

## 6. Docker

`Dockerfile` — multi-stage → distroless. **Distroless has no shell/curl**, so the
healthcheck must be a subcommand of the binary.

~~~dockerfile
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/collector ./cmd/collector

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /collector
EXPOSE 9090
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["/collector","healthcheck"]
ENTRYPOINT ["/collector"]
~~~

Subcommands: `run` (default), `migrate`, `healthcheck`, `fake`, `verify`, `retention --once`, `report`.

`docker-compose.yml` — pilot, everything local:

~~~yaml
services:
  db:
    image: postgres:16-alpine
    environment:
      POSTGRES_USER: collector
      POSTGRES_PASSWORD: collector
      POSTGRES_DB: collector
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL","pg_isready -U collector -d collector"]
      interval: 5s
      timeout: 3s
      retries: 10

  collector:
    build: .
    depends_on:
      db: { condition: service_healthy }
    environment:
      DB_DSN: postgres://collector:collector@db:5432/collector?sslmode=disable
      SYMBOLS: XPTUSDT,XPDUSDT,XAUUSDT,XAGUSDT
    command: ["sh","-c","/collector migrate && /collector run"]
    ports: ["127.0.0.1:9090:9090"]
    restart: unless-stopped
    stop_grace_period: 30s
    init: true

volumes:
  pgdata:
~~~

Note: distroless has no `sh`. Use a tiny `entrypoint` subcommand instead:
`command: ["bootstrap"]` where `bootstrap` = migrate-then-run in-process. Do not ship
the `sh -c` form above into the distroless image; it is shown only to make the intent explicit.

`docker-compose.test.yml` — override adding the fake and pointing the collector at it:

~~~yaml
services:
  fake:
    build: .
    command: ["fake","--addr=:8080","--scenario=normal"]
  collector:
    environment:
      BINANCE_BASE_URL: http://fake:8080
      RETENTION_DAYS: "45"
~~~

## 7. Testing — simplest path that covers every component

**Principle: every component except the real network is tested offline against `fake`.**
Fault drills become deterministic and repeatable; no rate-limit exposure.

### Built-in fake
`collector fake --addr=:8080 --scenario=<name>` serves recorded fixtures at the exact
paths and shapes in §2. Scenarios:

| Scenario | Behaviour |
|---|---|
| `normal` | valid fixtures, advancing `time` |
| `ban429` | 429 + `Retry-After: 5` every 10th call |
| `ban418` | 418 once, then normal |
| `timeout` | sleep 5 s (exceeds client timeout) |
| `garbage` | malformed JSON |
| `missing` | one symbol absent from response |
| `stale` | `time` frozen 30 s behind |

### Built-in verifier
`collector verify` runs SQL assertions against `DB_DSN`, prints a pass/fail table,
exits non-zero on any failure. Checks:

1. rows present for every configured symbol in the last N seconds
2. zero duplicate `(ts_sec, symbol)`
3. tomorrow's partition exists; ≥`PARTITION_AHEAD_DAYS` future partitions
4. no partition older than `RETENTION_DAYS`
5. numeric round-trip: stored `bid`/`ask` string-equal to the fixture values
6. `bid < ask` on ≥99% of rows (crossed-quote counter otherwise)
7. per-symbol coverage ≥99.5% of expected seconds in the window
8. skew p50/p99 per cycle within configured bound

### Test matrix

| # | Layer | Command | Pass criterion |
|---|---|---|---|
| T1 | unit | `go test ./...` | fixture parse exact; string→numeric round-trip; weight accounting; Retry-After parsing |
| T2 | pipeline | `docker compose -f docker-compose.yml -f docker-compose.test.yml up -d` then `verify` after 3 min | ≥99.5% coverage/symbol; 0 duplicates |
| T3 | faults | restart `fake` with each scenario | all symbols pause on 429/418; other symbols survive `missing`; error counters increment; no crash |
| T4 | DB outage | `docker compose stop db`, wait 3 min, `start db` | `buffer_rows` > 0 then drains; 0 dropped; 0 duplicates |
| T5 | graceful stop | `docker compose stop collector` | final flush commits; 0 rows lost vs `ticks_total` |
| T6 | hard kill | `docker compose kill -s SIGKILL collector` then up | loss ≤ flush window; **0 duplicates** |
| T7 | retention | insert backdated partition, `collector retention --once` | aged partition dropped; recent intact; ahead-partitions created |
| T8 | config extend | add 5th symbol, restart | new rows appear, no DDL, budget assertion still passes |
| T9 | live pilot | real compose, 24 h | ≥99.5% coverage/symbol-day; `dropped_ticks_total = 0` |

One driver: `scripts/test.sh` runs T1–T8 against the test compose, prints the matrix,
tears down with `-v`, exits non-zero on any failure. **T1–T8 must pass before the live pilot.**

## 8. Milestones

| M | Scope | Exit criterion |
|---|---|---|
| M1 (3 d) | compose, `migrate`, schema, partition manager, config, `/healthz` | `compose up` healthy; `verify` checks 3–4 pass; aged partition drop verified |
| M2 (4 d) | coordinator, REST client, batched upsert writer, ring buffer, graceful shutdown | T2, T4, T5, T6 pass |
| M3 (3 d) | `fake`, `verify`, `scripts/test.sh`, metrics, completeness report | **T1–T8 all green** |
| M4 (2 d) | 24 h live pilot, runbook | T9 passes; coverage report produced |

Acceptance replaces the untestable "reproduce live responses at the same second"
criterion from the design doc: you cannot re-fetch a past second. Use
(a) fixture replay equality, (b) `verify` coverage/duplicate checks,
(c) optional second collector for a fixed 1 h window, agreement ≥99%.

## 9. Metrics (plain text at `/metrics`, no client library in v1.0)

`ticks_total{symbol}`, `api_errors_total{symbol,kind}`, `api_bans_total`,
`used_weight`, `buffer_rows`, `dropped_ticks_total`, `inserted_rows_total`,
`db_flush_seconds`, `cycle_skew_ms`.

Alert thresholds for the report/verifier: coverage <99.5% ⇒ **59.7 rows/min**, not 55
(55/min = 91.7% and does not enforce the stated NFR). `dropped_ticks_total > 0` ⇒ fail.

## 10. Known gaps — deliberately deferred to v1.1

- Prometheus client library + Alertmanager; v1.0 has text metrics + `verify` only.
- Disk-backed spill (SIGKILL currently loses ≤2 s).
- `premiumIndex` / `fundingRate` for mark, index ratio and realised carry.
- Historical backfill of 1 m `indexPriceKlines` / `markPriceKlines` / `premiumIndexKlines`
  (archive has 2026-01-30 → present for these symbols). **Licence: CC BY-NC-SA 4.0 —
  commercial use needs a separate written Binance agreement.** Resolve before loading.
- Session tagging is stored but not enforced in analysis queries.
- RPI orders invisible in `bookTicker`; `rpiDepth` not collected.
- Dedicated egress IP: highest residual risk, unmitigated in code. Confirm before pilot.
- Design-doc corrections to carry into v1.1 docs: weight is **2** not 1 (480/min = 20%);
  `time` is transaction time; "raw ticks" are **1 Hz samples**; `spread` ≠ pairs spread.