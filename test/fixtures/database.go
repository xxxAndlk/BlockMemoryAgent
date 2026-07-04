//go:build integration

// Package fixtures provides shared integration-test infrastructure for
// BlockMemoryAgent. The database fixture can spin up Postgres + Redis via
// docker-compose, apply migrations, and truncate tables between tests.
package fixtures

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"
)

const (
	// migrationDir is relative to the repository root.
	migrationDir = "../../migrations"
	// composeFile is relative to the repository root.
	composeFile = "../../docker/docker-compose.yml"
	// defaultPostgresDSN matches the docker-compose credentials.
	defaultPostgresDSN = "postgres://blockmemory:blockmemory_dev@localhost:5432/blockmemory?sslmode=disable"
	// defaultRedisAddr matches the docker-compose port mapping.
	defaultRedisAddr = "localhost:6380"
)

// TestDatabase holds the connection details for the ephemeral Postgres + Redis
// services used by a single test or test suite. Call Truncate between tests and
// Cleanup when finished.
type TestDatabase struct {
	PostgresDSN  string
	RedisAddr    string
	RedisPass    string
	RedisDB      int
	postgresDB   *sql.DB
	redisClient  *redis.Client
	cleanupFuncs []func()
	t            testing.TB
}

// NewTestDatabase starts Postgres and Redis for integration tests. It tries, in
// order: docker-compose, environment-provided services, and finally skips the
// test if neither works.
func NewTestDatabase(t testing.TB) *TestDatabase {
	d := &TestDatabase{
		PostgresDSN: defaultPostgresDSN,
		RedisAddr:   defaultRedisAddr,
		RedisPass:   "blockmemory_dev",
		RedisDB:     5,
		t:           t,
	}

	composeOK := d.tryDockerCompose()
	if !composeOK {
		envOK := d.tryEnvServices()
		if !envOK {
			t.Skip("Postgres/Redis unavailable: docker-compose failed and env services not reachable")
		}
	}

	if err := d.applyMigrations(); err != nil {
		d.Cleanup()
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(d.Cleanup)
	return d
}

// tryDockerCompose attempts to start containers with the project compose file.
// It returns true only if both Postgres and Redis become reachable.
func (d *TestDatabase) tryDockerCompose() bool {
	if !commandExists("docker-compose") && !commandExists("docker") {
		return false
	}

	composePath, err := filepath.Abs(composeFile)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker-compose", "-f", composePath, "-p", "blockmemory-test", "up", "-d")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// Try docker compose (plugin form).
		cmd = exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "-p", "blockmemory-test", "up", "-d")
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return false
		}
	}

	d.cleanupFuncs = append(d.cleanupFuncs, func() {
		_ = exec.Command("docker-compose", "-f", composePath, "-p", "blockmemory-test", "down", "-v").Run()
		_ = exec.Command("docker", "compose", "-f", composePath, "-p", "blockmemory-test", "down", "-v").Run()
	})

	return d.waitForServices(30 * time.Second)
}

// tryEnvServices checks whether Postgres and Redis are already reachable using
// default or environment-provided addresses.
func (d *TestDatabase) tryEnvServices() bool {
	if v := os.Getenv("POSTGRES_DSN"); v != "" {
		d.PostgresDSN = v
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		d.RedisAddr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		d.RedisPass = v
	}
	return d.waitForServices(10 * time.Second)
}

// waitForServices pings both Postgres and Redis until timeout.
func (d *TestDatabase) waitForServices(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pgOK := d.pingPostgres()
		redisOK := d.pingRedis()
		if pgOK && redisOK {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// pingPostgres opens a short-lived connection to Postgres.
func (d *TestDatabase) pingPostgres() bool {
	db, err := sql.Open("postgres", d.PostgresDSN)
	if err != nil {
		return false
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return db.PingContext(ctx) == nil
}

// pingRedis opens a short-lived connection to Redis.
func (d *TestDatabase) pingRedis() bool {
	client := redis.NewClient(&redis.Options{
		Addr:     d.RedisAddr,
		Password: d.RedisPass,
		DB:       d.RedisDB,
	})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return client.Ping(ctx).Err() == nil
}

// applyMigrations runs 001_init.sql through 006_session_history_meta_memory.sql
// against the Postgres instance. Migration files are read from ../../migrations
// relative to this package.
func (d *TestDatabase) applyMigrations() error {
	db, err := sql.Open("postgres", d.PostgresDSN)
	if err != nil {
		return err
	}
	defer db.Close()

	for i := 1; i <= 6; i++ {
		name := fmt.Sprintf("%03d_*.sql", i)
		matches, err := filepath.Glob(filepath.Join(migrationDir, name))
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			return fmt.Errorf("migration %03d not found", i)
		}
		data, err := os.ReadFile(matches[0])
		if err != nil {
			return err
		}
		if _, err := db.ExecContext(context.Background(), string(data)); err != nil {
			return fmt.Errorf("exec migration %s: %w", matches[0], err)
		}
	}

	d.postgresDB = db
	return nil
}

// PostgresDB returns the long-lived *sql.DB connection used by the fixture.
func (d *TestDatabase) PostgresDB() *sql.DB {
	return d.postgresDB
}

// RedisClient returns the long-lived redis client used by the fixture.
func (d *TestDatabase) RedisClient() *redis.Client {
	if d.redisClient == nil {
		d.redisClient = redis.NewClient(&redis.Options{
			Addr:     d.RedisAddr,
			Password: d.RedisPass,
			DB:       d.RedisDB,
		})
	}
	return d.redisClient
}

// Truncate removes data from all application tables while keeping schema. It
// should be called between tests to provide isolation.
func (d *TestDatabase) Truncate() {
	if d.postgresDB == nil {
		return
	}
	tables := []string{
		"agent_private_memory",
		"agent_snapshots",
		"topics",
		"global_knowledge",
		"agent_registry",
		"decision_logs",
		"session_history",
		"session_events",
		"session_logs",
		"dag_jobs",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, table := range tables {
		if _, err := d.postgresDB.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s CASCADE", table)); err != nil {
			d.t.Logf("truncate %s: %v", table, err)
		}
	}
	if d.redisClient != nil {
		_ = d.redisClient.FlushDB(ctx).Err()
	}
}

// Cleanup stops docker-compose containers and closes database clients.
func (d *TestDatabase) Cleanup() {
	if d.redisClient != nil {
		_ = d.redisClient.Close()
	}
	if d.postgresDB != nil {
		_ = d.postgresDB.Close()
	}
	for i := len(d.cleanupFuncs) - 1; i >= 0; i-- {
		d.cleanupFuncs[i]()
	}
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// RepositoryRoot returns the absolute path to the BlockMemoryAgent repository
// root. It walks up from this file using runtime.Caller.
func RepositoryRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}
