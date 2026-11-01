// Package db 实现 AutoBid Engine 的 PostgreSQL 双图存储层：
// 全局静态实体图（assets + entity_edges）与单任务推演图（exploration_nodes/edges/anchors）。
package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

// migrateLockKey 串行化多实例同时迁移（"AUTOBID" 的 ASCII）。
const migrateLockKey int64 = 0x4155544F424944

var (
	ErrNotFound    = errors.New("db: not found")
	ErrInvalidEdge = errors.New("db: invalid edge")
	ErrLeaseLost   = errors.New("db: intent lease lost")
	ErrForeignNode = errors.New("db: node not readable from this exploration")
)

type DB struct {
	*sql.DB
	tx *sql.Tx // callback-scoped only; never shared with other requests
}

func Open(ctx context.Context, dsn string) (*DB, error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(32)
	sqlDB.SetMaxIdleConns(8)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return &DB{DB: sqlDB}, nil
}

// Migrate 在一个事务内执行嵌入的 schema.sql，可在每次启动时调用。
// 库内已记录相同 schema 校验和时直接返回：schema.sql 中的 DROP/CREATE TRIGGER 会取 AccessExclusive 锁，
// 若每次启动都重放，会与其他实例的在途写入死锁。
func (d *DB) Migrate(ctx context.Context) error {
	sum := sha256.Sum256([]byte(schemaSQL))
	checksum := hex.EncodeToString(sum[:])
	return d.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrateLockKey); err != nil {
			return fmt.Errorf("migrate lock: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (
			id         INT PRIMARY KEY CHECK (id = 1),
			checksum   TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
			return fmt.Errorf("migrate version table: %w", err)
		}
		var current string
		err := tx.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE id = 1`).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("migrate read version: %w", err)
		}
		if current == checksum {
			return nil
		}
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("migrate schema: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO schema_version(id, checksum) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET checksum = EXCLUDED.checksum, applied_at = now()`, checksum)
		return err
	})
}

// InTx 在事务中执行 fn；fn 返回错误或 panic 时回滚。
func (d *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if d.tx != nil {
		return fn(d.tx)
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Transaction composes existing stores in one commit boundary. The callback must
// return every error and must not retain or concurrently use the scoped handle.
// Nested InTx calls join this transaction; they do not commit independently.
func (d *DB) Transaction(ctx context.Context, fn func(*DB) error) error {
	return d.InTx(ctx, func(tx *sql.Tx) error { return fn(&DB{DB: d.DB, tx: tx}) })
}

func (d *DB) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if d.tx != nil {
		return d.tx.ExecContext(ctx, q, args...)
	}
	return d.DB.ExecContext(ctx, q, args...)
}

func (d *DB) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if d.tx != nil {
		return d.tx.QueryContext(ctx, q, args...)
	}
	return d.DB.QueryContext(ctx, q, args...)
}

func (d *DB) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	if d.tx != nil {
		return d.tx.QueryRowContext(ctx, q, args...)
	}
	return d.DB.QueryRowContext(ctx, q, args...)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
