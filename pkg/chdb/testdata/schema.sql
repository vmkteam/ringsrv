-- Fixture for the live tests of pkg/chdb, one statement per block: a
-- database with a table that has comments and the column types the answer
-- has to carry, and four users — one read-only the way the catalogue wants
-- it, one with a setting left changeable, one with an INSERT grant, one
-- with DDL — for the probe to tell apart. Applied by the admin user of the
-- local stand at the start of every run; idempotent.

DROP DATABASE IF EXISTS ringsrv_test;

DROP USER IF EXISTS ringsrv_test_ro, ringsrv_test_free, ringsrv_test_rw, ringsrv_test_ddl;

CREATE DATABASE ringsrv_test;

CREATE TABLE ringsrv_test.events (
  id     UInt64 COMMENT 'Event id',
  ts     DateTime64(3, 'UTC') COMMENT 'When it happened',
  kind   LowCardinality(String),
  amount Decimal(12, 2),
  tags   Array(String),
  attrs  Map(String, String),
  note   Nullable(String),
  ip     IPv4
) ENGINE = ReplacingMergeTree ORDER BY (id, ts) PARTITION BY toYYYYMM(ts)
COMMENT 'Events of the scanner';

INSERT INTO ringsrv_test.events VALUES
  (1, '2026-09-02 12:04:05.123', 'scan', 10.50, ['a', 'b'], {'k': 'v'}, 'note', '10.0.0.1'),
  (2, '2026-09-01 00:00:00', 'order', 0, [], {}, NULL, '127.0.0.1');

CREATE USER ringsrv_test_ro IDENTIFIED WITH plaintext_password BY 'ringsrv_test'
  SETTINGS readonly = 2 READONLY, allow_ddl = 0 READONLY, max_execution_time = 30 READONLY;

GRANT SELECT ON ringsrv_test.* TO ringsrv_test_ro;

CREATE USER ringsrv_test_free IDENTIFIED WITH plaintext_password BY 'ringsrv_test'
  SETTINGS readonly = 2 READONLY, allow_ddl = 0 READONLY;

GRANT SELECT ON ringsrv_test.* TO ringsrv_test_free;

CREATE USER ringsrv_test_rw IDENTIFIED WITH plaintext_password BY 'ringsrv_test'
  SETTINGS readonly = 2 READONLY, allow_ddl = 0 READONLY;

GRANT SELECT, INSERT ON ringsrv_test.* TO ringsrv_test_rw;

CREATE USER ringsrv_test_ddl IDENTIFIED WITH plaintext_password BY 'ringsrv_test'
  SETTINGS readonly = 2 READONLY, allow_ddl = 1;

GRANT SELECT ON ringsrv_test.* TO ringsrv_test_ddl;
