// Package testpg gives each test its own migrated PostgreSQL database on the server named by
// HOLDFAST_TEST_PG (an admin DSN; `make pg-up` prints one). Without the variable the test is skipped.
package testpg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kabsdk/holdfast/internal/server/store"
)

// DSN creates a fresh, migrated database and returns its DSN. It is dropped when the test ends.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := EmptyDSN(t)
	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return dsn
}

// EmptyDSN creates a fresh database with no table in it, as a rebuilt server's is, and returns its DSN. It is dropped
// when the test ends.
func EmptyDSN(t testing.TB) string {
	t.Helper()
	admin := os.Getenv("HOLDFAST_TEST_PG")
	if admin == "" {
		t.Skip("HOLDFAST_TEST_PG is not set (make pg-up prints one)")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "hf_test_" + hex.EncodeToString(b)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		_ = conn.Close(context.Background())
	})
	cfg, _ := pgx.ParseConfig(admin)
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// Store opens a fresh, migrated database.
func Store(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
