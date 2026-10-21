//go:build integration

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging/rabbitmq"
	"iChat/migrations"
	"iChat/tests/testenv"
)

func TestQuarantineCLIIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "..", "docker-compose.test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("quarantine query fixture run=%s", env.RunID())
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	conf, err := env.Config()
	if err != nil {
		t.Fatal(err)
	}
	// Private fixture credentials stay in a temporary file, never CLI arguments
	// or output. Deliberately invalid unrelated sections prove SQL-only loading.
	file := filepath.Join(t.TempDir(), "sql.yml")
	contents := fmt.Sprintf("MYSQL:\n  user: %q\n  password: %q\n  host: %q\n  port: %q\n  dbname: %q\nRABBITMQ: invalid\nRUNTIME:\n  role: invalid\n", conf.MYSQL.User, conf.MYSQL.Password, conf.MYSQL.Host, conf.MYSQL.Port, conf.MYSQL.DbName)
	if err := os.WriteFile(file, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"USER": conf.MYSQL.User, "PASSWORD": conf.MYSQL.Password, "HOST": conf.MYSQL.Host, "PORT": conf.MYSQL.Port, "DBNAME": conf.MYSQL.DbName} {
		t.Setenv("ICHAT_MYSQL_"+key, value)
	}
	t.Setenv("ICHAT_CONFIG_FILE", file)
	options, err := loadQuarantineMySQL(file)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := sql.Open("mysql", options.FormatDSN())
	if err != nil {
		t.Fatal("open fixture SQL failed")
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var schema string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); err != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing fixture writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	repo, _ := database.NewQuarantineRepository(pool)
	inputs := map[string]rabbitmq.QuarantineInput{}
	var ids []string
	for i := 0; i < 6; i++ {
		input := adminQuarantineInput(t, fmt.Sprintf("query-fixture-%d", i), i%2 == 0, i%3 == 0)
		if _, err := repo.Save(ctx, input); err != nil {
			t.Fatal(err)
		}
		id := input.Envelope().TransportID()
		inputs[id], ids = input, append(ids, id)
	}
	sort.Strings(ids)
	for i, state := range []string{"replay_pending", "resolved"} {
		var resolved any
		if state == "resolved" {
			resolved = "2026-10-22 02:03:04.000000"
		}
		if _, err := pool.ExecContext(ctx, "UPDATE message_quarantine SET state=?,replay_attempt_id=?,resolved_at=? WHERE transport_id=?", state, "8fd7c2ef-ae26-4bb5-91a5-32a302bb05f1", resolved, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	query := func(t *testing.T, args ...string) []byte {
		t.Helper()
		var output bytes.Buffer
		if err := run(append([]string{"quarantine"}, args...), &output); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	snapshot := func() [][]string {
		t.Helper()
		rows, err := pool.QueryContext(ctx, "SELECT transport_id,COALESCE(sender_id,''),COALESCE(client_message_id,''),COALESCE(command_id,''),COALESCE(payload_hash,''),HEX(raw_body),CAST(raw_headers AS CHAR),error_code,error_detail,state,COALESCE(replay_attempt_id,''),DATE_FORMAT(created_at,'%Y-%m-%d %H:%i:%s.%f'),COALESCE(DATE_FORMAT(resolved_at,'%Y-%m-%d %H:%i:%s.%f'),'') FROM message_quarantine ORDER BY transport_id")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result [][]string
		for rows.Next() {
			values := make([]string, 13)
			targets := make([]any, len(values))
			for i := range values {
				targets[i] = &values[i]
			}
			if err := rows.Scan(targets...); err != nil {
				t.Fatal(err)
			}
			result = append(result, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := snapshot()
	t.Run("state_filters_and_keyset_pages", func(t *testing.T) {
		for state, want := range map[string]int{"open": 4, "replay_pending": 1, "resolved": 1, "all": 6} {
			var page database.QuarantinePage
			if err := json.Unmarshal(query(t, "list", "--state", state), &page); err != nil || len(page.Records) != want {
				t.Fatal("state filter failed", state, err)
			}
		}
		var defaultPage database.QuarantinePage
		if err := json.Unmarshal(query(t, "list"), &defaultPage); err != nil || len(defaultPage.Records) != 4 {
			t.Fatal("default must list only open", err)
		}
		var seen []string
		after := ""
		for i := 0; i < 7; i++ {
			var page database.QuarantinePage
			if err := json.Unmarshal(query(t, "list", "--state", "all", "--limit", "1", "--after", after), &page); err != nil || len(page.Records) != 1 {
				t.Fatal("page failed", err)
			}
			seen = append(seen, page.Records[0].TransportID)
			if page.Next == "" {
				break
			}
			if page.Next <= after {
				t.Fatal("cursor did not advance")
			}
			after = page.Next
		}
		if !reflect.DeepEqual(seen, ids) {
			t.Fatal("pages lost or duplicated records")
		}
		var empty database.QuarantinePage
		if err := json.Unmarshal(query(t, "list", "--state", "all", "--after", ids[5]), &empty); err != nil || empty.Records == nil || len(empty.Records) != 0 || empty.Next != "" {
			t.Fatal("empty page failed", err)
		}
	})
	t.Run("verified_detail_and_exact_typed_source", func(t *testing.T) {
		for _, id := range ids {
			var view struct {
				database.QuarantineSummary
				AuditVerified bool            `json:"audit_verified"`
				Body          []byte          `json:"raw_body_base64"`
				Audit         json.RawMessage `json:"audit"`
			}
			if err := json.Unmarshal(query(t, "show", "--transport-id", id), &view); err != nil || !view.AuditVerified || len(view.Audit) != 0 || len(view.Body) != 0 {
				t.Fatal("default detail exposed source or failed verification", err)
			}
			if err := json.Unmarshal(query(t, "show", "--transport-id", id, "--include-raw"), &view); err != nil || view.TransportID != id {
				t.Fatal(err)
			}
			input, err := rabbitmq.DecodeQuarantineAudit(view.Audit)
			if err != nil {
				t.Fatal(err)
			}
			a, _ := inputs[id].EncodeAudit()
			b, _ := input.EncodeAudit()
			source, _ := input.Envelope().Source()
			if !bytes.Equal(a, b) || !bytes.Equal(source.Body, view.Body) || view.BodyBytes != int64(len(source.Body)) {
				t.Fatal("typed source, identity or first observation changed")
			}
			_, identified := input.Identity()
			if identified != (view.SenderID != nil) || identified != (view.PayloadHash != nil) {
				t.Fatal("nullable business identity changed")
			}
		}
	})
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("read commands changed audit columns")
	}
	t.Run("missing_or_corrupt_audit_never_yields_verified_detail", func(t *testing.T) {
		var output bytes.Buffer
		if err := run([]string{"quarantine", "show", "--transport-id", "mq:v1:" + strings.Repeat("0", 64)}, &output); !errors.Is(err, database.ErrQuarantineNotFound) || output.Len() != 0 {
			t.Fatal("missing record reported success", err)
		}
		id := ids[5]
		if _, err := pool.ExecContext(ctx, "UPDATE message_quarantine SET raw_headers=JSON_OBJECT('audit_version',99) WHERE transport_id=?", id); err != nil {
			t.Fatal(err)
		}
		corrupt := snapshot()
		if err := run([]string{"quarantine", "show", "--transport-id", id, "--include-raw"}, &output); !errors.Is(err, database.ErrQuarantineInvariant) || output.Len() != 0 {
			t.Fatal("corrupt record reported success", err)
		}
		var page database.QuarantinePage
		if err := json.Unmarshal(query(t, "list", "--state", "all"), &page); err != nil || len(page.Records) != 6 {
			t.Fatal("metadata listing hid corrupt record", err)
		}
		if !reflect.DeepEqual(corrupt, snapshot()) {
			t.Fatal("failed detail modified corrupt audit")
		}
	})
	for _, table := range []string{"conversation", "message", "message_command", "message_outbox", "message_delivery_cursor"} {
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM `"+table+"`").Scan(&count); err != nil || count != 0 {
			t.Fatal("query wrote business state", table, err)
		}
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema fingerprint changed", err)
	}
}
