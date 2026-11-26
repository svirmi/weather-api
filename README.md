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
`fake`, `verify`, `report`. The last three are delivered in M3.

## Status

- [x] M1 — schema, migrations, partitions, config, ops endpoints, compose
- [ ] M2 — polling pipeline, writer, retry buffer, graceful shutdown
- [ ] M3 — fake, verify, test driver
- [ ] M4 — 24 h live pilot and runbook
