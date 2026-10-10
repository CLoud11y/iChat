//go:build integration

package migrations_test

import (
	"context"
	"database/sql"
	"iChat/migrations"
	"iChat/tests/testenv"
	"iChat/utils"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReliableSchemaIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("reliable schema fixture run=%s", env.RunID())
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(closeCtx); err != nil {
			t.Error(err)
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
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatalf("refusing writes in schema %q: %v", schema, err)
	}
	// A pre-existing legacy table must never be reinterpreted as a new schema.
	mustExec(t, ctx, pool, "CREATE TABLE `user` (`id` bigint PRIMARY KEY)")
	wantError(t, migrations.ApplyReliable(ctx, pool))
	var markerCount int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='reliability_schema_migration'").Scan(&markerCount); err != nil || markerCount != 0 {
		t.Fatalf("legacy preflight changed schema: count=%d err=%v", markerCount, err)
	}
	mustExec(t, ctx, pool, "DROP TABLE `user`")
	// Both callers must see one complete v1 schema, even if they start together.
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- migrations.ApplyReliable(ctx, pool) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("concurrent migration: %v", err)
		}
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	// The old AutoMigrate path must not alter the new message table.
	wantError(t, utils.MigrateLegacy(db.WithContext(ctx)))
	var tableCount int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tableCount); err != nil || tableCount != 7 {
		t.Fatalf("tables=%d err=%v; want six plus version marker", tableCount, err)
	}
	for _, tc := range []struct{ table, column, dataType, collation string }{
		{"message", "id", "char", "ascii_bin"},
		{"message", "content", "varchar", "utf8mb4_bin"},
		{"message", "seq", "bigint", ""},
		{"message_command", "payload_hash", "char", "ascii_bin"},
		{"conversation", "last_seq", "bigint", ""},
	} {
		var dataType string
		var collation sql.NullString
		if err := pool.QueryRowContext(ctx, "SELECT data_type, collation_name FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?", tc.table, tc.column).Scan(&dataType, &collation); err != nil || dataType != tc.dataType || collation.String != tc.collation {
			t.Fatalf("%s.%s type=%s collation=%s err=%v", tc.table, tc.column, dataType, collation.String, err)
		}
	}
	now := time.Date(2026, 10, 11, 12, 0, 0, 123456000, time.UTC)
	mustExec(t, ctx, pool, "INSERT INTO conversation (id,type,private_user_a,private_user_b,last_seq,updated_at) VALUES ('p:1:2','private',1,2,1,?)", now)
	wantExecError(t, ctx, pool, "INSERT INTO conversation (id,type,private_user_a,private_user_b,updated_at) VALUES ('p:2:1','private',2,1,?)", now)
	wantExecError(t, ctx, pool, "INSERT INTO conversation (id,type,private_user_b,updated_at) VALUES ('p:1:2','private',2,?)", now)
	mustExec(t, ctx, pool, "INSERT INTO conversation (id,type,group_id,updated_at) VALUES ('g:3','group',3,?)", now)
	wantExecError(t, ctx, pool, "INSERT INTO conversation (id,type,group_id,updated_at) VALUES ('g:03','group',3,?)", now)
	commandID := strings.Repeat("a", 64)
	hash := strings.Repeat("b", 64)
	clientID := "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1"
	content := "  中文🙂\x00  "
	messageInsert := "INSERT INTO message (id,client_message_id,sender_id,receiver_id,type,media,content,conversation_id,seq,client_sent_at,server_created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)"
	mustExec(t, ctx, pool, messageInsert, commandID, clientID, uint64(1), uint64(2), "private", "text", content, "p:1:2", uint64(1), now, now)
	var got string
	if err := pool.QueryRowContext(ctx, "SELECT content FROM message WHERE id=?", commandID).Scan(&got); err != nil || got != content {
		t.Fatalf("content=%q err=%v", got, err)
	}
	precise := time.Date(2026, 10, 11, 12, 0, 0, 123456789, time.UTC)
	// Explicit truncation is required: the MySQL driver/server round extra digits.
	storedPrecision := precise.Truncate(time.Microsecond)
	mustExec(t, ctx, pool, messageInsert, strings.Repeat("f", 64), clientID, 1, 3, "group", "text", strings.Repeat("界", 1024), "g:3", 1, storedPrecision, storedPrecision)
	var storedTime time.Time
	if err := pool.QueryRowContext(ctx, "SELECT client_sent_at FROM message WHERE id=?", strings.Repeat("f", 64)).Scan(&storedTime); err != nil || storedTime.Nanosecond() != 123456000 {
		t.Fatalf("DATETIME(6) value=%v err=%v", storedTime, err)
	}
	wantExecError(t, ctx, pool, messageInsert, strings.Repeat("c", 64), clientID, 1, 2, "private", "text", "other", "p:1:2", 1, now, now)
	wantExecError(t, ctx, pool, messageInsert, strings.Repeat("C", 64), clientID, 1, 2, "private", "text", "other", "p:1:2", 2, now, now)
	wantExecError(t, ctx, pool, messageInsert, strings.Repeat("c", 64), clientID, 1, 2, "private", "text", "other", "p:1:2", 0, now, now)
	wantExecError(t, ctx, pool, messageInsert, strings.Repeat("c", 64), clientID, 1, 2, "private", "text", strings.Repeat("界", 1025), "p:1:2", 2, now, now)
	wantExecError(t, ctx, pool, messageInsert, strings.Repeat("c", 64), clientID, 1, 2, "private", "video", "other", "p:1:2", 2, now, now)
	commandInsert := "INSERT INTO message_command (sender_id,client_message_id,command_id,payload_hash,state,reason_code,message_id,conversation_id,created_at,completed_at) VALUES (?,?,?,?,?,?,?,?,?,?)"
	mustExec(t, ctx, pool, commandInsert, 1, clientID, commandID, hash, "persisted", nil, commandID, "p:1:2", now, now)
	wantExecError(t, ctx, pool, commandInsert, 1, clientID, commandID, hash, "persisted", nil, commandID, "p:1:2", now, now)
	wantExecError(t, ctx, pool, commandInsert, 2, clientID, commandID, hash, "persisted", nil, commandID, "p:1:2", now, now)
	wantExecError(t, ctx, pool, commandInsert, 2, clientID, strings.Repeat("c", 64), hash, "processing", nil, nil, nil, now, now)
	wantExecError(t, ctx, pool, commandInsert, 2, clientID, strings.Repeat("c", 64), hash, "persisted", nil, nil, "p:1:2", now, now)
	mustExec(t, ctx, pool, commandInsert, 2, clientID, strings.Repeat("c", 64), hash, "rejected", "NO_PERMISSION", nil, nil, now, now)
	outboxInsert := "INSERT INTO message_outbox (event_id,event_type,command_id,payload,status,available_at,created_at) VALUES (?,?,?,?,?,?,?)"
	mustExec(t, ctx, pool, outboxInsert, strings.Repeat("d", 64), "message.persisted", commandID, `{"seq":"1"}`, "pending", now, now)
	wantExecError(t, ctx, pool, outboxInsert, strings.Repeat("e", 64), "message.persisted", commandID, `{}`, "pending", now, now)
	wantExecError(t, ctx, pool, outboxInsert, strings.Repeat("e", 64), "command.result", commandID, `{}`, "leased", now, now)
	mustExec(t, ctx, pool, "INSERT INTO message_delivery_cursor (user_id,conversation_id,delivered_seq,updated_at) VALUES (?,?,?,?)", uint64(18446744073709551615), "p:1:2", uint64(1), now)
	wantExecError(t, ctx, pool, "INSERT INTO message_delivery_cursor (user_id,conversation_id,delivered_seq,updated_at) VALUES (?,?,?,?)", 3, "missing", 0, now)
	raw := []byte{0xff, 0, 0xfe, 0x41}
	mustExec(t, ctx, pool, "INSERT INTO message_quarantine (transport_id,raw_body,raw_headers,error_code,error_detail,created_at) VALUES (?,?,?,?,?,?)", "mq:1", raw, `{"retry":1}`, "BAD_ENVELOPE", "bad bytes", now)
	var stored []byte
	if err := pool.QueryRowContext(ctx, "SELECT raw_body FROM message_quarantine WHERE transport_id='mq:1'").Scan(&stored); err != nil || string(stored) != string(raw) {
		t.Fatalf("quarantine body=%x err=%v", stored, err)
	}
	wantExecError(t, ctx, pool, "INSERT INTO message_quarantine (transport_id,raw_body,error_code,error_detail,created_at) VALUES (?,?,?,?,?)", "mq:1", raw, "BAD_ENVELOPE", "duplicate", now)
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatalf("rerun with data: %v", err)
	}
	mustExec(t, ctx, pool, "ALTER TABLE message ADD INDEX idx_drift_test (sender_id)")
	wantError(t, migrations.ApplyReliable(ctx, pool))
	mustExec(t, ctx, pool, "DELETE FROM reliability_schema_migration")
	wantError(t, migrations.ApplyReliable(ctx, pool))
}

func mustExec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("execute %s: %v", query, err)
	}
}

func wantExecError(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	_, err := db.ExecContext(ctx, query, args...)
	wantError(t, err)
}

func wantError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a database constraint or migration error")
	}
}
