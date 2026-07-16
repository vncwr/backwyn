// package pgtools wraps postgres client binaries.
package pgtools

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
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

// dumpargs builds the pg_dump argument list. schemas scopes the dump with -n
// flags; empty means the whole database. rowSecurity dumps rls tables under
// the role's policies instead of refusing — callers must first prove the
// role sees every row (see RLSUncoveredTables) or the dump is silently
// incomplete.
func DumpArgs(dsn, outPath string, schemas []string, rowSecurity bool) []string {
	args := []string{
		"--format=custom",
		"--no-owner",
		"--no-privileges",
	}
	if rowSecurity {
		args = append(args, "--enable-row-security")
	}
	for _, s := range schemas {
		if s = strings.TrimSpace(s); s != "" {
			args = append(args, "-n", s)
		}
	}
	return append(args, "--file", outPath, "--dbname", dsn)
}

// dump runs pg_dump in custom format against dsn.
func Dump(ctx context.Context, dsn, outPath string, schemas []string, rowSecurity bool) error {
	cmd := exec.CommandContext(ctx, "pg_dump", DumpArgs(dsn, outPath, schemas, rowSecurity)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pg_dump failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// rlsuncoveredtables lists rls-enabled tables the connected role cannot
// provably read in full: tables with no permissive allow-all select policy
// for the role, or with a restrictive policy that filters it. run as a
// preflight before dumping with --enable-row-security, where such a table
// silently dumps only its visible subset. schemas limits the check to the
// schemas being dumped; empty checks all user schemas.
func RLSUncoveredTables(ctx context.Context, dsn string, schemas []string) ([]string, error) {
	scope := ""
	if clean := cleanIdents(schemas); len(clean) > 0 {
		lits := make([]string, len(clean))
		for i, s := range clean {
			lits[i] = quoteLiteral(s)
		}
		scope = "AND n.nspname IN (" + strings.Join(lits, ", ") + ") "
	}
	// a policy applies to the connected role when its role list is PUBLIC
	// (oid 0) or contains a role whose privileges the current role inherits.
	q := `SELECT n.nspname || '.' || c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND c.relrowsecurity
  AND n.nspname NOT IN ('pg_catalog', 'information_schema') ` + scope + `
  AND (
    NOT EXISTS (
      SELECT 1 FROM pg_policy p
      WHERE p.polrelid = c.oid
        AND p.polpermissive
        AND p.polcmd IN ('r', '*')
        AND pg_get_expr(p.polqual, p.polrelid) = 'true'
        AND (p.polroles = '{0}'::oid[] OR EXISTS (
          SELECT 1 FROM unnest(p.polroles) r WHERE pg_has_role(current_user, r, 'USAGE')))
    )
    OR EXISTS (
      SELECT 1 FROM pg_policy p
      WHERE p.polrelid = c.oid
        AND NOT p.polpermissive
        AND p.polcmd IN ('r', '*')
        AND pg_get_expr(p.polqual, p.polrelid) <> 'true'
        AND (p.polroles = '{0}'::oid[] OR EXISTS (
          SELECT 1 FROM unnest(p.polroles) r WHERE pg_has_role(current_user, r, 'USAGE')))
    )
  )
ORDER BY 1`
	out, err := psqlQuery(ctx, dsn, q)
	if err != nil {
		return nil, err
	}
	var tables []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			tables = append(tables, line)
		}
	}
	return tables, nil
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

	// sandbox restores into a fresh scratch database: POLICY entries are
	// skipped (they reference roles that only exist on the source) and the
	// public schema is not recreated (a fresh database already has one).
	Sandbox bool
}

// restore restores archivePath into targetDSN.
func Restore(ctx context.Context, targetDSN, archivePath string, opts RestoreOptions) error {
	args := []string{"--no-owner", "--no-privileges"}
	if opts.Clean {
		args = append(args, "--clean", "--if-exists")
	}
	if opts.Sandbox {
		listPath, cleanup, err := sandboxTOC(ctx, archivePath)
		if err != nil {
			return err
		}
		defer cleanup()
		args = append(args, "--use-list", listPath)
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

// toc entries a scratch sandbox cannot satisfy: policies name source-only
// roles, and a fresh database already owns a public schema.
var sandboxSkip = regexp.MustCompile(`^\d+; \d+ \d+ (POLICY |SCHEMA - public( |$))`)

// filtersandboxtoc comments out sandbox-unsatisfiable entries in a
// pg_restore --list toc, preserving the rest verbatim.
func FilterSandboxTOC(toc string) string {
	lines := strings.Split(toc, "\n")
	for i, line := range lines {
		if sandboxSkip.MatchString(line) {
			lines[i] = ";" + line
		}
	}
	return strings.Join(lines, "\n")
}

// sandboxtoc writes the filtered toc to a temp file for pg_restore --use-list.
func sandboxTOC(ctx context.Context, archivePath string) (string, func(), error) {
	toc, err := RestoreList(ctx, archivePath)
	if err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp("", "backwyn-toc-*.list")
	if err != nil {
		return "", nil, fmt.Errorf("create toc list file: %w", err)
	}
	path := f.Name()
	cleanup := func() { os.Remove(path) }
	if _, err := f.WriteString(FilterSandboxTOC(toc)); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("write toc list file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("close toc list file: %w", err)
	}
	return path, cleanup, nil
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

// quoteliteral single quotes a sql string literal.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// cleanidents trims entries and drops blanks.
func cleanIdents(ids []string) []string {
	var out []string
	for _, s := range ids {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
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
