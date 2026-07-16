// backwyn proves postgres backups restore and alerts when they don't.
// run with no arguments for usage; config is read from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/vncwr/backwyn/internal/alert"
	"github.com/vncwr/backwyn/internal/artifact"
	"github.com/vncwr/backwyn/internal/backup"
	"github.com/vncwr/backwyn/internal/check"
	"github.com/vncwr/backwyn/internal/config"
	"github.com/vncwr/backwyn/internal/manifest"
	"github.com/vncwr/backwyn/internal/restore"
	"github.com/vncwr/backwyn/internal/retention"
	"github.com/vncwr/backwyn/internal/runner"
	"github.com/vncwr/backwyn/internal/storage"
	"github.com/vncwr/backwyn/internal/verify"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	cleanupTempFiles()

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		usage()
		os.Exit(0)
	}
	if err := run(cmd, os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "backwyn: %v\n", err)
		os.Exit(1)
	}
}

func cleanupTempFiles() {
	dir := os.TempDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "backwyn-") && strings.HasSuffix(name, ".pgc") {
			path := filepath.Join(dir, name)
			_ = os.Remove(path)
		}
	}
}

func run(cmd string, args []string) error {
	ctx := context.Background()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := newBackend(cfg)
	if err != nil {
		return err
	}

	switch cmd {
	case "backup":
		res, err := backup.Run(ctx, cfg, store, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("backed up %s (%d bytes plaintext, %d encrypted) -> %s\n",
			res.Manifest.ID, res.Manifest.PlaintextSize, res.Manifest.EncryptedSize, res.Manifest.ArtifactKey)
		fmt.Printf("run 'backwyn verify %s' to prove it restores.\n", res.Manifest.ID)
		return nil

	case "verify":
		if len(args) != 1 {
			return fmt.Errorf("usage: backwyn verify <backup-id>")
		}
		m, err := verify.Run(ctx, cfg, store, args[0], time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("VERIFIED %s: checksum ok, archive listable, restored %d tables at %s\n",
			m.ID, m.Verification.TableCount, m.Verification.VerifiedAt.Format(time.RFC3339))
		return nil

	case "restore":
		return runRestore(ctx, cfg, store, args)

	case "list":
		return list(ctx, store)

	case "check":
		return runCheck(ctx, store, args)

	case "prune":
		return runPrune(ctx, store, args)

	case "run":
		return runDaemon(cfg, store, args)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func list(ctx context.Context, store storage.Backend) error {
	keys, err := store.List(ctx, "manifests/")
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSOURCE\tPLAINTEXT\tVERIFIED\tTABLES\tNOTE")
	for _, k := range keys {
		rc, err := store.Get(ctx, k)
		if err != nil {
			return err
		}
		m, err := manifest.Decode(rc)
		rc.Close()
		if err != nil {
			return err
		}
		status := "NO"
		if m.Verification.Verified {
			status = "yes"
		}
		note := m.Verification.Error
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%d\t%s\n",
			m.ID, m.SourceLabel, m.PlaintextSize, status, m.Verification.TableCount, note)
	}
	return w.Flush()
}

func runRestore(ctx context.Context, cfg *config.Config, store storage.Backend, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	to := fs.String("to", "", "target database DSN to restore into (must exist and be empty)")
	toFile := fs.String("to-file", "", "write the decrypted archive here instead of restoring")
	force := fs.Bool("force", false, "allow a non-empty target, the source database, or overwriting -to-file")
	allowUnverified := fs.Bool("allow-unverified", false, "restore a backup that has not passed verification")

	// accept id on either side of flags: flag.Parse stops at the first non-flag arg.
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	}
	if id == "" || fs.NArg() > 1 {
		return fmt.Errorf("usage: backwyn restore <backup-id> (-to <dsn> | -to-file <path>) [-force] [-allow-unverified]")
	}

	res, err := restore.Run(ctx, cfg, store, id, restore.Options{
		TargetDSN:       *to,
		ToFile:          *toFile,
		Force:           *force,
		AllowUnverified: *allowUnverified,
	})
	if err != nil {
		return err
	}

	if res.Path != "" {
		fmt.Printf("wrote %s (%d bytes, pg_dump custom format) in %s\n",
			res.Path, res.Manifest.PlaintextSize, res.Duration.Round(time.Millisecond))
		fmt.Printf("restore it with: pg_restore --no-owner --dbname <dsn> %s\n", res.Path)
		return nil
	}

	fmt.Printf("RESTORED %s into %s: %d tables in %s\n",
		res.Manifest.ID, res.TargetLabel, res.TableCount, res.Duration.Round(time.Millisecond))
	if res.Manifest.Verification.TableCount > 0 && res.TableCount != res.Manifest.Verification.TableCount {
		fmt.Fprintf(os.Stderr, "WARNING: expected %d tables from the verification record, found %d\n",
			res.Manifest.Verification.TableCount, res.TableCount)
	}
	return nil
}

func runPrune(ctx context.Context, store storage.Backend, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	pol := retentionFlags(fs)
	dryRun := fs.Bool("dry-run", false, "show the plan without deleting anything")
	sweep := fs.Bool("sweep-orphans", false, "also delete artifacts older than 24h that have no manifest")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ms, err := artifact.LoadAll(ctx, store)
	if err != nil {
		return err
	}
	plan := retention.Compute(ms, *pol, time.Now())

	// always show decisions so the operator can audit the plan.
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ACTION\tID\tCREATED\tVERIFIED\tREASON")
	for _, d := range plan.Decisions {
		action := "DELETE"
		if d.Keep {
			action = "keep"
		}
		verified := "no"
		if d.Manifest.Verification.Verified {
			verified = "yes"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", action, d.Manifest.ID,
			d.Manifest.CreatedAt.Format(time.RFC3339), verified, d.Reason)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, warn := range plan.Warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", warn)
	}

	if *dryRun {
		fmt.Printf("\ndry run: would delete %d backup(s), freeing %d bytes\n",
			len(plan.Remove()), plan.Bytes())
		return nil
	}

	freed, err := retention.Apply(ctx, store, plan)
	if err != nil {
		return err
	}
	fmt.Printf("\npruned %d backup(s), freed %d bytes\n", len(plan.Remove()), freed)

	if *sweep {
		n, sfreed, err := retention.SweepOrphans(ctx, store, 24*time.Hour, time.Now())
		if err != nil {
			return err
		}
		fmt.Printf("swept %d orphaned artifact(s), freed %d bytes\n", n, sfreed)
	}
	return nil
}

// retentionFlags adds retention flags to fs.
func retentionFlags(fs *flag.FlagSet) *retention.Policy {
	var p retention.Policy
	fs.IntVar(&p.KeepLast, "keep-last", 0, "keep the N most recent verified backups")
	fs.IntVar(&p.KeepDaily, "keep-daily", 0, "keep the newest verified backup from each of the last N days")
	fs.IntVar(&p.KeepWeekly, "keep-weekly", 0, "keep the newest verified backup from each of the last N weeks")
	fs.IntVar(&p.KeepMonthly, "keep-monthly", 0, "keep the newest verified backup from each of the last N months")
	return &p
}

func runDaemon(cfg *config.Config, store storage.Backend, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	interval := fs.Duration("interval", 6*time.Hour, "time between backup cycles")
	maxAge := fs.Duration("max-age", 24*time.Hour, "alert if no verified backup is newer than this")
	once := fs.Bool("once", false, "run a single cycle and exit (for external cron)")
	defaultListen := os.Getenv("BACKWYN_LISTEN_ADDR")
	if defaultListen == "" {
		defaultListen = ":8080"
	}
	listen := fs.String("listen", defaultListen, "address to listen on for health checks and metrics")
	pol := retentionFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	tracker := runner.NewTracker()
	if *listen != "" && !*once {
		runner.StartServer(*listen, tracker)
	}

	deps := runner.Deps{
		Cfg:       cfg,
		Store:     store,
		Alerter:   alert.New(cfg.AlertWebhook),
		MaxAge:    *maxAge,
		Now:       time.Now,
		Retention: *pol,
		Tracker:   tracker,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *once {
		return runner.Cycle(ctx, deps)
	}

	slog.Info("backwyn daemon started", "interval", *interval, "max_age", *maxAge)

	if err := runner.Cycle(ctx, deps); err != nil {
		slog.Error("cycle error", "err", err)
	}

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "reason", ctx.Err())
			return nil
		case <-ticker.C:
			if err := runner.Cycle(ctx, deps); err != nil {
				slog.Error("cycle error", "err", err)
			}
		}
	}
}

