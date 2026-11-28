# Runbook — metals L1 collector v1.0

## Normal operation

```
docker compose up -d --build
curl http://127.0.0.1:9090/healthz
curl http://127.0.0.1:9090/metrics
docker compose logs -f collector
```

`bootstrap` runs migrations, ensures partitions, then starts the pipeline.
Calling the binary with no subcommand defaults to `run`, which also migrates,
so platforms that launch the image with no arguments still self-initialise.
The container is distroless: no shell, no curl. All probes use the binary.

Port 9090 is published on all host interfaces (`"9090:9090"`), so `/healthz`
and `/metrics` are reachable from the Northflank ingress. The metrics are
non-sensitive operational counters. Northflank terminates TLS at the edge and
routes plain HTTP to container port 9090, so the public URL is
`https://<generated-host>/metrics` — do **not** append `:9090` to it.

## Verification

```
docker compose exec -T collector /collector verify --window-sec=180 --fixtures
docker compose exec -T collector /collector report --window-sec=180
```

`--fixtures` is only valid when the collector points at the built-in `fake`.
`--pause-sec` excludes known IP-ban pause intervals from the coverage denominator.
`verify` exits non-zero on any failed check.

## Test matrix

```
./scripts/test.sh                             # full 180 s windows
VERIFY_WINDOW_SEC=60 CHECK_WINDOW_SEC=30 DB_OUTAGE_SEC=45 ./scripts/test.sh
```

T1–T8 must be green before a live pilot. The driver tears down with `down -v`.

## Adding a symbol

Set `SYMBOLS` on both the collector and (in tests) the fake, then recreate.
No DDL is needed: partition names are per day, not per symbol. Check that
`weight_budget_per_min` stays below the `exchangeInfo` `REQUEST_WEIGHT` limit
(16 symbols is the hard startup cap).

Removing a symbol leaves orphan rows. Wipe the volume:

```
docker compose down -v && docker compose up -d --build
```

## DB outage

Rows accumulate in the bounded in-memory ring (`BUFFER_MAX_MINUTES`, default
10 min). Watch `buffer_rows` and `dropped_ticks_total`. When PostgreSQL
returns, the next flush drains the ring. If `dropped_ticks_total > 0`,
the pilot has failed its acceptance criterion. There is no disk spill in v1.0.

## IP bans (429 / 418)

A ban pauses **all** symbols for `Retry-After` seconds; `api_bans_total`
increments. Do not retry individual symbols — the limit is IP-scoped. No
backfill is performed for paused seconds. Confirm a dedicated egress IP
before the live pilot; this is the highest residual risk.

## Skew interpretation

Two distinct metrics are exported:

- `cycle_fetch_skew_ms` — spread of the concurrent GET completion times.
  This is the evidence that legs were sampled together; `verify` check 8
  enforces `SKEW_MAX_MS` (default 250 ms) against this metric.
- `cycle_skew_ms` — `max(exch_time) - min(exch_time)` from the payload.
  This is **informational**. Thin instruments such as XPTUSDT carry a stale
  transaction time (observed ~75 s behind wall clock) even when all legs were
  fetched in the same cycle.

## Retention

`retention --once` or the hourly partition manager drops partitions whose
entire UTC day ends before `now - RETENTION_DAYS`, and creates
`PARTITION_AHEAD_DAYS` future partitions. Today's partition is never dropped.

## Graceful shutdown

`docker compose stop collector` sends SIGTERM. The collector stops intake,
waits for in-flight HTTP, flushes with a **fresh** 10 s context, then closes
ops and the pool. `stop_grace_period: 30s` covers the worst case.
`docker compose kill -s SIGKILL collector` loses at most one flush window
(≤2 s) but must not create duplicates (PK on `ts_sec, symbol`).

## Remote deployment (Northflank)

Northflank does not run `docker compose`; `northflank.json` is a GitOps
template describing the remote pilot:

1. A project (`weather api`, `europe-west`) and a PostgreSQL 16 addon (`pg-1`,
   TLS enabled, 6 GB storage).
2. A secret group that maps the addon's `POSTGRES_URI` to `DB_DSN`. Credentials
   live only in Northflank — never in this repository.
3. A combined service (`weather-api`) built from this repo on `main`, public
   HTTP port 9090, 1 instance, **CI disabled**, build cache disabled.

One-time setup: create a template linked to this repository/branch `main`, path
`/northflank.json`, with GitOps enabled but **run automatically when the
template is updated disabled**. Both triggers are off: service CI
(`disabledCI: true`) and template autorun (`options.autorun: false`). Pushes to
`main` no longer restart the pilot. To ship a change, trigger a build manually
from the service menu, then deploy that build (confirm in the UI whether a
manual build on a combined service deploys automatically). The collector's
`run` subcommand self-migrates a fresh addon database automatically.

The commit that switches `autorun` off may itself be the last automatic template
run, so expect one final restart when you apply this change. Make template
changes in one place only: GitOps is bidirectional and UI edits are committed
back to `main`.

If you no longer want a template, disable GitOps in Northflank **before**
deleting `northflank.json`.

The binary defaults to the `run` subcommand, which now self-migrates, so no
platform command override is needed. `DATABASE_URL` is accepted as an alias
for `DB_DSN` if a platform injects that name instead.

## Live pilot (T9)

```
docker compose down -v
docker compose up -d --build
# after 24 h:
docker compose exec -T collector /collector verify --window-sec=86400
docker compose exec -T collector /collector report --window-sec=86400
```

Acceptance: ≥99.5 % coverage per symbol-day and `dropped_ticks_total = 0`.
Optional agreement test: a second collector writing to a **separate** DB for
a fixed 1 h window, agreement ≥99 % (two writers into one table would collide
on the primary key).

## Dev data proxy (`rawproxy`)

A separate, unauthenticated read-only HTTP service for local testing. It runs
inside Northflank (where the Postgres addon is reachable) and exposes:

- `/api/v1/rows?symbol=XAUUSDT&from=…&to=…&order=asc&limit=5000` — raw rows;
  when a full page is returned, feed the response's `next_cursor` into the next
  request's `cursor=` parameter to page without slicing time windows.
- `/api/v1/stats?from=…&to=…` — one shot with per-symbol row counts, first/last
  ts, max gap, duplicates, `bid<ask` ratio and partition state. This covers all
  DB-backed T9 checks without pulling ~235 k rows locally.
- `/api/v1/spread`, `/api/v1/sessions`, `/healthz`, `/metrics`.

Deploy: `northflank.json` already contains the `rawproxy` service; it uses the
injected `DB_DSN` and needs no token secret. Apply the template manually and
build/deploy `rawproxy` manually (`disabledCI: true`, `autorun: false`). The
endpoint is public and unauthenticated — it only issues the read-only SQL
above, but treat it as an open data source.

## Known limitations

RPI orders are invisible in `bookTicker`; `"volume at L1"` means resting
`bidQty`/`askQty`, not traded volume. No mark/index/funding, no historical
backfill, no alerting stack, no disk spill. The backfill archive is
CC BY-NC-SA 4.0 — commercial use needs a separate written Binance agreement.
