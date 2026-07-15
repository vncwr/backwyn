// package pgtools wraps postgres client binaries.
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

// require checks that the named binaries exist on PATH.
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

// version returns the version string for a binary.
func Version(ctx context.Context, bin string) (string, error) {
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// dump runs pg_dump in custom format against dsn.
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

// restorelist runs pg_restore --list to verify the archive parses.
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

// createdatabase creates a database.
func CreateDatabase(ctx context.Context, adminDSN, name string) error {
	// quote defensively.
	return psqlExec(ctx, adminDSN, fmt.Sprintf("CREATE DATABASE %s", quoteIdent(name)))
}

// dropdatabase drops a database.
func DropDatabase(ctx context.Context, adminDSN, name string) error {
	return psqlExec(ctx, adminDSN, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", quoteIdent(name)))
}

// restoreoptions configures a restore.
type RestoreOptions struct {
	// clean drops objects before recreating them.
	Clean bool
}

// restore restores archivePath into targetDSN.
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

// countusertables returns the number of user tables.
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

// RunQuery executes query against dsn and returns the output.
func RunQuery(ctx context.Context, dsn, query string) (string, error) {
	return psqlQuery(ctx, dsn, query)
}

// quoteident double quotes identifiers.
func quoteIdent(id string) string {
	return `"` + strings.ReplaceAll(id, `"`, `""`) + `"`
}

// withdatabase returns a dsn with a replaced database name.
func WithDatabase(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DSN: %w", err)
	}
	u.Path = "/" + db
	return u.String(), nil
}

// sametarget reports whether two dsns address the same host and database.
func SameTarget(a, b string) bool {
	ua, erra := url.Parse(a)
	ub, errb := url.Parse(b)
	if erra != nil || errb != nil {
		return true
	}
	return strings.EqualFold(ua.Host, ub.Host) && ua.Path == ub.Path
}

// sourcelabel returns a stripped label for the dsn.
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
