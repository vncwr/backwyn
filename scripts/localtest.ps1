# localtest.ps1 — end-to-end self-test for backwyn.
#
# Stands up an isolated, throwaway PostgreSQL instance (trust auth, custom
# port), seeds sample data, then exercises the whole engine:
#   backup -> verify -> check(healthy) -> restore round-trip (row-count diff vs
#   source) -> restore guards -> restore to plain archive -> corruption
#   detection -> check(alert) -> restore refuses unverified -> prune safety
#   rules -> prune retention policy
# and tears the instance down. Nothing touches any real database.
#
# Run from anywhere:  powershell -File scripts\localtest.ps1
# It locates Go and PostgreSQL automatically (no PATH setup needed).

$ErrorActionPreference = "Stop"

function Find-One($patterns) {
  foreach ($p in $patterns) {
    $hit = Get-ChildItem $p -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($hit) { return $hit.FullName }
  }
  return $null
}

$proj = Split-Path $PSScriptRoot -Parent
$go = Find-One @("C:\Program Files\Go\bin\go.exe", "$env:LOCALAPPDATA\Programs\Go\bin\go.exe")
$initdb = Find-One @("C:\Program Files\PostgreSQL\*\bin\initdb.exe")
if (-not $go)     { throw "Go not found. Install it (winget install GoLang.Go) and retry." }
if (-not $initdb) { throw "PostgreSQL not found. Install it (winget install PostgreSQL.PostgreSQL.17) and retry." }
$pgbin = Split-Path $initdb

$port = 55450
$stamp = Get-Date -Format "yyyyMMddHHmmss"
$work = Join-Path $env:TEMP "backwyn-localtest-$stamp"
$data = Join-Path $work "data"
$store = Join-Path $work "storage"
New-Item -ItemType Directory -Force $work | Out-Null

$fail = $false
$pg = $null
function Check($cond, $label) {
  if ($cond) { Write-Host "  PASS: $label" -ForegroundColor Green }
  else { Write-Host "  FAIL: $label" -ForegroundColor Red; $script:fail = $true }
}

# BackupCount counts manifests via the CLI's own list output, so the assertions
# see exactly what an operator would.
function BackupCount {
  return (& $script:backwyn list | Select-String -Pattern '^\d{8}T\d{6}Z').Count
}

