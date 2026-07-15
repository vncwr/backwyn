#!/usr/bin/env bash
# localtest.sh — end-to-end self-test for backwyn on Linux/macOS.
#
# The POSIX twin of localtest.ps1. It exercises the whole engine against a real
# PostgreSQL:
#   backup -> verify -> check(healthy) -> restore round-trip (row-count diff vs
#   source) -> restore guards -> restore to plain archive -> corruption
#   detection -> check(alert) -> restore refuses unverified -> prune safety
#   rules -> prune retention policy
#
# Unlike localtest.ps1 (which starts its own throwaway postmaster), this expects
# a PostgreSQL to already be listening — a service container in CI, or a local
# one you started yourself:
#
#   docker run --rm -d -p 5432:5432 -e POSTGRES_PASSWORD=postgres postgres:17
#   ./scripts/localtest.sh
#
# It creates and drops its own databases; it never touches an existing one.

set -uo pipefail

PGHOST="${PGHOST:-localhost}"
PGPORT="${PGPORT:-5432}"
PGUSER="${PGUSER:-postgres}"
export PGPASSWORD="${PGPASSWORD:-postgres}"
export PGHOST PGPORT PGUSER

PROJ="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$PROJ/backwyn"
WORK="$(mktemp -d)"
STORE="$WORK/storage"
FAIL=0

# Every database this script creates, so teardown can drop them all.
TEST_DBS=(sourcedb restoredb plaindb unverifieddb)

log()  { printf '\n%s\n' "$*"; }
pass() { printf '  \033[32mPASS\033[0m: %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m: %s\n' "$1"; FAIL=1; }
check() { if [ "$1" = "0" ]; then pass "$2"; else fail "$2"; fi; }

# check_eq compares two values so failures print what was actually seen.
check_eq() {
  if [ "$1" = "$2" ]; then pass "$3"; else fail "$3 (got '$1', want '$2')"; fi
}

psql_q() { psql -d "$1" -t -A -v ON_ERROR_STOP=1 -c "$2"; }

# counts returns "customers/orders" row counts for a database.
counts() { psql_q "$1" "SELECT (SELECT count(*) FROM customers) || '/' || (SELECT count(*) FROM orders);"; }

# backup_count counts manifests via the CLI's own list output, so assertions see
# exactly what an operator would.
backup_count() { "$BIN" list | grep -cE '^[0-9]{8}T[0-9]{6}Z' || true; }

# latest_id returns the newest backup id from list output.
latest_id() { "$BIN" list | grep -E '^[0-9]{8}T[0-9]{6}Z' | tail -1 | awk '{print $1}'; }

cleanup() {
  log "Tearing down ..."
  for db in "${TEST_DBS[@]}"; do
    psql -d postgres -q -c "DROP DATABASE IF EXISTS $db WITH (FORCE);" >/dev/null 2>&1 || true
  done
  # Scratch databases from verify are dropped by the engine, but a failed run
  # can leak one; sweep them so a re-run starts clean.
  psql -d postgres -t -A -c \
    "SELECT datname FROM pg_database WHERE datname LIKE 'backwyn_verify_%';" 2>/dev/null \
    | while read -r db; do
        [ -n "$db" ] && psql -d postgres -q -c "DROP DATABASE IF EXISTS \"$db\" WITH (FORCE);" >/dev/null 2>&1 || true
      done
  rm -rf "$WORK"
}
trap cleanup EXIT

log "[1/14] Building backwyn ..."
( cd "$PROJ" && go build -o "$BIN" ./cmd/backwyn ) || { echo "build failed"; exit 1; }

log "[2/14] Waiting for PostgreSQL at $PGHOST:$PGPORT ..."
for i in $(seq 1 60); do
  pg_isready -q && break
  [ "$i" = "60" ] && { echo "PostgreSQL never became ready"; exit 1; }
  sleep 0.5
done
# pg_dump must be at least as new as the server, or every backup fails.
echo "  server:  $(psql_q postgres 'SHOW server_version;')"
echo "  pg_dump: $(pg_dump --version)"

log "[3/14] Seeding sample data ..."
psql -d postgres -q -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS sourcedb WITH (FORCE);"
psql -d postgres -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE sourcedb;"
psql -d sourcedb -q -v ON_ERROR_STOP=1 -c "
  CREATE TABLE customers(id serial primary key, name text);
  INSERT INTO customers(name) SELECT 'c'||g FROM generate_series(1,250) g;
  CREATE TABLE orders(id serial primary key, customer_id int, amount numeric);
  INSERT INTO orders(customer_id, amount)
    SELECT (random()*249+1)::int,(random()*100)::numeric(6,2) FROM generate_series(1,1000);"

export BACKWYN_SOURCE_DSN="postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/sourcedb?sslmode=disable"
export BACKWYN_VERIFY_ADMIN_DSN="postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/postgres?sslmode=disable"
export BACKWYN_STORAGE_DIR="$STORE"
export BACKWYN_ENCRYPTION_KEY="$(head -c32 /dev/urandom | base64)"
export BACKWYN_VERIFY_QUERY="SELECT count(*) FROM customers;"

log "[4/14] backup"
OUT="$("$BIN" backup)"; RC=$?
echo "$OUT" | sed 's/^/    /'
ID="$(echo "$OUT" | sed -n 's/^backed up \([^ ]*\).*/\1/p')"
check "$([ $RC -eq 0 ] && [ -n "$ID" ] && echo 0 || echo 1)" "backup created ($ID)"

log "[5/14] verify (clean backup should VERIFY)"
"$BIN" verify "$ID"; check $? "clean backup verified"

log "[5.5/14] verify query failure (should fail verification)"
BACKWYN_VERIFY_QUERY="SELECT count(*) FROM non_existent_table;" "$BIN" verify "$ID" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "failed verification query fails verify"

