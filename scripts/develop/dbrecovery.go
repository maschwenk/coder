//go:build !windows

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/lib/pq"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3"
)

const trackingDDL = `
CREATE SCHEMA IF NOT EXISTS _develop;
CREATE TABLE IF NOT EXISTS _develop.applied_migrations (
    version BIGINT PRIMARY KEY,
    filename TEXT NOT NULL,
    down_sql TEXT NOT NULL
);
`

// recoverDB checks for migration conflicts before the server
// starts. A JSON cache file maps {version: filename} for every
// migration that was applied. If a cached file is gone from disk
// (branch switch), the developer must choose --db-rollback or
// --db-reset.
//
// New migrations on disk that aren't in the cache are normal
// (forward migration) and ignored — the server handles those.
func recoverDB(ctx context.Context, logger slog.Logger, cfg *devConfig) error {
	pgURL := os.Getenv("CODER_PG_CONNECTION_URL")
	isBuiltinPG := pgURL == ""

	migrDir := filepath.Join(cfg.projectRoot, "coderd", "database", "migrations")
	cachePath := migrationCachePath(cfg, isBuiltinPG)

	if isBuiltinPG {
		pgDir := filepath.Join(cfg.configDir, "postgres")
		if _, err := os.Stat(filepath.Join(pgDir, "data")); err != nil {
			return nil // Fresh install.
		}
		if cfg.dbReset {
			logger.Warn(ctx, "wiping built-in database (--db-reset)")
			if err := os.RemoveAll(pgDir); err != nil {
				return xerrors.Errorf("remove postgres directory: %w", err)
			}
			return nil
		}
	}

	conflict := cacheConflict(cachePath, migrDir)
	if conflict == "" {
		return nil
	}

	if !cfg.dbRollback && !cfg.dbReset {
		return xerrors.Errorf(
			"database migration conflict: %s\n\n"+
				"  --db-rollback   roll back mismatched migrations (preserves data)\n"+
				"  --db-reset      destroy database and start fresh\n",
			conflict)
	}

	// From here we need a DB connection.
	if isBuiltinPG {
		stopPG, err := startTempPostgresSetURL(ctx, logger, cfg, &pgURL)
		if err != nil {
			return xerrors.Errorf(
				"cannot start temporary postgres: %w\n\ntry --db-reset instead", err)
		}
		defer stopPG()
	}

	db, err := connectDB(ctx, pgURL)
	if err != nil {
		return xerrors.Errorf("connect: %w", err)
	}
	defer db.Close()

	if cfg.dbReset {
		if !isBuiltinPG {
			_, _ = fmt.Fprintf(os.Stderr,
				"\n  WARNING: this will DROP all schemas in the external database.\n"+
					"  Set CODER_DEV_DB_RESET=1 to confirm.\n\n")
			if os.Getenv("CODER_DEV_DB_RESET") != "1" {
				return xerrors.New("refusing to reset external database without CODER_DEV_DB_RESET=1")
			}
		}
		logger.Warn(ctx, "resetting database (--db-reset)")
		return resetSchema(ctx, db)
	}

	return rollbackMigrations(ctx, logger, db, migrDir, cachePath)
}

