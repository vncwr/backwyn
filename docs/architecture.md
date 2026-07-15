# Architecture

backwyn takes a Postgres backup, then immediately proves it restores. Everything
else follows from that second step.

## The cycle

One `run` cycle is four stages. The daemon and `run -once` (cron) drive the same
code path, so scheduled and cron runs behave identically.

```
backup ──> verify ──> check ──> prune
```

**backup** — `pg_dump --format=custom` to a temp file, SHA-256 the plaintext,
stream it through AES-256-GCM into the storage backend, write the manifest.

**verify** — fetch the artifact back out of storage, decrypt, re-check the
plaintext SHA-256 against the manifest, confirm `pg_restore --list` parses the
archive, restore it into a throwaway database, count user tables. Only then is
the manifest stamped `verified: true`.

**check** — ask a different question: *is there a verified backup newer than
`-max-age`?* Not "did anything error". Exit 3 and fire the alert webhook if not.

**prune** — apply the retention policy. Runs only after coverage is confirmed
healthy, so nothing is deleted on a cycle that is unsure about the new backup.

## Why verify is the product

A backup job that silently stops running produces **no error to alert on**. A
corrupted artifact produces no error until someone tries to restore it — which
is, by definition, the worst possible moment to find out.

Verification collapses that discovery forward in time. The backup that would
have failed at restore fails tonight, on a throwaway database, while production
is still healthy. It also makes absence-alerting possible at all: you can only
ask "is there a good backup right now" if something is continuously proving
backups good.

## Invariants

These are load-bearing. Changing one changes correctness.

**The manifest is written last.** Its existence implies a complete artifact.
`backup` uploads bytes, then writes the manifest.

**Prune deletes in reverse: manifest first, then artifact.** This preserves the
invariant above under a crash. An interrupted prune leaves an orphaned artifact
— wasted space, invisible to `check` and `list`. The opposite order would leave
a manifest advertising a verified backup whose bytes are gone: coverage that
reports healthy and cannot restore. Orphans are reclaimed by `-sweep-orphans`,
which has a 24h age guard because an in-flight backup is indistinguishable from
an orphan.

**Retention slots are filled by verified backups only.** An unverified artifact
is not coverage and does not occupy a daily slot a real backup could hold.

**Three prune rules hold regardless of policy:** nothing is pruned unless
something is verified; the newest verified backup is never deleted; the newest
backup is never deleted (it may be mid-verification). An empty policy prunes
nothing, so a forgotten flag cannot wipe history.

**`Decrypt` gates on the error, not on output length.** It streams, so on error
`dst` may already hold output — possibly byte-complete. A stream missing only
its 4-byte terminator yields every plaintext byte before truncation is caught.
Callers must discard `dst` on any error.

**Materialization is shared.** `verify` and `restore` drive the same
`internal/artifact` path, so the recovery code an outage depends on is exercised
on every cycle.

## Packages

```
cmd/backwyn/        CLI (backup | verify | restore | list | check | prune | run)
internal/config/    env-based configuration
internal/pgtools/   every exec.Command for pg_dump/pg_restore/psql
internal/crypto/    streaming AES-256-GCM
internal/storage/   Backend interface + Local + S3/R2
internal/manifest/  backup metadata + verification record
internal/artifact/  fetch -> decrypt -> checksum (shared by verify + restore)
internal/backup/    backup orchestration
internal/verify/    restore-verification orchestration
internal/restore/   restore to a database or to a plain archive
internal/retention/ GFS prune policy + safety rules
internal/check/     absence-alerting evaluation
internal/alert/     webhook / no-op alerters
internal/runner/    one full cycle, for the daemon
```

## Storage layout

```
artifacts/<id>.dump.enc        AES-256-GCM encrypted pg_dump custom archive
manifests/<id>.manifest.json   unencrypted metadata (holds no secrets)
```

`<id>` is the backup timestamp, `20060102T150405Z`. Because the id *is* the
timestamp, orphan sweeping can date an artifact without reading object metadata.

Manifests are unencrypted deliberately: `check` and `list` must work without the
encryption key.

## Artifact format

```
"BWY1"                                    4-byte magic + format version
[uint32 len][12-byte nonce][ciphertext]   repeating frame
[uint32 0]                                terminator
```

Fresh random nonce per frame. The trailing digit in the magic is the format
version — bump it if the frame layout changes, so old readers reject new
artifacts loudly instead of misparsing them.

The terminator frame is what makes truncation detectable: without it, a cut
stream would decode as a complete, shorter plaintext.

## Trust boundaries

- The source DSN should be a least-privilege read-only role, never the owner.
  See [supabase.md](supabase.md) and `sql/backup_role.sql`.
- The encryption key never leaves the machine. Artifacts are encrypted before
  upload. Lose the key and every backup is permanently unreadable.
- The bucket is the customer's. Data never lands in a third-party account.
- The verify sandbox is a throwaway Postgres, never the source database.

## Known constraints

**Verification cost scales with database size.** `verify` performs a full
restore every cycle. That is cheap for a sub-GB database and expensive for a
50GB one. `check` reads manifests only, so backup and verify cadence can be
decoupled — back up every 6h, deep-verify daily — without touching the design.

**Single database per process.** The daemon protects one source DSN.
