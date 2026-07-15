# Backing up a Supabase database with backwyn

This guide connects the engine to a Supabase Postgres database safely.

## 1. Create a least-privilege, read-only role

Run [`sql/backup_role.sql`](../sql/backup_role.sql) in the Supabase SQL editor
(Dashboard → SQL Editor), replacing the placeholder password. This creates a
`backwyn_backup` role that can read everything and change nothing — so the
connection string you give the engine cannot be used to damage your data.

## 2. Pick the RIGHT connection string

Supabase exposes several connection strings. **It matters which one you use:**

| Connection | Port | Use for backups? |
|------------|------|------------------|
| Direct connection | 5432 | Best. Full `pg_dump` support. IPv6 only (needs IPv4 add-on or an IPv6-capable host). |
| Session pooler (Supavisor) | 5432 | OK. Works with `pg_dump`; provides IPv4. |
| Transaction pooler (Supavisor) | 6543 | Avoid. Transaction mode doesn't support the session features `pg_dump` relies on. |

Get the string from Dashboard → Project Settings → Database → Connection string.
Substitute the `backwyn_backup` role and its password for the default user.

## 3. SSL is required

Supabase requires TLS. Include `sslmode=require` (or stronger,
`sslmode=verify-full` with the Supabase CA) in the DSN. The engine passes the
DSN straight to `pg_dump`, so any libpq option in the URL is honored.

## 4. Configure the engine

```bash
# The source: your Supabase DB, via the read-only role, over SSL.
export BACKWYN_SOURCE_DSN='postgresql://backwyn_backup:PASSWORD@db.<ref>.supabase.co:5432/postgres?sslmode=require'

# The verify sandbox: a LOCAL Postgres the engine restores test copies into.
# This is never your Supabase instance — verification must not touch production.
export BACKWYN_VERIFY_ADMIN_DSN='postgresql://postgres@localhost:5432/postgres?sslmode=disable'

# 32-byte encryption key (base64). Generate once and store as a secret.
export BACKWYN_ENCRYPTION_KEY="$(head -c32 /dev/urandom | base64)"

# Off-provider storage: your own bucket (Cloudflare R2 shown).
export BACKWYN_STORAGE=s3
export BACKWYN_S3_BUCKET=my-backups
export BACKWYN_S3_ENDPOINT='https://<account>.r2.cloudflarestorage.com'
export BACKWYN_S3_REGION=auto
export BACKWYN_S3_ACCESS_KEY=...
export BACKWYN_S3_SECRET_KEY=...
export BACKWYN_S3_PATH_STYLE=true

# Optional: get alerted when a cycle fails or coverage goes stale.
export BACKWYN_ALERT_WEBHOOK='https://hooks.slack.com/services/...'

# Run a single cycle (cron), or `backwyn run` as a daemon.
backwyn run -once -max-age 24h
```

## Scope note

This performs **scheduled logical dumps** (`pg_dump`) with verification. It is
not continuous/point-in-time capture: recovery granularity is the snapshot
interval, not any-second PITR. Continuous change-capture via logical replication
is a separate, later milestone and depends on the provider granting a
replication role.
