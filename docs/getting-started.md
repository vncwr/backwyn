# Getting started

From zero to a backup that has proven it restores. Fifteen minutes, most of it
waiting on your first `pg_dump`.

If you get stuck: [failure modes](operations.md#failure-modes) covers the
common errors.

## What you need

Three things, none of them from us:

1. **A Postgres database to protect** — Supabase, Neon, PlanetScale, RDS, or
   anything else that speaks `pg_dump`.
2. **A bucket you own** — Cloudflare R2 is the suggested default (no egress
   fees, and a restore is the one day you don't want a bandwidth bill). Any
   S3-compatible store works.
3. **An encryption key** — 32 random bytes, base64:

   ```bash
   head -c32 /dev/urandom | base64
   ```

   Store it in a password manager or secrets manager — **not** in the bucket,
   not in this repo. Lose it and every backup becomes permanently unreadable.
   There is no recovery path. This is the one step to take seriously before
   anything else.

## Step 1 — create a read-only role

Never give backwyn your owner or `postgres` connection string. It needs
`SELECT` and nothing more.

Run [`sql/backup_role.sql`](../sql/backup_role.sql) against your database
(replace the placeholder password). For Supabase specifics — which connection
string to use, SSL, the pooler trap — see [supabase.md](supabase.md).

## Step 2 — run it (Docker Compose, the easy path)

```bash
git clone https://github.com/vncwr/backwyn
cd backwyn
cp .env.example .env    # fill in: source DSN, encryption key, bucket
docker compose up -d
```

That starts two containers:

- **backwyn** — the engine, on a 6h cycle: backup → verify → check → prune.
- **verify-sandbox** — a throwaway Postgres that exists only as somewhere to
  test-restore into. Nothing of yours lives there; a scratch database is
  created per verify and dropped after.

The image bundles `pg_dump`/`pg_restore`/`psql`, so there is no client-version
matching to get wrong.

Watch the first cycle:

```bash
docker compose logs -f backwyn
```

You are looking for two lines: `backup ok` and then `verify ok`. The second
one is the product — it means the backup was pulled back out of your bucket,
decrypted, and actually restored into the sandbox.

### Without Docker (bare binary)

```bash
go build -o backwyn ./cmd/backwyn
```

You then need `pg_dump`, `pg_restore`, and `psql` on PATH at a version **>=**
your server's, plus a local Postgres to act as the verify sandbox
(`BACKWYN_VERIFY_ADMIN_DSN`, a role with `CREATEDB`). Set the same variables
as `.env.example` in your environment and run `backwyn run`, or `run -once`
from cron. Every variable is documented in [configuration.md](configuration.md).

## Step 3 — see what you have

```bash
docker compose exec backwyn backwyn list
```

```
ID                SOURCE                    PLAINTEXT  VERIFIED  TABLES  NOTE
20260715T120000Z  db.xxx.supabase.co/postgres  8123456    yes       24
```

The `VERIFIED` column is the only one that matters. `yes` means this backup
has been restored into a real database and came back with your tables. `NO`
means it has not been proven — the `NOTE` column says why.

## Step 4 — do one restore drill now

Not during an outage. Now, while nothing is wrong:

```bash
createdb drill
backwyn restore <id> -to postgresql://postgres@localhost:5432/drill
psql drill -c '\dt'    # your tables, in a database you just made
dropdb drill
```

`restore` refuses by default to touch a non-empty database, the source
database, or an unverified backup — the guards exist because this command
usually runs while someone is panicking. The full runbook, including the
escape hatches, is in [operations.md](operations.md#restore-runbook).

## Step 5 — wire up the alarm

backwyn's core claim is that it tells you when you *don't* have a good backup.
That needs somewhere to tell you:

```bash
BACKWYN_ALERT_WEBHOOK=https://hooks.slack.com/services/...
```

Left empty, a silent backup failure stays silent, which is the exact failure
mode this tool exists to prevent. Set it.

For monitoring systems: `backwyn check -max-age 24h` exits **3** when no
verified backup is fresh enough, and the daemon serves `/healthz` and
Prometheus `/metrics` on `:8080`. Details in
[operations.md](operations.md#monitoring).

## Optional — prove the data is yours

The built-in check counts restored tables. A verify query goes further —
verification fails unless it runs cleanly against the restored copy:

```bash
BACKWYN_VERIFY_QUERY=SELECT count(*) FROM customers;
```

Point it at a table that must exist for a backup to be worth having.

## Where to next

- [commands.md](commands.md) — every command, flag, and exit code
- [configuration.md](configuration.md) — every environment variable
- [operations.md](operations.md) — monitoring, the restore runbook, retention
- [architecture.md](architecture.md) — how it works and the invariants it keeps
