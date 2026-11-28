# metals-collector

Binance USDT-M futures L1 bid/ask collector (pilot v1.0). See
`binance-metals-docs/DEV_PLAN.md` for the full design and test matrix.

## Quick start

```
docker compose up -d --build
curl http://127.0.0.1:9090/healthz
curl http://127.0.0.1:9090/metrics
```

The `collector` service runs `bootstrap` (migrate, ensure partitions, then run).
The image is distroless: there is no shell inside it. Health checks are
performed by the binary itself (`/collector healthcheck`).

## Configuration

Environment variables only. Defaults:

| Var | Default |
|---|---|
| `SYMBOLS` | `XPTUSDT,XPDUSDT,XAUUSDT,XAGUSDT` |
| `POLL_INTERVAL_MS` | `1000` |
| `BINANCE_BASE_URL` | `https://fapi.binance.com` |
| `DB_DSN` | required |
| `FLUSH_INTERVAL_MS` | `2000` |
| `BUFFER_MAX_MINUTES` | `10` |
| `HTTP_TIMEOUT_MS` | `3000` |
| `RETENTION_DAYS` | `45` |
| `PARTITION_AHEAD_DAYS` | `7` |
| `METRICS_ADDR` | `:9090` |
| `LOG_LEVEL` | `info` |
| `SKEW_MAX_MS` | `250` |

## Subcommands

`bootstrap`, `run`, `migrate`, `healthcheck`, `retention --once`,
`fake`, `verify`, `report`. All are implemented.

## Verification

```
docker compose exec -T collector /collector verify --window-sec=180 --fixtures
./scripts/test.sh
```

`verify` runs the eight checks from the design doc and exits non-zero on any
failure. `cycle_fetch_skew_ms` is the sampling-comparability metric;
`cycle_skew_ms` is exchange-time skew and is informational (XPTUSDT quotes can
be ~75 s stale). See `RUNBOOK.md`.

## Deployment

Locally the whole pilot is `docker compose up -d --build` (PostgreSQL +
collector). Northflank does not execute `docker compose`, so `northflank.json`
is a GitOps template describing the equivalent resources:

- a project and a PostgreSQL 16 addon with TLS;
- a secret group that links the addon's `POSTGRES_URI` to the collector's
  `DB_DSN` (no credentials are stored in this repo);
- a combined service built from this repo's Dockerfile on branch `main`,
  public HTTP port 9090, scaled to 1 instance with **CI disabled**.

One-time setup: create a template linked to this repository, branch `main`,
path `/northflank.json`, with GitOps enabled but "run automatically when the
template is updated" **disabled**. Service CI is also off (`disabledCI: true`),
so pushes to `main` no longer rebuild or redeploy the collector; builds and
deploys are manual from the Northflank service menu. Commit-message skip flags
are enabled as a second safeguard. See `RUNBOOK.md` for details.

## Status

- [x] M1 — schema, migrations, partitions, config, ops endpoints, compose
- [x] M2 — polling pipeline, writer, retry buffer, graceful shutdown
- [x] M3 — fake scenarios, verify, report, T1–T8 driver (all green in quick mode)
- [x] M4 — live pilot, early T9 passed 2026-11-28 (17 h 29 m observed):
  ≥99.9 % coverage per symbol, 0 drops, 0 duplicates, partitions OK
  (details in `RUNBOOK.md`)

GitOps is active but service CI is disabled: pushes to `main` do not rebuild or
redeploy. Builds and deploys are manual from the Northflank service menu.
