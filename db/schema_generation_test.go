package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// generationSchemaHash is the SHA-256 of db/schema.sql (LF line endings) at SchemaGeneration.
// If this test fails you changed the schema: bump SchemaGeneration and update this hash together,
// otherwise an older program could replay its DDL over the newer database.
const generationSchemaHash = "07f219bb2badd0a9ae98e0ccb6674259b489e675310474ac21a75f5ebff242c4"

func TestSchemaGenerationBumped(t *testing.T) {
	sum := sha256.Sum256([]byte(strings.ReplaceAll(schemaSQL, "\r\n", "\n")))
	if got := hex.EncodeToString(sum[:]); got != generationSchemaHash {
		t.Fatalf("db/schema.sql changed (hash %s): bump SchemaGeneration and update generationSchemaHash", got)
	}
}

// migratedTempDB is a private database: these tests rewrite schema_version, which would break any
// other package migrating the shared test database at the same time.
func migratedTempDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("AUTOBID_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AUTOBID_TEST_DATABASE_URL is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	name := "gen_" + hex.EncodeToString(b)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port)), Path: "/" + name, User: url.UserPassword(cfg.User, cfg.Password), RawQuery: "sslmode=disable"}
	d, err := Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMigrateRefusesNewerDatabase(t *testing.T) {
	d := migratedTempDB(t)
	ctx := context.Background()
	if _, err := d.ExecContext(ctx, `UPDATE schema_version SET generation = $1`, SchemaGeneration+1); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("Migrate on a newer database = %v, want ErrSchemaNewer", err)
	}
}

func TestMigrateRecordsGeneration(t *testing.T) {
	d := migratedTempDB(t)
	ctx := context.Background()
	// A database upgraded before generations existed has the current checksum and generation 0.
	if _, err := d.ExecContext(ctx, `UPDATE schema_version SET generation = 0`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var got int
	if err := d.QueryRowContext(ctx, `SELECT generation FROM schema_version WHERE id = 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != SchemaGeneration {
		t.Fatalf("generation = %d, want %d", got, SchemaGeneration)
	}
}
