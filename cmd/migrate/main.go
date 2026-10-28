// Command migrate 将 db/schema.sql 应用到 AUTOBID_DATABASE_URL 指向的 PostgreSQL。
package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/autobid/autobid-engine/db"
)

func main() {
	dsn := os.Getenv("AUTOBID_DATABASE_URL")
	if dsn == "" {
		log.Fatal("AUTOBID_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	d, err := db.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Print("schema applied")
}