try {
  Write-Host "`n[1/8] Building backwyn.exe ..."
  $env:GOFLAGS = "-mod=mod"
  & $go -C $proj build -o "$proj\backwyn.exe" ./cmd/backwyn
  if (-not $?) { throw "build failed" }
  $backwyn = "$proj\backwyn.exe"
  $script:backwyn = $backwyn # BackupCount runs in its own scope

  Write-Host "[2/8] Starting throwaway Postgres on port $port ..."
  & "$pgbin\initdb.exe" -D $data -U postgres --auth=trust -E UTF8 --no-locale | Out-Null
  # Launch postgres.exe directly (detached, -PassThru). We avoid `pg_ctl start`
  # because on some Windows setups pg_ctl does not exit, which makes any wait on
  # it hang. postgres.exe writes startup logs to stderr; we capture them and
  # poll pg_isready for readiness.
  $pg = Start-Process -FilePath "$pgbin\postgres.exe" `
    -ArgumentList @("-D", $data, "-p", "$port") `
    -NoNewWindow -PassThru -RedirectStandardError (Join-Path $work "pg.log")
  $prevEAP = $ErrorActionPreference
  $ErrorActionPreference = "Continue"
  $ready = $false
  for ($i = 0; $i -lt 60; $i++) {
    & "$pgbin\pg_isready.exe" -h localhost -p $port -q | Out-Null
    if ($LASTEXITCODE -eq 0) { $ready = $true; break }
    Start-Sleep -Milliseconds 500
  }
  $ErrorActionPreference = $prevEAP
  if (-not $ready) { throw "Postgres did not become ready on port $port" }

  Write-Host "[3/8] Seeding sample data ..."
  & "$pgbin\psql.exe" -p $port -U postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE sourcedb;" | Out-Null
  & "$pgbin\psql.exe" -p $port -U postgres -d sourcedb -v ON_ERROR_STOP=1 -c "CREATE TABLE customers(id serial primary key, name text); INSERT INTO customers(name) SELECT 'c'||g FROM generate_series(1,250) g; CREATE TABLE orders(id serial primary key, customer_id int, amount numeric); INSERT INTO orders(customer_id, amount) SELECT (random()*249+1)::int,(random()*100)::numeric(6,2) FROM generate_series(1,1000);" | Out-Null

  $env:BACKWYN_SOURCE_DSN = "postgresql://postgres@localhost:$port/sourcedb?sslmode=disable"
  $env:BACKWYN_VERIFY_ADMIN_DSN = "postgresql://postgres@localhost:$port/postgres?sslmode=disable"
  $env:BACKWYN_STORAGE_DIR = $store
  $b = New-Object byte[] 32
  [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
  $env:BACKWYN_ENCRYPTION_KEY = [Convert]::ToBase64String($b)
  $env:BACKWYN_VERIFY_QUERY = "SELECT count(*) FROM customers;"
  $env:Path = "$pgbin;$env:Path"

  Write-Host "[4/8] backup"
  $out = & $backwyn backup
  $out | ForEach-Object { Write-Host "    $_" }
  $id = ($out | Select-String -Pattern 'backed up (\S+)').Matches.Groups[1].Value
  Check ($LASTEXITCODE -eq 0 -and $id) "backup created ($id)"

  Write-Host "[5/8] verify (clean backup should VERIFY)"
  & $backwyn verify $id
  Check ($LASTEXITCODE -eq 0) "clean backup verified"

  Write-Host "[5.5/8] verify query failure (should fail verification)"
  $env:BACKWYN_VERIFY_QUERY = "SELECT count(*) FROM non_existent_table;"
  & $backwyn verify $id
  Check ($LASTEXITCODE -ne 0) "failed verification query fails verify"
  $env:BACKWYN_VERIFY_QUERY = "SELECT count(*) FROM customers;"

  Write-Host "[6/8] check -max-age 24h (should be OK / exit 0)"
  & $backwyn check -max-age 24h
  Check ($LASTEXITCODE -eq 0) "coverage healthy when a fresh verified backup exists"

  Write-Host "[7/12] restore round-trip (restore into a fresh DB; data MUST match source)"
  # The claim under test: a backup is not just 'verified', it actually gives the
  # data back. Compare real row counts, not just that the command exited 0.
  $srcCounts = (& "$pgbin\psql.exe" -p $port -U postgres -d sourcedb -t -A -c `
      "SELECT (SELECT count(*) FROM customers) || '/' || (SELECT count(*) FROM orders);").Trim()
  & "$pgbin\psql.exe" -p $port -U postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE restoredb;" | Out-Null
  $restoreDSN = "postgresql://postgres@localhost:$port/restoredb?sslmode=disable"
  & $backwyn restore $id -to $restoreDSN
  $restoreOK = ($LASTEXITCODE -eq 0)
  $dstCounts = (& "$pgbin\psql.exe" -p $port -U postgres -d restoredb -t -A -c `
      "SELECT (SELECT count(*) FROM customers) || '/' || (SELECT count(*) FROM orders);").Trim()
  Check ($restoreOK) "restore into a fresh database succeeds"
  Check ($srcCounts -eq $dstCounts -and $srcCounts) "restored data matches source (customers/orders = $srcCounts vs $dstCounts)"

  Write-Host "[8/12] restore guards (must refuse non-empty target and the source DB)"
  # restoredb now has data, so a second restore must be refused.
  & $backwyn restore $id -to $restoreDSN
  Check ($LASTEXITCODE -ne 0) "refuses to restore into a non-empty database"
  & $backwyn restore $id -to $env:BACKWYN_SOURCE_DSN
  Check ($LASTEXITCODE -ne 0) "refuses to restore over the SOURCE database"
  # -force is the documented escape hatch; it must actually work, and must
  # REPLACE the contents rather than duplicating them into the existing tables.
  & $backwyn restore $id -to $restoreDSN -force
  $forceOK = ($LASTEXITCODE -eq 0)
  $forceCounts = (& "$pgbin\psql.exe" -p $port -U postgres -d restoredb -t -A -c `
      "SELECT (SELECT count(*) FROM customers) || '/' || (SELECT count(*) FROM orders);").Trim()
  Check ($forceOK) "-force permits restoring into a non-empty database"
  Check ($forceCounts -eq $srcCounts) "-force replaces contents rather than duplicating ($forceCounts)"

  Write-Host "[9/12] restore -to-file (escape hatch: plain archive, no lock-in)"
  $plain = Join-Path $work "escape.pgc"
  & $backwyn restore $id -to-file $plain
  Check ($LASTEXITCODE -eq 0 -and (Test-Path $plain)) "decrypted archive written to disk"
  # The real test of 'no lock-in': stock pg_restore must read it with no backwyn involved.
  & "$pgbin\psql.exe" -p $port -U postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE plaindb;" | Out-Null
  & "$pgbin\pg_restore.exe" --no-owner --no-privileges -d "postgresql://postgres@localhost:$port/plaindb?sslmode=disable" $plain
  $plainCounts = (& "$pgbin\psql.exe" -p $port -U postgres -d plaindb -t -A -c `
      "SELECT (SELECT count(*) FROM customers) || '/' || (SELECT count(*) FROM orders);").Trim()
  Check ($plainCounts -eq $srcCounts) "stock pg_restore reads the archive without backwyn ($plainCounts)"
  # Must not silently clobber an existing file.
  & $backwyn restore $id -to-file $plain
  Check ($LASTEXITCODE -ne 0) "refuses to overwrite an existing -to-file path"

  Write-Host "[10/12] corruption detection (tamper the artifact; verify MUST fail)"
  $artifact = Join-Path $store "artifacts\$id.dump.enc"
  $fs = [IO.File]::Open($artifact, 'Open', 'ReadWrite')
  $mid = [long]($fs.Length / 2); $fs.Position = $mid
  $orig = $fs.ReadByte(); $fs.Position = $mid; $fs.WriteByte(($orig -bxor 0xFF)); $fs.Close()
  # These calls are EXPECTED to fail. Do not use `2>&1` here: in PowerShell 5.1
  # merging a native exe's stderr under $ErrorActionPreference='Stop' raises a
  # terminating NativeCommandError. Calling plainly prints stderr and leaves
  # the real exit code in $LASTEXITCODE.
  & $backwyn verify $id
  Check ($LASTEXITCODE -ne 0) "corrupted backup FAILS verification (silent-failure catch)"

  Write-Host "[11/12] check -max-age 1ns (should ALERT / exit 3)"
  & $backwyn check -max-age 1ns
  Check ($LASTEXITCODE -eq 3) "absence-alerting fires when no fresh verified backup"

  Write-Host "[12/14] restore refuses an unverified backup"
  # The failed verify above marked this backup unverified. Restoring it would
  # hand back corrupt data, so it must be refused unless explicitly allowed.
  & "$pgbin\psql.exe" -p $port -U postgres -v ON_ERROR_STOP=1 -c "CREATE DATABASE unverifieddb;" | Out-Null
  $unverDSN = "postgresql://postgres@localhost:$port/unverifieddb?sslmode=disable"
  & $backwyn restore $id -to $unverDSN
  Check ($LASTEXITCODE -ne 0) "refuses to restore a backup that failed verification"
  # Even when forced past that guard, AES-GCM must still catch the tampering.
  & $backwyn restore $id -to $unverDSN -allow-unverified
  Check ($LASTEXITCODE -ne 0) "-allow-unverified still fails on a corrupt artifact (AES-GCM)"

  Write-Host "[13/14] prune refuses to delete while NOTHING is verified (safety rule 1)"
  # The only backup is the corrupted one from step 10. Pruning here would
  # destroy the last artifact anyone could still salvage by hand.
  & $backwyn prune -keep-last 1
  Check ($LASTEXITCODE -eq 0) "prune exits cleanly when it declines to act"
  Check ((BackupCount) -eq 1) "prune deleted nothing while no backup is verified"

  Write-Host "[14/14] prune retention policy"
  # Fresh verified backups. IDs are second-granular, so space them out.
  Start-Sleep -Seconds 1
  & $backwyn backup | Out-Null
  $id2 = (& $backwyn list | Select-String -Pattern '^\d{8}T\d{6}Z' | Select-Object -Last 1).Line.Split()[0]
  & $backwyn verify $id2 | Out-Null
  Start-Sleep -Seconds 1
  & $backwyn backup | Out-Null
  $id3 = (& $backwyn list | Select-String -Pattern '^\d{8}T\d{6}Z' | Select-Object -Last 1).Line.Split()[0]
  & $backwyn verify $id3 | Out-Null
  Check ((BackupCount) -eq 3) "three backups present before pruning"

  # No -keep flags: an unconfigured policy must never delete anything.
  & $backwyn prune | Out-Null
  Check ((BackupCount) -eq 3) "prune with no policy keeps everything"

  & $backwyn prune -keep-last 1 -dry-run | Out-Null
  Check ((BackupCount) -eq 3) "-dry-run deletes nothing"

  & $backwyn prune -keep-last 1 | Out-Null
  Check ((BackupCount) -eq 1) "prune -keep-last 1 removes the other two"
  # The survivor must be the newest VERIFIED backup, never the corrupt one.
  $survivor = (& $backwyn list | Select-String -Pattern '^\d{8}T\d{6}Z').Line.Split()[0]
  Check ($survivor -eq $id3) "the surviving backup is the newest verified one ($survivor)"
  & $backwyn check -max-age 24h
  Check ($LASTEXITCODE -eq 0) "coverage still healthy after pruning"
  # The pruned backups' artifacts must be gone, not orphaned.
  Check ((Get-ChildItem (Join-Path $store "artifacts") -ErrorAction SilentlyContinue).Count -eq 1) `
    "pruned artifacts are deleted, not left orphaned"

  Write-Host "`n---- final list ----"
  & $backwyn list
}
finally {
  Write-Host "`nTearing down test instance ..."
  if (Test-Path (Join-Path $data "postmaster.pid")) {
    Start-Process -FilePath "$pgbin\pg_ctl.exe" `
      -ArgumentList @("-D", $data, "-m", "immediate", "stop") -NoNewWindow -Wait -ErrorAction SilentlyContinue
  }
  # Hard-kill fallback if the postmaster is somehow still alive.
  if ($pg -and -not $pg.HasExited) {
    Stop-Process -Id $pg.Id -Force -ErrorAction SilentlyContinue
  }
}

if ($fail) { Write-Host "`nRESULT: SOME CHECKS FAILED" -ForegroundColor Red; exit 1 }
else { Write-Host "`nRESULT: ALL CHECKS PASSED" -ForegroundColor Green; exit 0 }
