-- Idempotent base schema. Daily partitions are created by the partition manager.
CREATE TABLE IF NOT EXISTS book_l1 (
  ts_sec    timestamptz   NOT NULL,
  symbol    text          NOT NULL,
  exch_time timestamptz   NOT NULL,
  bid       numeric(20,8) NOT NULL,
  bid_qty   numeric(20,8) NOT NULL,
  ask       numeric(20,8) NOT NULL,
  ask_qty   numeric(20,8) NOT NULL,
  PRIMARY KEY (ts_sec, symbol)
) PARTITION BY RANGE (ts_sec);

CREATE TABLE IF NOT EXISTS session_window (
  start_time timestamptz PRIMARY KEY,
  end_time   timestamptz NOT NULL,
  kind       text        NOT NULL
);

CREATE OR REPLACE VIEW book_l1_spread AS
SELECT ts_sec, symbol, bid, ask, bid_qty, ask_qty,
       ask - bid AS spread,
       (bid + ask) / 2 AS mid
FROM book_l1;
