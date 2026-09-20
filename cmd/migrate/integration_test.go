//go:build integration

package main

import (
	"context"
	"encoding/json"
	"iChat/config"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMigrationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("migration fixture run=%s", env.RunID())
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
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	db = db.WithContext(ctx)
	var schema string
	if err := db.Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil {
		t.Fatal(err)
	}
	if schema != "ichat_test_"+env.RunID() {
		t.Fatalf("refusing migration test writes in schema %q", schema)
	}
	var tables []string
	if err := db.Raw("SHOW TABLES").Scan(&tables).Error; err != nil || len(tables) != 0 {
		t.Fatalf("connection constructor changed schema: tables=%v err=%v", tables, err)
	}
	// The command must succeed even with an unusable Redis configuration.
	conf.REDIS.Addr = "127.0.0.1:0"
	conf.REDIS.Password = "not-a-redis-password"
	writeConfig := func(conf config.Config) string {
		t.Helper()
		file := filepath.Join(t.TempDir(), "config.json")
		body, err := json.Marshal(conf)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
		return file
	}
	file := writeConfig(conf)
	checkFailure := func(file, want string) {
		t.Helper()
		stdout, stderr, code := migrationProcess(t, file)
		if code != 1 || !strings.Contains(stderr, want) || strings.Contains(stdout, "migration completed") {
			t.Fatalf("exit=%d stdout=%q stderr=%q; want failure %q", code, stdout, stderr, want)
		}
	}
	badConf := conf
	badConf.MYSQL.Password += "-invalid"
	checkFailure(writeConfig(badConf), "open mysql")
	// A view with a model's table name causes an actual DDL failure, after a
	// successful connection. Only this run's empty schema is modified.
	if err := db.Exec("CREATE VIEW `user` AS SELECT 1 AS id").Error; err != nil {
		t.Fatal(err)
	}
	checkFailure(file, "migrate legacy schema")
	if err := db.Exec("DROP VIEW `user`").Error; err != nil {
		t.Fatal(err)
	}
	checkSuccess := func() {
		t.Helper()
		stdout, stderr, code := migrationProcess(t, file)
		if code != 0 || !strings.Contains(stdout, "legacy schema migration completed") || stderr != "" {
			t.Fatalf("exit=%d stdout=%q stderr=%q; want successful migration", code, stdout, stderr)
		}
	}
	checkSuccess()
	tables = nil
	if err := db.Raw("SHOW TABLES").Scan(&tables).Error; err != nil {
		t.Fatal(err)
	}
	if want := []string{"group", "legacy_message_quarantine", "message", "relation", "user"}; !reflect.DeepEqual(tables, want) {
		t.Fatalf("tables=%v, want %v", tables, want)
	}
	user := models.User{Identifier: "migration-user", Name: "preserved", Phone: "migration-phone", Password: "fixture-only"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	message := models.Message{Id: env.RunID(), SenderId: user.ID, ReceiverId: user.ID, Type: 2, Media: 1, Content: "保留已有消息", TimeStamp: 1700000000000}
	if err := db.Create(&message).Error; err != nil {
		t.Fatal(err)
	}
	audit := models.LegacyMessageQuarantine{PayloadHash: strings.Repeat("a", 64), RawBody: []byte{0xff, 0, 0xfe}, ReasonCode: "INVALID_JSON", ReasonDetail: "preserve raw bytes"}
	if err := db.Create(&audit).Error; err != nil {
		t.Fatal(err)
	}
	checkSuccess()
	var gotAudit models.LegacyMessageQuarantine
	if err := db.Where("payload_hash = ?", audit.PayloadHash).Take(&gotAudit).Error; err != nil || !reflect.DeepEqual(gotAudit.RawBody, audit.RawBody) || gotAudit.ReasonCode != audit.ReasonCode || gotAudit.ReasonDetail != audit.ReasonDetail {
		t.Fatalf("audit changed on repeat migration: %+v error=%v", gotAudit, err)
	}
	var got models.Message
	if err := db.First(&got, "id = ?", message.Id).Error; err != nil || !reflect.DeepEqual(got, message) {
		t.Fatalf("message after repeat migration=%+v, want %+v, err=%v", got, message, err)
	}
	var gotUser models.User
	if err := db.First(&gotUser, user.ID).Error; err != nil || gotUser.Name != user.Name || gotUser.Identifier != user.Identifier || gotUser.Phone != user.Phone || gotUser.Password != user.Password {
		t.Fatalf("user changed after repeat migration: err=%v", err)
	}
	for _, table := range []string{"user", "message", "legacy_message_quarantine"} {
		var count int64
		if err := db.Table(table).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v, want 1", table, count, err)
		}
	}
}
