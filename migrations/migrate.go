package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const migrationLockID int64 = 517_611_908_2026

var (
	//go:embed *.up.sql *.down.sql
	migrationFiles embed.FS

	migrationName = regexp.MustCompile(`^(\d{6})_([a-z0-9_]+)\.(up|down)\.sql$`)
)

type Migration struct {
	Version      int64
	Name         string
	UpSQL        string
	DownSQL      string
	UpChecksum   string
	DownChecksum string
}

type Status struct {
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

func List() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFiles, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	type pair struct {
		version int64
		name    string
		up      string
		down    string
	}
	pairs := make(map[int64]*pair)

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationName.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", matches[1], err)
		}
		contents, err := migrationFiles.ReadFile(entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		current, ok := pairs[version]
		if !ok {
			current = &pair{version: version, name: matches[2]}
			pairs[version] = current
		} else if current.name != matches[2] {
			return nil, fmt.Errorf("migration version %06d has conflicting names", version)
		}

		switch matches[3] {
		case "up":
			if current.up != "" {
				return nil, fmt.Errorf("migration version %06d has duplicate up files", version)
			}
			current.up = string(contents)
		case "down":
			if current.down != "" {
				return nil, fmt.Errorf("migration version %06d has duplicate down files", version)
			}
			current.down = string(contents)
		}
	}

	migrations := make([]Migration, 0, len(pairs))
	for _, current := range pairs {
		if strings.TrimSpace(current.up) == "" || strings.TrimSpace(current.down) == "" {
			return nil, fmt.Errorf("migration %06d_%s requires both up and down files", current.version, current.name)
		}
		migrations = append(migrations, Migration{
			Version:      current.version,
			Name:         current.name,
			UpSQL:        current.up,
			DownSQL:      current.down,
			UpChecksum:   checksum(current.up),
			DownChecksum: checksum(current.down),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})
	return migrations, nil
}

func Apply(ctx context.Context, db *sql.DB) error {
	migrations, err := List()
	if err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockAndBootstrap(ctx, tx); err != nil {
		return err
	}
	applied, err := readApplied(ctx, tx)
	if err != nil {
		return err
	}

	for _, migration := range migrations {
		if current, ok := applied[migration.Version]; ok {
			if current.Name != migration.Name || current.Checksum != migration.UpChecksum {
				return fmt.Errorf("migration %06d checksum/name drift detected", migration.Version)
			}
			continue
		}

		if _, err := tx.ExecContext(ctx, migration.UpSQL); err != nil {
			return fmt.Errorf("apply migration %06d_%s: %w", migration.Version, migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO loyalty.schema_migrations (version, name, checksum)
            VALUES ($1, $2, $3)`, migration.Version, migration.Name, migration.UpChecksum); err != nil {
			return fmt.Errorf("record migration %06d: %w", migration.Version, err)
		}
		if err := recordEvent(ctx, tx, migration, "UP", migration.UpChecksum); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func RollbackLast(ctx context.Context, db *sql.DB) error {
	migrations, err := List()
	if err != nil {
		return err
	}
	byVersion := make(map[int64]Migration, len(migrations))
	for _, migration := range migrations {
		byVersion[migration.Version] = migration
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin rollback transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockAndBootstrap(ctx, tx); err != nil {
		return err
	}

	var current Status
	err = tx.QueryRowContext(ctx, `
        SELECT version, name, checksum, applied_at
        FROM loyalty.schema_migrations
        ORDER BY version DESC
        LIMIT 1`).Scan(&current.Version, &current.Name, &current.Checksum, &current.AppliedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read latest migration: %w", err)
	}

	migration, ok := byVersion[current.Version]
	if !ok || migration.Name != current.Name || migration.UpChecksum != current.Checksum {
		return fmt.Errorf("cannot rollback migration %06d: embedded definition drift", current.Version)
	}
	if _, err := tx.ExecContext(ctx, migration.DownSQL); err != nil {
		return fmt.Errorf("rollback migration %06d_%s: %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM loyalty.schema_migrations WHERE version = $1`, migration.Version); err != nil {
		return fmt.Errorf("remove migration record %06d: %w", migration.Version, err)
	}
	if err := recordEvent(ctx, tx, migration, "DOWN", migration.DownChecksum); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollback: %w", err)
	}
	return nil
}

func Current(ctx context.Context, db *sql.DB) ([]Status, error) {
	var migrationTable sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('loyalty.schema_migrations')`).Scan(&migrationTable); err != nil {
		return nil, fmt.Errorf("locate migration ledger: %w", err)
	}
	if !migrationTable.Valid {
		return nil, nil
	}

	rows, err := db.QueryContext(ctx, `
        SELECT version, name, checksum, applied_at
        FROM loyalty.schema_migrations
        ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("query migration status: %w", err)
	}
	defer rows.Close()

	var statuses []Status
	for rows.Next() {
		var status Status
		if err := rows.Scan(&status.Version, &status.Name, &status.Checksum, &status.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan migration status: %w", err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate migration status: %w", err)
	}
	return statuses, nil
}

// ValidateCurrent verifies that the database has exactly the embedded migration
// history. Service startup uses this read-only check and never applies schema
// changes implicitly.
func ValidateCurrent(statuses []Status) error {
	migrations, err := List()
	if err != nil {
		return err
	}
	if len(statuses) != len(migrations) {
		return fmt.Errorf("database has %d loyalty migrations; service requires %d", len(statuses), len(migrations))
	}
	for index, migration := range migrations {
		status := statuses[index]
		if status.Version != migration.Version || status.Name != migration.Name || status.Checksum != migration.UpChecksum {
			return fmt.Errorf("database migration %06d does not match the embedded definition", migration.Version)
		}
	}
	return nil
}

const schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS loyalty.schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL,
    checksum char(64) NOT NULL,
    applied_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS loyalty.schema_migration_events (
    event_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    version bigint NOT NULL,
    name text NOT NULL,
    direction text NOT NULL CHECK (direction IN ('UP', 'DOWN')),
    checksum char(64) NOT NULL,
    occurred_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

func lockAndBootstrap(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS loyalty`); err != nil {
		return fmt.Errorf("ensure loyalty schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("ensure migration ledger: %w", err)
	}
	return nil
}

func readApplied(ctx context.Context, tx *sql.Tx) (map[int64]Status, error) {
	rows, err := tx.QueryContext(ctx, `
        SELECT version, name, checksum, applied_at
        FROM loyalty.schema_migrations
        ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int64]Status)
	for rows.Next() {
		var status Status
		if err := rows.Scan(&status.Version, &status.Name, &status.Checksum, &status.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[status.Version] = status
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate applied migrations: %w", err)
	}
	return applied, nil
}

func recordEvent(ctx context.Context, tx *sql.Tx, migration Migration, direction, sum string) error {
	if _, err := tx.ExecContext(ctx, `
        INSERT INTO loyalty.schema_migration_events (version, name, direction, checksum)
        VALUES ($1, $2, $3, $4)`, migration.Version, migration.Name, direction, sum); err != nil {
		return fmt.Errorf("record %s event for migration %06d: %w", direction, migration.Version, err)
	}
	return nil
}

func checksum(contents string) string {
	sum := sha256.Sum256([]byte(contents))
	return hex.EncodeToString(sum[:])
}
