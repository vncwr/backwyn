# Commands

Every command reads its configuration from the environment — see
[configuration.md](configuration.md). All commands need `BACKWYN_SOURCE_DSN`,
`BACKWYN_ENCRYPTION_KEY`, and a storage backend configured; `verify`,
`restore`, and `run` additionally need `BACKWYN_VERIFY_ADMIN_DSN`.

Backup IDs are UTC timestamps (`20260715T120000Z`) — `list` shows them.

## backup

```
backwyn backup
```

One backup: `pg_dump --format=custom` → SHA-256 → AES-256-GCM → your bucket →
manifest. No flags. Prints the new backup's ID.

A fresh backup is **not yet verified** — nothing counts as coverage until it
has been restored somewhere.

## verify

```
backwyn verify <id>
```

Proves a stored backup restores: fetches it back out of storage, decrypts,
re-checks the checksum, confirms the archive parses, restores it into a
scratch database on the verify sandbox, counts the tables, and — if
`BACKWYN_VERIFY_QUERY` is set — runs that query against the restored copy.
Only if all of that succeeds is the manifest stamped `verified`.

A failed verify is recorded on the manifest with the reason, **replacing any
earlier successful verification** — the record reflects the latest proof
attempt. Re-run `verify` after fixing the cause to restore verified status.

## restore

```
backwyn restore <id> -to <dsn>
backwyn restore <id> -to-file <path>
```

Brings a backup back — this is the command the rest of the tool exists to
make trustworthy.

| Flag | Meaning |
|---|---|
| `-to <dsn>` | Restore into this database. Must exist and be empty. |
| `-to-file <path>` | Skip the restore; write the decrypted `pg_dump` custom archive to disk. |
| `-force` | Allow a non-empty target (adds `--clean --if-exists`), the source database, or overwriting an existing `-to-file` path. |
| `-allow-unverified` | Restore a backup that never passed verification, accepting it may be corrupt. |

Without the flags it refuses: a non-empty target, the source database, an
unverified backup, and overwriting an existing file. The guards exist because
this command runs when someone is in a hurry.

`-allow-unverified` bypasses policy, not integrity — AES-GCM still rejects a
tampered artifact.

The `-to-file` archive is standard `pg_dump` custom format; stock `pg_restore`
reads it with backwyn nowhere in the loop:

```
pg_restore --no-owner --dbname <dsn> ./recovered.pgc
```

## list

```
backwyn list
```

```
ID                SOURCE          PLAINTEXT  VERIFIED  TABLES  NOTE
20260715T120000Z  host/db         8123456    yes       24
20260715T060000Z  host/db         8120012    NO        0       verify query: ...
```

| Column | Meaning |
|---|---|
| `ID` | Backup ID; pass to `verify` / `restore`. |
| `PLAINTEXT` | Dump size in bytes, before encryption. |
| `VERIFIED` | `yes` only if this backup has actually been restored. |
| `TABLES` | Tables counted in the verification restore. |
| `NOTE` | Why verification failed, when it did. |

## check

```
backwyn check [-max-age 24h]
```

The monitoring integration point. Answers one question: *is there a verified
backup newer than `-max-age`?* Exits **3** if not — meaning *you do not
currently have a provably good backup* — which is a different and more useful
fact than "a job errored".

Warns (stderr, exit still 0) when coverage is healthy but the newest backup
failed verification: an older backup is carrying you.

## prune

```
backwyn prune [-keep-last N] [-keep-daily N] [-keep-weekly N] [-keep-monthly N]
              [-dry-run] [-sweep-orphans]
```

Applies the retention policy. Prints every keep/delete decision with its
reason — run `-dry-run` first, always.

Only **verified** backups fill retention slots. Three rules hold regardless of
policy: nothing is pruned unless at least one backup is verified; the newest
verified backup is never deleted; the newest backup is never deleted (it may
be mid-verification). With no `-keep-*` flags the policy is empty and prune
deletes nothing.

`-sweep-orphans` also removes artifacts older than 24h that have no manifest —
leftovers of an interrupted prune.

## run

```
backwyn run [-interval 6h] [-max-age 24h] [-once] [-listen :8080] [-keep-* N]
```

The daemon: backup → verify → check → prune, on a timer. The coverage check
runs every cycle even if that cycle's backup or verify failed — an older
verified backup may still hold coverage. Prune runs only on a fully healthy
cycle.

| Flag | Default | Meaning |
|---|---|---|
| `-interval` | `6h` | Time between cycles. |
| `-max-age` | `24h` | Alert if no verified backup is newer than this. |
| `-once` | | Run a single cycle and exit — for external cron. |
| `-listen` | `:8080` | Address for `/healthz` + `/metrics`; `""` disables. Ignored with `-once`. |
| `-keep-*` | `0` | Retention policy, same flags as `prune`. |

Alerts go to `BACKWYN_ALERT_WEBHOOK`; endpoint semantics are in
[operations.md](operations.md#monitoring).

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Success |
| `1` | Error |
| `2` | Usage error |
| `3` | `check` only: no verified backup within `-max-age` |
