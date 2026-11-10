package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

func SchemaChecksum() string {
	sum := sha256.Sum256([]byte(schemaSQL))
	return hex.EncodeToString(sum[:])
}

// AppliedSchemaChecksum is deliberately read-only; an operations check must not migrate.
func (d *DB) AppliedSchemaChecksum(ctx context.Context) (string, error) {
	var exists bool
	if err := d.QueryRowContext(ctx, `SELECT to_regclass('schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", nil
	}
	var checksum string
	err := d.QueryRowContext(ctx, `SELECT checksum FROM schema_version WHERE id=1`).Scan(&checksum)
	return checksum, err
}
