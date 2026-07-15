// package pgtools wraps the postgres client binaries the engine shells out to.
// every exec.Command lives here.
package pgtools

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
)

// Require checks the named binaries are on PATH, listing any that are missing.
func Require(bins ...string) error {
	var missing []string
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			missing = append(missing, b)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("required PostgreSQL client tools not found on PATH: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Version returns the version string reported by a client binary.
func Version(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Dump runs pg_dump in custom format against dsn, writing to outPath. Custom
// format is compressed and restorable selectively.
func Dump(ctx context.Context, dsn, outPath string) error {
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--format=custom",
		"--no-owner",
		"--no-privileges",
		"--file", outPath,
		"--dbname", dsn,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RestoreList runs pg_restore --list to confirm the header and TOC parse. A
// cheap structural check that catches truncation without a live server.
func RestoreList(ctx context.Context, archivePath string) (string, error) {
	cmd := exec.CommandContext(ctx, "pg_restore", "--list", archivePath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pg_restore --list failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// CreateDatabase creates a database named name using adminDSN.
func CreateDatabase(ctx context.Context, adminDSN, name string) error {
	// caller validates identifiers; quote defensively regardless.
	return psqlExec(ctx, adminDSN, fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name)))
}

// DropDatabase drops name if it exists, terminating other connections first.
func DropDatabase(ctx context.Context, adminDSN, name string) error {
	return psqlExec(ctx, adminDSN, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name)))
}

// RestoreOptions tunes a restore.
type RestoreOptions struct {
	// Clean drops each object before recreating it (--clean --if-exists).
	// required for a non-empty target, or pg_restore collides on duplicate keys.
	Clean bool
}

// Restore restores archivePath into the database addressed by targetDSN.
func Restore(ctx context.Context, targetDSN, archivePath string, opts RestoreOptions) error {
	args := []string{"--no-owner", "--no-privileges"}
	if opts.Clean {
		args = append(args, "--clean", "--if-exists")
	}
	args = append(args, "--dbname", targetDSN, archivePath)

	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_restore failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// CountUserTables returns the number of tables in non-system schemas: a sanity
// signal that a restore actually has content.
func CountUserTables(ctx context.Context, dsn string) (int, error) {
	const q = "SELECT count(*) FROM information_schema.tables " +
		"WHERE table_type = 'BASE TABLE' " +
		"AND table_schema NOT IN ('pg_catalog', 'information_schema')"
	out, err := psqlQuery(ctx, dsn, q)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("parse table count %q: %w", out, err)
	}
	return n, nil
}

func psqlExec(ctx context.Context, dsn, sql string) error {
	cmd := exec.CommandContext(ctx, "psql", "--dbname", dsn, "-v", "ON_ERROR_STOP=1", "-c", sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql exec failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func psqlQuery(ctx context.Context, dsn, sql string) (string, error) {
	cmd := exec.CommandContext(ctx, "psql", "--dbname", dsn, "-t", "-A", "-v", "ON_ERROR_STOP=1", "-c", sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("psql query failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// quoteIdent quotes a Postgres identifier by doubling embedded quotes.
func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// WithDatabase returns a copy of dsn with its database path replaced by db,
// reusing the admin connection's host/port/credentials.
func WithDatabase(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/" + db
	return u.String(), nil
}

// SameTarget reports whether two DSNs address the same host:port/database.
// fails closed: an unparseable DSN returns true rather than allowing a restore
// over production.
func SameTarget(a, b string) bool {
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil {
		return true
	}
	return strings.EqualFold(ua.Host, ub.Host) && ua.Path == ub.Path
}

// SourceLabel returns a credential-stripped host:port/dbname label, safe to
// record in a manifest.
func SourceLabel(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return "unknown"
	}
	host := u.Host
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" {
		return host
	}
	return host + "/" + db
}
