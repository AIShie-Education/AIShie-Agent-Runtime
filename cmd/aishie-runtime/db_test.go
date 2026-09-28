package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// scratchDatabase is an empty database, aishie_worker_t_<random>, on the
// server at TEST_DATABASE_URL (the local one by default), dropped when t
// ends. Where PostgreSQL cannot be reached the test is skipped, unless CI
// is set.
func scratchDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("TEST_DATABASE_URL")
	if admin == "" {
		admin = "postgres:///postgres"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		if ci, _ := strconv.ParseBool(os.Getenv("CI")); !ci {
			t.Skipf("PostgreSQL cannot be reached: %v", err)
		}
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "aishie_worker_t_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if c, err := pgx.Connect(ctx, admin); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
