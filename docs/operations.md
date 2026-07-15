# Operations

## Running

### Docker Compose

```
cp .env.example .env    # fill in source DSN, key, bucket
docker compose up -d
```

Two containers: the engine, and a `verify-sandbox` Postgres that exists only as
somewhere to test-restore into. The sandbox has no exposed ports and keeps its
data in tmpfs.

### Cron

`run -once` executes a single cycle and exits.

```
0 */6 * * *  docker run --rm --env-file /etc/backwyn.env ghcr.io/vncwr/backwyn \
               run -once -keep-daily 14 -keep-monthly 12
```

This still needs a reachable verify sandbox via `BACKWYN_VERIFY_ADMIN_DSN`.

### Binary

Needs `pg_dump`, `pg_restore`, and `psql` on PATH, at a version **>=** your
server. The container image bundles them; a bare binary does not.

## Monitoring

`check` is the integration point. It exits **3** when there is no verified
backup within `-max-age`.

```
backwyn check -max-age 24h
```

Wire it to whatever pages you. Exit 3 means: *you do not currently have a
provably good backup.* That is different from "a job errored" — a backup job
that silently stopped produces no error at all, and this is the only signal.

Also handle `warn`-level webhook events. They mean coverage is healthy but the
newest backup failed verification: an older backup is carrying you, and nobody
has noticed yet.

### HTTP endpoints (daemon only)

The daemon serves two endpoints on `-listen` (default `:8080`):

**`/healthz`** answers the coverage question, not "did the last job error".
It returns 200 only when the last check found a verified backup within
`-max-age`. Before the first cycle completes it returns 503 `starting` — a
fresh daemon has proven nothing yet. A verify can take minutes on a large
database, so give startup probes headroom rather than pointing a restart
policy straight at this endpoint.

**`/metrics`** is Prometheus text format. The one to alert on:

```
backwyn_last_verified_backup_time_seconds
```

the epoch timestamp of the newest *verified* backup (0 if none). The alert
rule that matches `check -max-age 24h`:

```
time() - backwyn_last_verified_backup_time_seconds > 86400
```

`backwyn_coverage_healthy` is the same judgment precomputed. The rest
(`backwyn_last_backup_*`, `backwyn_last_verify_*`) describe the most recent
cycle — useful for dashboards, but a failed cycle is not an emergency while
coverage holds; that distinction is exactly why the coverage gauges exist.

The endpoints hold no secrets, but there is no reason to expose them publicly;
keep the port internal to your network.

## Restore runbook

**Production is down and you need data back.**

### 1. Find a verified backup

```
backwyn list
```

The `VERIFIED` column is the only one that matters. `NO` means it was never
proven restorable — the `NOTE` column says why.

### 2. Restore into a new database

Never restore over production. Create an empty target first:

```
createdb recovered
backwyn restore <id> -to postgresql://postgres@localhost:5432/recovered
```

backwyn refuses by default to restore into a non-empty database, into the source
database, or from an unverified backup. Those guards exist because this command
runs when someone is in a hurry.

### 3. If you need the raw archive instead

```
backwyn restore <id> -to-file ./recovered.pgc
pg_restore --no-owner --dbname <dsn> ./recovered.pgc
```

This writes a standard `pg_dump` custom archive. Stock `pg_restore` reads it with
backwyn nowhere in the loop — the encryption envelope is not a lock-in trap. If
backwyn ever disappears, your backups remain yours.

### Escape hatches

| Flag | Use when |
|---|---|
| `-force` | Target is non-empty (adds `pg_restore --clean --if-exists`), or you really mean to restore over the source |
| `-allow-unverified` | You need a backup that failed verification, accepting it may be corrupt |

`-allow-unverified` does not bypass integrity: AES-GCM still rejects a tampered
artifact. It only bypasses the *policy* of refusing unproven backups.

## Failure modes

| Symptom | Cause |
|---|---|
| `pg_dump failed: server version mismatch` | Client older than the server. Use the image, or install a newer client. |
| `verification failed: decrypt: ... message authentication failed` | The artifact is corrupt or the key is wrong. Working as designed — this is the catch. |
| `checksum mismatch` | Artifact decrypts but the plaintext is not what was dumped. |
| `cannot inspect restore target` | Target database does not exist. `createdb` it first. |
| `refusing to prune: no verified backup exists` | Safety rule. Fix verification before pruning. |
| `BACKWYN_VERIFY_ADMIN_DSN is required` | `verify`/`restore`/`run` need a sandbox. |

## Retention

```
backwyn prune -keep-daily 14 -keep-monthly 12 -dry-run
```

Always `-dry-run` first. It prints every decision with a reason.

Only **verified** backups fill retention slots — an unverified artifact is not
coverage. Three rules hold regardless of policy: nothing is pruned unless
something is verified, the newest verified backup is never deleted, and the
newest backup is never deleted.

## Cost notes

Verification is the only part that costs real CPU, because it is a full restore
every cycle. A sub-GB database is a few minutes per cycle. A 50GB database is
closer to an hour, plus scratch disk equal to the database size.

If verification cost becomes a problem, decouple the cadence: back up every 6h,
deep-verify daily. `check` reads manifests only, so coverage reporting is
unaffected.
