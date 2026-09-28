//go:build integration

package database

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestConcurrentMigrateOnFreshSchema(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)

	schema := "migration_race_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})

	separator := "?"
	if strings.Contains(databaseURL, "?") {
		separator = "&"
	}
	schemaURL := fmt.Sprintf("%s%ssearch_path=%s", databaseURL, separator, schema)
	pool, err := Open(ctx, schemaURL)
	if err != nil {
		t.Fatalf("open schema pool: %v", err)
	}
	defer pool.Close()

	const attempts = 8
	start := make(chan struct{})
	errorsByAttempt := make(chan error, attempts)
	var waitGroup sync.WaitGroup
	for range attempts {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			errorsByAttempt <- Migrate(ctx, pool)
		}()
	}
	close(start)
	waitGroup.Wait()
	close(errorsByAttempt)

	for err := range errorsByAttempt {
		if err != nil {
			t.Fatalf("concurrent migration failed: %v", err)
		}
	}

	var applied int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied migrations=%d, want 1", applied)
	}
}