# the failed verify above re-stamped the manifest UNVERIFIED; re-verify with
# the good query so the rest of the suite sees a verified backup again.
"$BIN" verify "$ID" >/dev/null
check $? "re-verify with a passing query restores verified status"

log "[6/14] check -max-age 24h (should be OK / exit 0)"
"$BIN" check -max-age 24h; check $? "coverage healthy when a fresh verified backup exists"

log "[7/14] restore round-trip (restore into a fresh DB; data MUST match source)"
# The claim under test: a backup is not just 'verified', it actually gives the
# data back. Compare real row counts, not just that the command exited 0.
SRC_COUNTS="$(counts sourcedb)"
psql -d postgres -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE restoredb;"
RESTORE_DSN="postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/restoredb?sslmode=disable"
"$BIN" restore "$ID" -to "$RESTORE_DSN"; check $? "restore into a fresh database succeeds"
check_eq "$(counts restoredb)" "$SRC_COUNTS" "restored data matches source"

log "[8/14] restore guards (must refuse non-empty target and the source DB)"
"$BIN" restore "$ID" -to "$RESTORE_DSN" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "refuses to restore into a non-empty database"
"$BIN" restore "$ID" -to "$BACKWYN_SOURCE_DSN" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "refuses to restore over the SOURCE database"
# -force is the documented escape hatch; it must work AND replace, not duplicate.
"$BIN" restore "$ID" -to "$RESTORE_DSN" -force >/dev/null; check $? "-force permits restoring into a non-empty database"
check_eq "$(counts restoredb)" "$SRC_COUNTS" "-force replaces contents rather than duplicating"

log "[9/14] restore -to-file (escape hatch: plain archive, no lock-in)"
PLAIN="$WORK/escape.pgc"
"$BIN" restore "$ID" -to-file "$PLAIN" >/dev/null
check "$([ $? -eq 0 ] && [ -f "$PLAIN" ] && echo 0 || echo 1)" "decrypted archive written to disk"
# The real test of 'no lock-in': stock pg_restore must read it, no backwyn involved.
psql -d postgres -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE plaindb;"
pg_restore --no-owner --no-privileges -d "postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/plaindb?sslmode=disable" "$PLAIN"
check_eq "$(counts plaindb)" "$SRC_COUNTS" "stock pg_restore reads the archive without backwyn"
"$BIN" restore "$ID" -to-file "$PLAIN" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "refuses to overwrite an existing -to-file path"

log "[10/14] corruption detection (tamper the artifact; verify MUST fail)"
ARTIFACT="$STORE/artifacts/$ID.dump.enc"
SIZE=$(wc -c < "$ARTIFACT")
printf '\xFF' | dd of="$ARTIFACT" bs=1 seek=$((SIZE / 2)) count=1 conv=notrunc status=none
"$BIN" verify "$ID" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "corrupted backup FAILS verification (silent-failure catch)"

log "[11/14] check -max-age 1ns (should ALERT / exit 3)"
"$BIN" check -max-age 1ns >/dev/null 2>&1
check_eq "$?" "3" "absence-alerting fires (exit 3) when no fresh verified backup"

log "[12/14] restore refuses an unverified backup"
psql -d postgres -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE unverifieddb;"
UNVER_DSN="postgresql://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/unverifieddb?sslmode=disable"
"$BIN" restore "$ID" -to "$UNVER_DSN" >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "refuses to restore a backup that failed verification"
# Even when forced past that guard, AES-GCM must still catch the tampering.
"$BIN" restore "$ID" -to "$UNVER_DSN" -allow-unverified >/dev/null 2>&1
check "$([ $? -ne 0 ] && echo 0 || echo 1)" "-allow-unverified still fails on a corrupt artifact (AES-GCM)"

log "[13/14] prune refuses to delete while NOTHING is verified (safety rule 1)"
# The only backup is the corrupted one. Pruning here would destroy the last
# artifact anyone could still salvage by hand.
"$BIN" prune -keep-last 1 >/dev/null; check $? "prune exits cleanly when it declines to act"
check_eq "$(backup_count)" "1" "prune deleted nothing while no backup is verified"

log "[14/14] prune retention policy"
# Fresh verified backups. IDs are second-granular, so space them out.
sleep 1; "$BIN" backup >/dev/null; ID2="$(latest_id)"; "$BIN" verify "$ID2" >/dev/null
sleep 1; "$BIN" backup >/dev/null; ID3="$(latest_id)"; "$BIN" verify "$ID3" >/dev/null
check_eq "$(backup_count)" "3" "three backups present before pruning"
# No -keep flags: an unconfigured policy must never delete anything.
"$BIN" prune >/dev/null
check_eq "$(backup_count)" "3" "prune with no policy keeps everything"
"$BIN" prune -keep-last 1 -dry-run >/dev/null
check_eq "$(backup_count)" "3" "-dry-run deletes nothing"
"$BIN" prune -keep-last 1 >/dev/null
check_eq "$(backup_count)" "1" "prune -keep-last 1 removes the other two"
check_eq "$(latest_id)" "$ID3" "the surviving backup is the newest verified one"
"$BIN" check -max-age 24h >/dev/null; check $? "coverage still healthy after pruning"
check_eq "$(ls -1 "$STORE/artifacts" | wc -l | tr -d ' ')" "1" "pruned artifacts are deleted, not left orphaned"

log "---- final list ----"
"$BIN" list

if [ "$FAIL" -ne 0 ]; then
  printf '\n\033[31mRESULT: SOME CHECKS FAILED\033[0m\n'; exit 1
fi
printf '\n\033[32mRESULT: ALL CHECKS PASSED\033[0m\n'
