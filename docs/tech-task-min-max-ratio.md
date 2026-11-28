# Tech Task: Min/Max Ratio Markers on Pair Charts

## Goal

For a chosen pair (A/B) and time period, show on the chart the **minimum** and **maximum** ratio points, together with the **prices of both symbols at those points**. This lets traders see the best-case range of a pairs trade over the period.

## Assumptions (please verify against the current code)

- Data source: L1 bid/ask rows collected in PostgreSQL (partitioned by time), e.g. for `XPTUSDT, XPDUSDT, XAUUSDT, XAGUSDT`.
- Charts live in `tools/spread-view` (or the current chart UI). Adapt names below to the real schema and endpoints.
- Both symbols are sampled in the same polling cycle, so rows can be aligned by cycle timestamp.

## Definitions

- **Pair:** symbols A and B, ratio `R = A / B`.
- **Price basis (configurable, default `mid`):**
  - `mid`: `R = mid_A / mid_B`, where `mid = (bid + ask) / 2`.
  - `executable` (optional): entry ratio for long A / short B is `ask_A / bid_B`; exit ratio is `bid_A / ask_B`. Min and max are then computed on the corresponding series.
- **Period:** `from` and `to` (UTC, inclusive start, exclusive end), or presets (1h, 6h, 24h, 7d).
- **Point:** the timestamp where `R` is min or max within the period.

## Functional requirements

1. **Aligned series.** Build the ratio series only from timestamps where both symbols have a valid quote (bid > 0 and ask > 0). Align by cycle timestamp. Do not use the exchange timestamp unless it is the agreed alignment key.
2. **Min/max calculation.** Return for the selected period:
   - `min`: `{ ts, ratio, price_a, price_b }`
   - `max`: `{ ts, ratio, price_a, price_b }`
   - Prices are in the selected basis (`mid` by default). When `executable` is selected, also return the bid and ask of both legs at that point.
3. **Chart display.**
   - Mark both points on the ratio chart (distinct markers, e.g. green for min, red for max).
   - Show a label or tooltip with: timestamp (UTC), ratio (6 decimals), price A, price B.
   - Add a small summary panel: min, max, `max/min − 1` in %, and the time between the two points.
   - Markers must update when the period or pair changes.
4. **Tie-breaking.** If several points share the same extreme value, show the earliest one.
5. **Price chart link.** Hovering a marker highlights the same timestamp on the price chart(s) of A and B.
6. **Empty or short data.** If fewer than 2 aligned points exist, show "Not enough data" and no markers.

## API (proposed)

`GET /api/pairs/ratio-extremes?a=XAUUSDT&b=XAGUSDT&from=2026-11-27T00:00:00Z&to=2026-11-28T00:00:00Z&basis=mid`

```json
{
  "pair": { "a": "XAUUSDT", "b": "XAGUSDT" },
  "period": { "from": "...", "to": "..." },
  "basis": "mid",
  "points": 85960,
  "min": { "ts": "...", "ratio": 0.0, "price_a": 0.0, "price_b": 0.0 },
  "max": { "ts": "...", "ratio": 0.0, "price_a": 0.0, "price_b": 0.0 },
  "range_pct": 0.0
}
```

Errors: `400` for invalid symbols or period (`from >= to`), `404` if there is no data, `422` if the period exceeds the allowed maximum.

## Implementation notes

- Compute min and max in SQL (join A and B on cycle timestamp, `ORDER BY ratio ASC/DESC LIMIT 1`) so only two rows travel to the UI. Do not compute them in the browser from the full series.
- Make sure the query prunes partitions by the time range and uses the existing time/symbol indexes. Check `EXPLAIN` for a 7-day period.
- Exclude or flag stale quotes: XPTUSDT quotes can be about 75 s stale. Add an optional `max_staleness_ms` parameter (default: off) that drops rows older than the threshold.
- Use a decimal or high-precision type for ratio, and format consistently (ratio 6 decimals, prices as stored).
- All timestamps are UTC in the API; the UI may show local time with a UTC toggle.

## Acceptance criteria

- [ ] For a known fixture dataset, the API returns the expected min and max, timestamps and prices.
- [ ] Markers and labels appear on the chart and match the API values.
- [ ] Changing pair, period or basis updates the markers within 1 s for a 24 h period.
- [ ] A 7-day query completes in under 2 s on production-sized data.
- [ ] Empty or one-point periods show "Not enough data" without errors.
- [ ] Ties return the earliest timestamp.
- [ ] Rows where either leg has bid/ask ≤ 0 are ignored.

## Tests

- Unit: ratio calculation for `mid` and `executable`; tie-breaking; invalid inputs.
- Integration: SQL query against the `fake` scenario data (the repo's `fake` subcommand can generate datasets with known extremes).
- UI: marker rendering and tooltip content for a fixed dataset.

## Out of scope

- Trade P&L simulation, fees and funding (can be a follow-up: `100 × (R_max / R_min − 1)` as a gross best-case estimate for 25 USDT × 4x per leg).
- Alerts and notifications.

## Open questions for the team

1. Default price basis: `mid` or `executable`?
2. Maximum allowed period (suggest 30 days)?
3. Should the chart support more than one pair at once?
