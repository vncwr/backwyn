# backwyn

**postgres backups that prove they restore.**

Every backup is restored into a real database — every cycle. If it doesn't
restore, you find out tonight instead of during an outage. If backups quietly
stop happening, you get told that too.

For Supabase, Neon, PlanetScale, or any Postgres. Artifacts are encrypted and
land in a bucket you own. MIT licensed.

---

## the problem

> The condition of any backup is unknown until a restore is attempted.

A backup job that silently stops running produces **no error to alert on**. A
corrupted archive produces no error either — until someone tries to restore it,
which is by definition the worst possible moment to find out.

Almost every backup tool answers *"did the job run?"* Nobody answers *"do I have
a good backup right now?"* — and that's the only question that matters when
production is gone.

Managed Postgres providers don't close this either: they store backups in the
same account as the primary, don't test-restore them, and won't tell you when
there hasn't been a good one in three days.

## what backwyn does

Every cycle, it takes a backup **and then proves it**:

| stage | what it does |
|---|---|
| **backup** | `pg_dump` → sha256 → AES-256-GCM → your bucket → manifest |
| **verify** | fetch it back → decrypt → re-check the checksum → confirm the archive parses → **restore into a throwaway database** → count tables |
| **check** | is there a *verified* backup within `-max-age`? exit 3 and fire a webhook if not |
| **prune** | retention over verified backups only, with safety rules |

A backup is only marked `verified` after it has actually been restored. Anything
less and it's `VERIFIED=NO` with the reason attached.

That's what makes the alerting possible: you can only ask *"is there a good
backup right now"* if something is continuously proving backups good.

## quick start

```bash
cp .env.example .env      # source DSN, encryption key, bucket
docker compose up -d
```

Two containers: the engine, and a throwaway Postgres that exists only as
somewhere to test-restore into. Nothing of yours lives in the sandbox — a scratch
database is created per verify and dropped after.

The image bundles `pg_dump`/`pg_restore`/`psql`, so there's no client-version
matching to get wrong.

You need three things: a database, a bucket, and a 32-byte key
(`head -c32 /dev/urandom | base64`).

> Point it at a **least-privilege read-only role**, never the owner connection
> string. `sql/backup_role.sql` creates one; [docs/supabase.md](docs/supabase.md)
> walks through Supabase.

## getting your data back

The whole point.

```bash
# into a fresh database
createdb recovered
backwyn restore <id> -to postgresql://postgres@localhost:5432/recovered
```

`restore` refuses by default to restore into a non-empty database, into the
source database, or from a backup that never passed verification. Those guards
exist because this command runs when someone is in a hurry. `-force` and
`-allow-unverified` are the deliberate escape hatches.

**Or skip backwyn entirely:**

```bash
backwyn restore <id> -to-file ./recovered.pgc
pg_restore --no-owner --dbname <dsn> ./recovered.pgc
```

That writes a standard `pg_dump` archive. Stock `pg_restore` reads it with
backwyn nowhere in the loop. The encryption envelope is not a lock-in trap — if
this project disappears tomorrow, your backups are still yours.

## commands

```
backwyn backup                        dump, encrypt, store, write manifest
backwyn verify <id>                   prove a stored backup restores
backwyn restore <id> -to <dsn>        restore into a database
            <id> -to-file <path>      ...or out to a plain archive
backwyn list                          backups and verification status
backwyn check -max-age 24h            exit 3 if no verified backup is fresh
backwyn prune -keep-daily 14          delete outside the retention policy
backwyn run -interval 6h              the full cycle, on a timer
```

`check` is the monitoring integration point. Exit **3** means: *you do not
currently have a provably good backup.*

## retention

```bash
backwyn prune -keep-daily 14 -keep-monthly 12 -dry-run
```

Always dry-run first — it prints every decision with its reason.

Only **verified** backups fill retention slots; an unverified artifact isn't
coverage and doesn't get to hold a daily slot a real backup could. Three rules
hold regardless of policy:

1. nothing is pruned unless at least one backup is verified
2. the most recent verified backup is never deleted
3. the most recent backup is never deleted — it may be mid-verification

With no `-keep` flags the policy is empty and **prune deletes nothing**. A
forgotten flag keeps everything rather than wiping history.

## what it doesn't do

- **Not point-in-time recovery.** These are scheduled logical dumps. Recovery
  granularity is your snapshot interval, not any-second PITR.
- **Not a replacement for your provider's backups.** It's the independent second
  copy, and the proof that a copy actually works.
- **One database per daemon.**
- **Verification costs real CPU** — it's a full restore every cycle. Cheap for a
  sub-GB database, expensive for a 50GB one.

## trust

You're being asked for a production database connection string. Reasons to
believe it's safe, all of them checkable in this repo:

- **The worker is open source.** This is the code that touches your database.
- **Your bucket, your account.** Data never lands anywhere we control.
- **Your key never leaves your machine.** Artifacts are encrypted before upload.
- **Read-only by design.** The source role needs `SELECT` and nothing more.
- **No lock-in.** `-to-file` gives you a standard archive, any time.

## docs

- [architecture](docs/architecture.md) — the cycle, invariants, artifact format
- [configuration](docs/configuration.md) — every env var, flags, exit codes
- [operations](docs/operations.md) — running, monitoring, restore runbook
- [supabase](docs/supabase.md) — connection + least-privilege role

## tests

`go test ./...` is hermetic — no Postgres, no network, no credentials.

`scripts/localtest.sh` is the end-to-end proof against a real throwaway Postgres:
backup → verify → check → restore round-trip (diffing restored row counts against
the source) → restore guards → plain-archive escape hatch → corruption detection
→ absence-alerting → prune safety rules. It touches no real database.

CI runs both against PostgreSQL 15 and 17 on every push.

## license

MIT