// rollbackMigrations uses the DB tracking table to undo
// migrations that no longer exist on disk.
func rollbackMigrations(ctx context.Context, logger slog.Logger, db *sql.DB, migrDir, cachePath string) error {
	if _, err := db.ExecContext(ctx, trackingDDL); err != nil {
		return xerrors.Errorf("create tracking table: %w", err)
	}

	dbVersion, dirty, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return xerrors.Errorf("get db version: %w", err)
	}
	if dbVersion < 0 {
		return nil
	}
	if dirty {
		return xerrors.Errorf(
			"database is dirty at version %d (a migration failed halfway)\n\n"+
				"  --db-reset  destroy database and start fresh\n", dbVersion)
	}

	// Check for migrations the server applied that we never
	// tracked. We have no down SQL for these.
	maxTracked, err := maxTrackedVersion(ctx, db)
	if err != nil {
		return xerrors.Errorf("get max tracked version: %w", err)
	}
	if dbVersion > maxTracked && maxTracked >= 0 {
		return xerrors.Errorf(
			"database is at version %d but tracking table only covers up to %d\n"+
				"(migrations %d–%d were applied outside develop.sh)\n\n"+
				"  --db-reset  destroy database and start fresh\n",
			dbVersion, maxTracked, maxTracked+1, dbVersion)
	}

	// Find tracked entries whose file is gone from disk.
	rollbacks, err := findRollbacks(ctx, db, migrDir)
	if err != nil {
		return xerrors.Errorf("find rollbacks: %w", err)
	}
	if len(rollbacks) == 0 {
		// No rollbacks needed. Capture any new migrations and
		// update cache (first-adoption case lands here).
		if err := captureDownSQL(ctx, db, migrDir, dbVersion); err != nil {
			return xerrors.Errorf("capture down SQL: %w", err)
		}
		return writeCache(cachePath, migrDir, dbVersion)
	}

	if !contiguousFromTop(rollbacks, dbVersion) {
		return xerrors.Errorf(
			"cannot roll back: versions are not contiguous (%s); use --db-reset",
			formatVersions(rollbacks))
	}

	logger.Warn(ctx, "rolling back mismatched migrations",
		slog.F("db_version", dbVersion),
		slog.F("count", len(rollbacks)))

	for _, rb := range rollbacks {
		if err := applyRollback(ctx, db, rb); err != nil {
			return xerrors.Errorf(
				"rollback of version %d (%s) failed: %w\n\nuse --db-reset to start fresh",
				rb.version, rb.filename, err)
		}
		logger.Info(ctx, "rolled back migration",
			slog.F("version", rb.version),
			slog.F("filename", rb.filename))
	}

	// Capture current state after rollback.
	dbVersion, _, err = currentMigrationVersion(ctx, db)
	if err != nil {
		return xerrors.Errorf("get db version after rollback: %w", err)
	}
	if err := captureDownSQL(ctx, db, migrDir, dbVersion); err != nil {
		return xerrors.Errorf("capture down SQL: %w", err)
	}
	return writeCache(cachePath, migrDir, dbVersion)
}

// cacheConflict checks if any cached migration file is missing
// from disk. Returns a description of the first conflict found,
// or "" if everything is fine. New migrations on disk that aren't
// in the cache are ignored (forward migration).
func cacheConflict(cachePath, migrDir string) string {
	cache, err := readCache(cachePath)
	if err != nil {
		return "" // No cache = no conflicts to detect.
	}
	for vStr, filename := range cache {
		if _, err := os.Stat(filepath.Join(migrDir, filename)); err != nil {
			return fmt.Sprintf(
				"version %s (%s) was applied but no longer exists on disk",
				vStr, filename)
		}
	}
	return ""
}

type rollbackEntry struct {
	version  int
	filename string
	downSQL  string
}

// findRollbacks returns tracked migrations whose file no longer
// exists on disk, sorted in descending version order.
func findRollbacks(ctx context.Context, db *sql.DB, migrDir string) ([]rollbackEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT version, filename, down_sql
		FROM _develop.applied_migrations
		ORDER BY version DESC
	`)
	if err != nil {
		return nil, xerrors.Errorf("query tracking table: %w", err)
	}
	defer rows.Close()

	var rollbacks []rollbackEntry
	for rows.Next() {
		var rb rollbackEntry
		if err := rows.Scan(&rb.version, &rb.filename, &rb.downSQL); err != nil {
			return nil, xerrors.Errorf("scan row: %w", err)
		}
		if _, err := os.Stat(filepath.Join(migrDir, rb.filename)); err != nil {
			rollbacks = append(rollbacks, rb)
		}
	}
	return rollbacks, rows.Err()
}

func maxTrackedVersion(ctx context.Context, db *sql.DB) (int, error) {
	var v sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT MAX(version) FROM _develop.applied_migrations`,
	).Scan(&v)
	if err != nil {
		var pgErr *pq.Error
		if xerrors.As(err, &pgErr) && pgErr.Code.Name() == "undefined_table" {
			return -1, nil
		}
		return -1, xerrors.Errorf("query max tracked version: %w", err)
	}
	if !v.Valid {
		return -1, nil // Empty table.
	}
	return int(v.Int64), nil
}

func contiguousFromTop(rollbacks []rollbackEntry, dbVersion int) bool {
	expected := dbVersion
	for _, rb := range rollbacks {
		if rb.version != expected {
			return false
		}
		expected--
	}
	return true
}

