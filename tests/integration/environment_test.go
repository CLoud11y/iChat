//go:build integration

package integration

import (
	"context"
	"iChat/tests/testenv"
	"iChat/utils"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentIsolation(t *testing.T) {
	// These inherited settings must have no effect on the test resources.
	t.Setenv("ICHAT_CONFIG_FILE", filepath.Join(t.TempDir(), "must-not-read.yml"))
	t.Setenv("ICHAT_TEST_RUN_ID", "development")
	t.Setenv("COMPOSE_FILE", "must-not-read.yml")
	t.Setenv("COMPOSE_PROJECT_NAME", "ichat-development")
	t.Setenv("MYSQL_DATABASE", "iChat")
	first, firstRead, firstWrite, closeFirstClients := openEnvironment(t)
	second, secondRead, secondWrite, _ := openEnvironment(t)
	if first.RunID() == second.RunID() {
		t.Fatal("independent runs reused an identity")
	}
	// Deliberately use identical table/key names, as the legacy code does.
	firstWrite("first")
	secondWrite("second")
	firstRead("first")
	secondRead("second")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Close clients before explicitly reclaiming the first run's resources.
	closeFirstClients()
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	secondRead("second")
	if _, err := first.Config(); err == nil {
		t.Fatal("closed environment still exposes usable configuration")
	}
}

func openEnvironment(t *testing.T) (*testenv.Environment, func(string), func(string), func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(ctx); err != nil {
			t.Errorf("cleanup run %s: %v", env.RunID(), err)
		}
	})
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	rds, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rds.Close() })
	var schema, mysqlVersion string
	if err := db.Raw("SELECT DATABASE(), VERSION()").Row().Scan(&schema, &mysqlVersion); err != nil {
		t.Fatal(err)
	}
	if schema != "ichat_test_"+env.RunID() {
		t.Fatalf("unexpected schema %q", schema)
	}
	info, err := rds.Info(ctx, "server").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(info, "\n") {
		if strings.HasPrefix(line, "redis_version:") {
			t.Logf("run=%s schema=%s mysql=%s %s", env.RunID(), schema, mysqlVersion, strings.TrimSpace(line))
		}
	}
	if err := db.Exec("CREATE TABLE isolation_probe (id INT PRIMARY KEY, value VARCHAR(32) NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	write := func(value string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.WithContext(ctx).Exec("INSERT INTO isolation_probe (id, value) VALUES (1, ?)", value).Error; err != nil {
			t.Fatal(err)
		}
		if err := rds.Set(ctx, "dirty:msgs", value, 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	read := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var got string
		if err := db.WithContext(ctx).Raw("SELECT value FROM isolation_probe WHERE id=1").Scan(&got).Error; err != nil || got != want {
			t.Fatalf("mysql isolation: got=%q want=%q err=%v", got, want, err)
		}
		if got, err := rds.Get(ctx, "dirty:msgs").Result(); err != nil || got != want {
			t.Fatalf("redis isolation: got=%q want=%q err=%v", got, want, err)
		}
	}
	return env, read, write, func() {
		_ = rds.Close()
		_ = pool.Close()
	}
}
