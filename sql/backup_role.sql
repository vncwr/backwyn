-- Least-privilege backup role for backwyn.
-- Run this once in the Supabase SQL editor (Dashboard -> SQL Editor), or via
-- psql as the `postgres` role. Replace the password with a strong secret and
-- store it only in BACKWYN_SOURCE_DSN (never commit it).

-- 1. Create the login role.
CREATE ROLE backwyn_backup WITH LOGIN PASSWORD 'REPLACE_WITH_A_STRONG_SECRET';

-- 2. Allow it to connect to the database being backed up (Supabase: "postgres").
GRANT CONNECT ON DATABASE postgres TO backwyn_backup;

-- 3. Grant read-only access to ALL data, current and future.
--    pg_read_all_data is a predefined role (PostgreSQL 14+, Supabase is newer)
--    that confers SELECT on every table/view and USAGE on every sequence,
--    including objects created later.
GRANT pg_read_all_data TO backwyn_backup;

-- To revoke later:
--   DROP ROLE backwyn_backup;