// applyRollback executes a single down migration and updates both
// schema_migrations and the tracking table in one transaction.
func applyRollback(ctx context.Context, db *sql.DB, rb rollbackEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return xerrors.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, rb.downSQL); err != nil {
		return xerrors.Errorf("execute down SQL: %w", err)
	}

	targetVersion := rb.version - 1
	if _, err := tx.ExecContext(ctx, `TRUNCATE schema_migrations`); err != nil {
		return xerrors.Errorf("truncate schema_migrations: %w", err)
	}
	if targetVersion >= 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, dirty) VALUES ($1, $2)`,
			targetVersion, false); err != nil {
			return xerrors.Errorf("set version: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM _develop.applied_migrations WHERE version = $1`,
		rb.version); err != nil {
		return xerrors.Errorf("remove tracking entry: %w", err)
	}

	return tx.Commit()
}

// captureDownSQL scans .down.sql files on disk and stores their
// content in the tracking table for versions <= dbVersion.
func captureDownSQL(ctx context.Context, db *sql.DB, migrDir string, dbVersion int) error {
	entries, err := os.ReadDir(migrDir)
	if err != nil {
		return xerrors.Errorf("read migrations dir: %w", err)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".down.sql") || len(name) < 7 {
			continue
		}
		version, err := strconv.Atoi(name[:6])
		if err != nil || version > dbVersion {
			continue
		}
		content, err := os.ReadFile(filepath.Join(migrDir, name))
		if err != nil {
			return xerrors.Errorf("read %s: %w", name, err)
		}
		_, err = db.ExecContext(ctx, `
			INSERT INTO _develop.applied_migrations (version, filename, down_sql)
			VALUES ($1, $2, $3)
			ON CONFLICT (version) DO UPDATE
			SET filename = EXCLUDED.filename, down_sql = EXCLUDED.down_sql
		`, version, name, string(content))
		if err != nil {
			return xerrors.Errorf("upsert version %d: %w", version, err)
		}
	}
	return nil
}

func readCache(cachePath string) (map[string]string, error) {
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}
	var cache map[string]string
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}
	return cache, nil
}

func writeCache(cachePath, migrDir string, dbVersion int) error {
	entries, err := os.ReadDir(migrDir)
	if err != nil {
		return xerrors.Errorf("read migrations dir: %w", err)
	}
	cache := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".down.sql") || len(name) < 7 {
			continue
		}
		version, err := strconv.Atoi(name[:6])
		if err != nil || version > dbVersion {
			continue
		}
		cache[strconv.Itoa(version)] = name
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return xerrors.Errorf("marshal cache: %w", err)
	}
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		return xerrors.Errorf("write cache file: %w", err)
	}
	return nil
}