func newBackend(cfg *config.Config) (storage.Backend, error) {
	switch cfg.Backend {
	case config.BackendS3:
		return storage.NewS3(storage.S3Options{
			Bucket:    cfg.S3.Bucket,
			Endpoint:  cfg.S3.Endpoint,
			Region:    cfg.S3.Region,
			AccessKey: cfg.S3.AccessKey,
			SecretKey: cfg.S3.SecretKey,
			PathStyle: cfg.S3.PathStyle,
		})
	default:
		return storage.NewLocal(cfg.StorageDir)
	}
}

func runCheck(ctx context.Context, store storage.Backend, args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	maxAge := fs.Duration("max-age", 24*time.Hour,
		"alert if there is no verified backup newer than this (e.g. 24h, 90m)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rep, err := check.Run(ctx, store, *maxAge, time.Now())
	if err != nil {
		return err
	}

	if rep.LastVerified != nil {
		fmt.Printf("last verified backup: %s (%s old, %d tables)\n",
			rep.LastVerified.ID, rep.LastVerifiedAge.Round(time.Second), rep.LastVerified.Verification.TableCount)
	} else {
		fmt.Println("last verified backup: NONE")
	}

	for _, w := range rep.Warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", w)
	}

	if rep.Healthy {
		fmt.Printf("OK: a verified backup exists within %s\n", *maxAge)
		return nil
	}

	fmt.Fprintf(os.Stderr, "ALERT: backup coverage is not healthy (threshold %s)\n", *maxAge)
	for _, r := range rep.Reasons {
		fmt.Fprintf(os.Stderr, "  - %s\n", r)
	}
	os.Exit(3)
	return nil
}

