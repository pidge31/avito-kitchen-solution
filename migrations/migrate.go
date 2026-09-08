// Пакет migrations содержит версионируемые транзакционные миграции БД.
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
)

//go:embed *.sql
var files embed.FS

const LatestVersion = 2

var versions = []string{"001_init", "002_order_indexes"}

// Run применяет все ожидающие версии либо откатывает все версии. Перед down нужно остановить API.
func Run(ctx context.Context, db *sql.DB, direction string) error {
	if direction != "up" && direction != "down" {
		return fmt.Errorf("expected up or down")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(718293640)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var highest int
	if err = tx.QueryRowContext(ctx, `SELECT coalesce(max(version),0) FROM schema_migrations`).Scan(&highest); err != nil {
		return err
	}
	if highest > LatestVersion {
		return fmt.Errorf("database has a newer migration version: %d", highest)
	}
	for step := range versions {
		i := step
		if direction == "down" {
			i = len(versions) - 1 - step
		}
		name, version := versions[i], i+1
		up, readErr := files.ReadFile(name + ".up.sql")
		if readErr != nil {
			return readErr
		}
		expected := fmt.Sprintf("%x", sha256.Sum256(up))
		var checksum string
		err = tx.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, version).Scan(&checksum)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		applied := err == nil
		if applied && checksum != expected {
			return fmt.Errorf("migration %d checksum differs: restore the applied migration", version)
		}
		if (direction == "up" && applied) || (direction == "down" && !applied) {
			continue
		}
		body, readErr := files.ReadFile(name + "." + direction + ".sql")
		if readErr != nil {
			return readErr
		}
		if _, err = tx.ExecContext(ctx, string(body)); err != nil {
			return err
		}
		if direction == "up" {
			_, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,checksum) VALUES($1,$2)`, version, expected)
		} else {
			_, err = tx.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=$1`, version)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