// resetSchema drops public and _develop schemas.
func resetSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return xerrors.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS _develop CASCADE`,
		`DROP SCHEMA IF EXISTS public CASCADE`,
		`CREATE SCHEMA IF NOT EXISTS public`,
		`GRANT ALL ON SCHEMA public TO public`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return xerrors.Errorf("exec %q: %w", stmt, err)
		}
	}
	return tx.Commit()
}

func migrationCachePath(cfg *devConfig, builtinPG bool) string { //nolint:revive // selector, not control flow.
	if builtinPG {
		return filepath.Join(cfg.configDir, "postgres", "migration_cache.json")
	}
	return filepath.Join(cfg.configDir, "migration_cache.json")
}

// updateMigrationTracking connects to the running server's
// database and captures down SQL for any new migrations applied
// since the last cache write.
func updateMigrationTracking(ctx context.Context, _ slog.Logger, cfg *devConfig) error {
	pgURL := os.Getenv("CODER_PG_CONNECTION_URL")
	isBuiltinPG := pgURL == ""
	if isBuiltinPG {
		var err error
		pgURL, err = builtinPostgresURL(cfg)
		if err != nil {
			return xerrors.Errorf("resolve builtin postgres URL: %w", err)
		}
	}

	db, err := connectDB(ctx, pgURL)
	if err != nil {
		return xerrors.Errorf("connect for tracking update: %w", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, trackingDDL); err != nil {
		return xerrors.Errorf("ensure tracking table: %w", err)
	}

	dbVersion, _, err := currentMigrationVersion(ctx, db)
	if err != nil {
		return xerrors.Errorf("get db version: %w", err)
	}
	if dbVersion < 0 {
		return nil
	}

	migrDir := filepath.Join(cfg.projectRoot, "coderd", "database", "migrations")
	if err := captureDownSQL(ctx, db, migrDir, dbVersion); err != nil {
		return xerrors.Errorf("capture down SQL: %w", err)
	}

	cachePath := migrationCachePath(cfg, isBuiltinPG)
	return writeCache(cachePath, migrDir, dbVersion)
}

func builtinPostgresURL(cfg *devConfig) (string, error) {
	pgDir := filepath.Join(cfg.configDir, "postgres")

	portBytes, err := os.ReadFile(filepath.Join(pgDir, "port"))
	if err != nil {
		return "", xerrors.Errorf("read postgres port: %w", err)
	}
	port := strings.TrimSpace(string(portBytes))

	passwordBytes, err := os.ReadFile(filepath.Join(pgDir, "password"))
	if err != nil {
		return "", xerrors.Errorf("read postgres password: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))

	return fmt.Sprintf(
		"postgres://coder@localhost:%s/coder?sslmode=disable&password=%s",
		port, url.QueryEscape(password)), nil
}

func connectDB(ctx context.Context, pgURL string) (*sql.DB, error) {
	db, err := sql.Open("postgres", pgURL)
	if err != nil {
		return nil, xerrors.Errorf("open: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, xerrors.Errorf("ping: %w", err)
	}
	return db, nil
}

// startTempPostgresSetURL starts an embedded postgres on an
// ephemeral port and writes the URL into *pgURL. Returns a
// cleanup function that stops postgres.
func startTempPostgresSetURL(ctx context.Context, logger slog.Logger, cfg *devConfig, pgURL *string) (func(), error) {
	pgDir := filepath.Join(cfg.configDir, "postgres")
	cleanStalePIDFile(filepath.Join(pgDir, "data"))

	passwordBytes, err := os.ReadFile(filepath.Join(pgDir, "password"))
	if err != nil {
		return nil, xerrors.Errorf("read postgres password: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, xerrors.Errorf("find ephemeral port: %w", err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, xerrors.New("listener returned non-TCP addr")
	}
	port := tcpAddr.Port
	_ = listener.Close()

	ep := embeddedpostgres.NewDatabase(
		embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V13).
			BinariesPath(filepath.Join(pgDir, "bin")).
			CachePath(filepath.Join(pgDir, "cache")).
			DataPath(filepath.Join(pgDir, "data")).
			RuntimePath(filepath.Join(pgDir, "runtime")).
			Port(uint32(port)). //nolint:gosec // port from listener, fits uint32.
			Username("coder").
			Password(password).
			Database("coder").
			Logger(nil),
	)

	logger.Info(ctx, "starting temporary postgres for migration recovery",
		slog.F("port", port))
	if err := ep.Start(); err != nil {
		return nil, xerrors.Errorf("start embedded postgres: %w", err)
	}

	*pgURL = fmt.Sprintf(
		"postgres://coder@localhost:%d/coder?sslmode=disable&password=%s",
		port, url.QueryEscape(password))

	return func() {
		if err := ep.Stop(); err != nil {
			logger.Warn(ctx, "failed to stop temporary postgres",
				slog.Error(err))
		}
	}, nil
}

func cleanStalePIDFile(dataDir string) {
	pidPath := filepath.Join(dataDir, "postmaster.pid")
	content, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	lines := strings.SplitN(string(content), "\n", 2)
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil {
		_ = os.Remove(pidPath)
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = os.Remove(pidPath)
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		_ = os.Remove(pidPath)
	}
}

func currentMigrationVersion(ctx context.Context, db *sql.DB) (int, bool, error) {
	var version int
	var dirty bool
	err := db.QueryRowContext(ctx,
		`SELECT version, dirty FROM schema_migrations LIMIT 1`,
	).Scan(&version, &dirty)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return -1, false, nil
		}
		var pgErr *pq.Error
		if xerrors.As(err, &pgErr) && pgErr.Code.Name() == "undefined_table" {
			return -1, false, nil
		}
		return -1, false, xerrors.Errorf("query schema_migrations: %w", err)
	}
	return version, dirty, nil
}

func formatVersions(rollbacks []rollbackEntry) string {
	var parts []string
	for _, rb := range rollbacks {
		parts = append(parts, strconv.Itoa(rb.version))
	}
	return strings.Join(parts, ", ")
}
