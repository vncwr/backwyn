# backwyn docs

Postgres backups that prove they restore. If you're new, read in this order:

| Doc | What it answers |
|---|---|
| [getting-started.md](getting-started.md) | Zero to a verified backup, including one restore drill. Start here. |
| [configuration.md](configuration.md) | Every environment variable, the flags, exit codes. |
| [commands.md](commands.md) | Reference for each command: `backup`, `verify`, `restore`, `list`, `check`, `prune`, `run`. |
| [operations.md](operations.md) | Running it for real: monitoring, the restore runbook, failure modes, retention, cost. |
| [architecture.md](architecture.md) | How it works — the cycle, the invariants, the artifact format, trust boundaries. |
| [supabase.md](supabase.md) | Supabase specifics: least-privilege role, which connection string, SSL. |

## The 60-second version

Every cycle, backwyn takes a `pg_dump`, encrypts it into a bucket **you** own,
then pulls it back out and **restores it into a throwaway database**. Only a
backup that has actually been restored counts as coverage. `check` exits 3 —
and the webhook fires — when there is no proven backup within your freshness
window, which catches the failure no backup tool alerts on: the job that
silently stopped running.

Getting data back is two commands, and one of them is optional:

```
backwyn restore <id> -to postgresql://...          # into a fresh database
backwyn restore <id> -to-file ./recovered.pgc      # or a plain pg_dump archive
```

The second form needs nothing from backwyn ever again — stock `pg_restore`
reads it.
