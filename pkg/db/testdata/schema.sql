-- Fixture for the live tests of pkg/db: a schema with the shapes the
-- introspection has to read — comments, an enum, a foreign key, a check, a
-- view — and the roles the read-only proof has to tell apart.
-- Applied by a superuser at the start of every run; idempotent.

DROP SCHEMA IF EXISTS ringsrv_test CASCADE;

-- Login roles first, group roles after them: a group cannot be dropped while
-- one of its members is still there.
DO $$
DECLARE r text;
BEGIN
  FOREACH r IN ARRAY ARRAY[
    'ringsrv_test_ro', 'ringsrv_test_rw', 'ringsrv_test_group_ro',
    'ringsrv_test_hidden_rw', 'ringsrv_test_col_rw',
    'ringsrv_test_readers', 'ringsrv_test_writers'
  ] LOOP
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('DROP OWNED BY %I', r);
      EXECUTE format('DROP ROLE %I', r);
    END IF;
  END LOOP;
END $$;

CREATE ROLE ringsrv_test_ro LOGIN PASSWORD 'ringsrv_test';
CREATE ROLE ringsrv_test_rw LOGIN PASSWORD 'ringsrv_test';

-- Group roles, the shape a DBA uses to avoid granting each base by hand.
CREATE ROLE ringsrv_test_readers NOLOGIN;
CREATE ROLE ringsrv_test_writers NOLOGIN;

-- A member of a group that holds nothing but SELECT: the proof must pass it.
CREATE ROLE ringsrv_test_group_ro LOGIN PASSWORD 'ringsrv_test' IN ROLE ringsrv_test_readers;

-- NOINHERIT hides the writing group from every grants view, while SET ROLE
-- reaches it in one statement: the proof must refuse it.
CREATE ROLE ringsrv_test_hidden_rw LOGIN NOINHERIT PASSWORD 'ringsrv_test' IN ROLE ringsrv_test_writers;

-- A write granted on one column, which no table-level view reports.
CREATE ROLE ringsrv_test_col_rw LOGIN PASSWORD 'ringsrv_test';

CREATE SCHEMA ringsrv_test;

CREATE TYPE ringsrv_test.order_status AS ENUM ('new', 'paid', 'cancelled');

CREATE TABLE ringsrv_test.users (
  id         bigserial PRIMARY KEY,
  email      text NOT NULL,
  phone      text,
  created_at timestamptz NOT NULL DEFAULT now(),
  balance    numeric(12,2) NOT NULL DEFAULT 0,
  avatar     bytea,
  tags       text[] NOT NULL DEFAULT '{}',
  meta       jsonb
);
COMMENT ON TABLE ringsrv_test.users IS 'Registered users';
COMMENT ON COLUMN ringsrv_test.users.email IS 'Login e-mail, unique per user';

CREATE TABLE ringsrv_test.orders (
  id      bigserial PRIMARY KEY,
  user_id bigint NOT NULL REFERENCES ringsrv_test.users(id),
  status  ringsrv_test.order_status NOT NULL DEFAULT 'new',
  amount  numeric(12,2) NOT NULL CHECK (amount >= 0)
);
COMMENT ON TABLE ringsrv_test.orders IS 'Orders; amount in rubles';
COMMENT ON COLUMN ringsrv_test.orders.status IS 'Lifecycle of an order';

CREATE VIEW ringsrv_test.paid_orders AS
  SELECT * FROM ringsrv_test.orders WHERE status = 'paid';

INSERT INTO ringsrv_test.users (email, phone, created_at, balance, avatar, tags, meta) VALUES
  ('alice@example.com', '+7 999 123-45-67', '2026-09-02 15:04:05+03', 10.50, '\x00ff', '{a,b}', '{"k": [1, 2]}'),
  ('bob@example.com', NULL, '2026-09-01 00:00:00+00', 0, NULL, '{}', NULL);

INSERT INTO ringsrv_test.orders (user_id, status, amount) VALUES
  (1, 'paid', 100), (1, 'new', 5), (2, 'cancelled', 1);

ANALYZE ringsrv_test.users;
ANALYZE ringsrv_test.orders;

GRANT USAGE ON SCHEMA ringsrv_test TO ringsrv_test_ro, ringsrv_test_rw,
  ringsrv_test_readers, ringsrv_test_writers, ringsrv_test_group_ro,
  ringsrv_test_hidden_rw, ringsrv_test_col_rw;
GRANT SELECT ON ALL TABLES IN SCHEMA ringsrv_test TO ringsrv_test_ro;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ringsrv_test TO ringsrv_test_rw;

GRANT SELECT ON ALL TABLES IN SCHEMA ringsrv_test TO ringsrv_test_readers;
GRANT SELECT, INSERT ON ALL TABLES IN SCHEMA ringsrv_test TO ringsrv_test_writers;

GRANT SELECT ON ALL TABLES IN SCHEMA ringsrv_test TO ringsrv_test_col_rw;
GRANT UPDATE (amount) ON ringsrv_test.orders TO ringsrv_test_col_rw;
