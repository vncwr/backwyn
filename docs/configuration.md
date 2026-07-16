# Configuration

All configuration is read from the environment. Nothing is passed via flags —
flags leak into process listings and shell history.

See [.env.example](../.env.example) for a fillable template.

## Required

| Variable | Description |
|---|---|
| `BACKWYN_SOURCE_DSN` | The database to back up. Use a least-privilege read-only role, never the owner. See [supabase.md](supabase.md). |
| `BACKWYN_ENCRYPTION_KEY` | Base64 of 32 random bytes. `head -c32 /dev/urandom \| base64` |

`BACKWYN_VERIFY_ADMIN_DSN` is required for `verify`, `restore`, and `run` — but
not for `backup`, `list`, `check`, or `prune`.

### The encryption key

Store it somewhere that is **not** your artifact bucket and **not** this repo —
a password manager or a secrets manager. Artifacts are AES-256-GCM; if the key
is lost there is no recovery path, and every backup you hold becomes permanently
unreadable.

A key in the same place as the artifacts also defeats the point: anyone who
reaches the bucket reaches the plaintext.

## Verify sandbox

| Variable | Description |
|---|---|
| `BACKWYN_VERIFY_ADMIN_DSN` | Admin DSN for a throwaway Postgres that backups are test-restored into. |

This must **not** be your source database. backwyn creates a scratch database
per verify (`backwyn_verify_<id>`) and drops it afterward, so the role needs
`CREATEDB`.

Its major version must be **>=** the source's, or `pg_restore` refuses the
archive. `docker-compose.yml` runs one as a sidecar with no exposed ports.

### Verify query

| Variable | Description |
|---|---|
| `BACKWYN_VERIFY_QUERY` | Optional SQL executed against the restored sandbox. If it errors, verification fails. |

The built-in table count proves the archive restores; a verify query proves the
restored data is *yours*. Point it at something that must exist:

```
BACKWYN_VERIFY_QUERY=SELECT count(*) FROM customers;
```

A restore that comes back without your most important table is not a backup,
even if `pg_restore` exits 0.

### Dump scope

| Variable | Description |
|---|---|
| `BACKWYN_DUMP_SCHEMAS` | Comma-separated schemas to dump (`pg_dump -n`). Empty dumps the whole database. |

Useful for Supabase and similar platforms, where a whole-database dump carries
the platform's own schemas (`auth`, `storage`, ...) and `CREATE EXTENSION`
entries the verify sandbox may not be able to satisfy:

```
BACKWYN_DUMP_SCHEMAS=public
```

**Understand what you exclude.** `BACKWYN_DUMP_SCHEMAS=public` backs up your
tables but not `auth` — a restored copy will not contain user accounts. The
alternative is keeping the full dump and running a Supabase-compatible verify
sandbox instead; see [supabase.md](supabase.md#extensions-and-the-verify-sandbox).

### Row-level security

| Variable | Default | Description |
|---|---|---|
| `BACKWYN_DUMP_ROW_SECURITY` | `false` | Dump RLS tables under the role's policies (`pg_dump --enable-row-security`) instead of requiring `BYPASSRLS`. |

`pg_dump` refuses to dump an RLS-enabled table unless the role can bypass RLS,
and hosted platforms (Supabase) will not let you grant `BYPASSRLS` to a custom
role. Setting this dumps under the role's policies instead — which is only a
complete backup if the role can see every row. Grant it an allow-all read
policy on every table first: [`sql/backup_role_rls.sql`](../sql/backup_role_rls.sql).

Before every dump, backwyn checks that each in-scope RLS table has such a
policy and refuses to back up otherwise — a silently partial backup marked
VERIFIED is worse than a loud failure. A table created later without the
policy fails the cycle (and alerts) until you re-run the script.

## Storage

| Variable | Default | Description |
|---|---|---|
| `BACKWYN_STORAGE` | `local` | `local` or `s3` |
| `BACKWYN_STORAGE_DIR` | — | Root directory, when `local` |
| `BACKWYN_S3_BUCKET` | — | Bucket name, when `s3` |
| `BACKWYN_S3_ENDPOINT` | AWS default | e.g. `https://<account>.r2.cloudflarestorage.com` |
| `BACKWYN_S3_REGION` | `auto` | R2 uses `auto` |
| `BACKWYN_S3_ACCESS_KEY` | — | |
| `BACKWYN_S3_SECRET_KEY` | — | |
| `BACKWYN_S3_PATH_STYLE` | `false` | `true` for R2, MinIO |

`local` is for testing. A backup on the same machine as the database it came
from is not an off-provider copy.

Cloudflare R2 is the suggested default because it charges **no egress fees** —
you pull data out during a restore, which is the one day you do not also want a
bandwidth bill. S3 charges roughly $0.09/GB for that.

## Alerting

| Variable | Description |
|---|---|
| `BACKWYN_ALERT_WEBHOOK` | URL that receives JSON POSTs. Slack-compatible. |

Empty disables alerting, which means a silent backup failure stays silent. Set
it.

Payload:

```json
{
  "level": "error",
  "title": "backup coverage unhealthy",
  "detail": "last verified backup is 31h0m0s old, older than the 24h0m0s threshold",
  "time": "2026-07-15T12:00:00Z"
}
```

`level` is `info`, `warn`, or `error`. **`warn` matters**: it fires when coverage
is still healthy but the *most recent* backup failed verification — an older
backup is carrying you. Do not route `warn` to a channel nobody reads; left
alone it becomes an outage.

## Observability server

| Variable | Default | Description |
|---|---|---|
| `BACKWYN_LISTEN_ADDR` | `:8080` | Default for `run -listen`. The flag wins when both are set. |

The daemon serves `/healthz` and `/metrics` (Prometheus text format). Set
`-listen ""` to disable. `run -once` never listens — a cron invocation has
nothing to scrape. See [operations.md](operations.md#monitoring) for the
endpoint semantics.

## Flags

Configuration is environment-only; schedule and policy are flags.

```
backwyn run [-interval 6h] [-max-age 24h] [-once] [-listen :8080]
            [-keep-last N] [-keep-daily N] [-keep-weekly N] [-keep-monthly N]

backwyn check [-max-age 24h]

backwyn prune [-keep-* N] [-dry-run] [-sweep-orphans]

backwyn restore <id> (-to <dsn> | -to-file <path>) [-force] [-allow-unverified]
```

| Flag | Default | Description |
|---|---|---|
| `-interval` | `6h` | Time between cycles |
| `-max-age` | `24h` | Alert if no verified backup is newer than this |
| `-once` | `false` | Run a single cycle and exit (for cron) |
| `-listen` | `:8080` | Address for `/healthz` + `/metrics`; `""` disables |
| `-keep-last` | `0` | Keep the N most recent verified backups |
| `-keep-daily` | `0` | Keep the newest verified backup from each of the last N days |
| `-keep-weekly` | `0` | ...each of the last N ISO weeks |
| `-keep-monthly` | `0` | ...each of the last N months |
| `-dry-run` | `false` | Show the prune plan without deleting |
| `-sweep-orphans` | `false` | Also delete artifacts >24h old with no manifest |

With no `-keep-*` flags the policy is empty and **prune deletes nothing**. That
default is deliberate: a forgotten flag keeps everything rather than wiping
history.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Error |
| `2` | Usage error |
| `3` | `check` only: coverage is not healthy |

`check` exiting 3 is the cron/monitoring integration point.
