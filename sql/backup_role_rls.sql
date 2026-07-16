-- RLS companion to backup_role.sql, for sources where you cannot grant
-- BYPASSRLS (hosted Supabase: only the built-in `postgres` role has it, and
-- it cannot pass it on).
--
-- pg_read_all_data grants SELECT but does NOT bypass row-level security, so
-- pg_dump as backwyn_backup refuses to dump any RLS-enabled table. The
-- least-privilege fix has three parts:
--
--   1. this script: an allow-all SELECT policy for backwyn_backup on every
--      RLS table in the schemas you back up;
--   2. BACKWYN_DUMP_ROW_SECURITY=true, so pg_dump reads under those policies
--      (--enable-row-security) instead of refusing;
--   3. BACKWYN_DUMP_SCHEMAS scoped to those same schemas, so the dump never
--      touches managed schemas (auth, storage) you cannot cover.
--
-- backwyn refuses to dump while any in-scope RLS table lacks coverage, so a
-- table created later without the policy fails the cycle loudly (and alerts)
-- instead of silently backing up zero rows from it. Re-run this script
-- whenever that happens; it is idempotent.
--
-- Caveat: RESTRICTIVE policies subtract from what permissive policies allow.
-- If you use them, exempt backwyn_backup in their role lists or the
-- preflight will (correctly) refuse to dump.
--
-- Run in the Supabase SQL editor (or via psql as `postgres`), after
-- backup_role.sql.

DO $$
DECLARE
  t record;
BEGIN
  FOR t IN
    SELECT n.nspname AS schema_name, c.relname AS table_name
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p')
      AND c.relrowsecurity
      AND n.nspname IN ('public')  -- <-- the schemas in BACKWYN_DUMP_SCHEMAS
      AND NOT EXISTS (
        SELECT 1 FROM pg_policy p
        WHERE p.polrelid = c.oid AND p.polname = 'backwyn_read')
  LOOP
    EXECUTE format(
      'CREATE POLICY backwyn_read ON %I.%I FOR SELECT TO backwyn_backup USING (true)',
      t.schema_name, t.table_name);
    RAISE NOTICE 'created policy backwyn_read on %.%', t.schema_name, t.table_name;
  END LOOP;
END;
$$;

-- To revoke later (per table):
--   DROP POLICY backwyn_read ON public.some_table;