func usage() {
	fmt.Fprintln(os.Stderr, `backwyn — verified Postgres backups

usage:
  backwyn backup           dump, encrypt, store, write manifest
  backwyn verify <id>      prove a stored backup restores cleanly
  backwyn restore <id> (-to <dsn> | -to-file <path>) [-force] [-allow-unverified]
                            bring a backup back: restore into a database, or
                            write a plain pg_dump archive you can restore
                            yourself. Refuses a non-empty target, the source
                            database, and unverified backups unless told.
  backwyn list             show stored backups and verification status
  backwyn check [-max-age 24h]
                            alert (exit 3) if no verified backup is fresh enough
  backwyn prune [-keep-last N] [-keep-daily N] [-keep-weekly N]
                 [-keep-monthly N] [-dry-run] [-sweep-orphans]
                            delete backups outside the retention policy. Never
                            deletes the most recent verified backup, and prunes
                            nothing at all if no backup is verified. With no
                            -keep flags it keeps everything.
  backwyn run [-interval 6h] [-max-age 24h] [-once] [-listen :8080] [-keep-* N]
                            daemon: backup -> verify -> check -> prune on a
                            schedule (-once runs a single cycle, for cron).
                            Serves /healthz and /metrics on -listen ("" to
                            disable; skipped with -once).

environment:
  BACKWYN_SOURCE_DSN        source database connection string
  BACKWYN_ENCRYPTION_KEY    base64-encoded 32-byte key
  BACKWYN_VERIFY_ADMIN_DSN  local Postgres admin DSN (verify sandbox)
  BACKWYN_STORAGE           backend: "local" (default) or "s3"
  BACKWYN_STORAGE_DIR       local backend root (when BACKWYN_STORAGE=local)
  BACKWYN_S3_BUCKET         bucket name (when BACKWYN_STORAGE=s3)
  BACKWYN_S3_ENDPOINT       S3 endpoint, e.g. R2 https://<acct>.r2.cloudflarestorage.com
  BACKWYN_S3_REGION         region ("auto" for R2)
  BACKWYN_S3_ACCESS_KEY     S3 access key id
  BACKWYN_S3_SECRET_KEY     S3 secret access key
  BACKWYN_S3_PATH_STYLE     "true" for path-style addressing (R2, MinIO)
  BACKWYN_ALERT_WEBHOOK     optional URL to POST JSON alerts to
  BACKWYN_VERIFY_QUERY      optional SQL run against the restored sandbox; if
                            it errors, verification fails
  BACKWYN_DUMP_SCHEMAS      optional comma-separated schemas to dump (pg_dump
                            -n); empty dumps the whole database
  BACKWYN_LISTEN_ADDR       default for run -listen (health checks + metrics)`)
}
