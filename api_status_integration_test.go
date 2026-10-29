//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	"iChat/application"
	"iChat/config"
	"iChat/database"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
)

// Real default API factories, JWT/Redis and SQL snapshots. Commands go through
// PersistCommand; quarantine rows below are explicit status-metadata fixtures.
// This suite does not claim to exercise the MQ/Outbox delivery pipeline.
func TestProductionCommandStatusIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, e := testenv.Start(ctx, filepath.Join("docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if e := env.Close(c); e != nil {
				t.Error(e)
			}
		})
	}
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("production command status fixture run=%s", env.RunID())
	conf, e := env.Config()
	if e != nil {
		t.Fatal(e)
	}
	db, e := utils.OpenMySQL(conf)
	if e != nil {
		t.Fatal(e)
	}
	pool, e := db.DB()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { pool.Close() })
	var schema string
	if e := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&schema); e != nil || schema != "ichat_test_"+env.RunID() {
		t.Fatal("refusing writes outside owned schema")
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal(e)
	}
	if e := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); e != nil {
		t.Fatal(e)
	}
	users := make([]models.User, 3)
	for i := range users {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: v, Phone: v, Name: "command-status-fixture"}
	}
	if e := db.WithContext(ctx).Create(&users).Error; e != nil {
		t.Fatal(e)
	}
	group := models.Group{Name: "command-status", OwnerId: users[0].ID, MaxUser: 50}
	if e := db.WithContext(ctx).Create(&group).Error; e != nil {
		t.Fatal(e)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if e := db.WithContext(ctx).Create(&relations).Error; e != nil {
		t.Fatal(e)
	}
	repo, _ := database.NewMessageRepository(pool)
	command := func(n int, kind messaging.TargetType, target uint) messaging.Command {
		t.Helper()
		c, e := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("b302bb05-f123-4234-8234-%012d", n), TargetType: kind, TargetID: strconv.FormatUint(uint64(target), 10), Media: messaging.Audio, Content: fmt.Sprintf("PRIVATE_STATUS_%d", n)})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	persisted, rejected, groupCommand := command(1, messaging.Private, users[1].ID), command(2, messaging.Private, users[2].ID), command(3, messaging.Group, group.ID)
	for _, c := range []messaging.Command{persisted, rejected, groupCommand} {
		if _, e := repo.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
	}
	pending, quarantine := command(4, messaging.Private, users[1].ID), command(5, messaging.Private, users[1].ID)
	addQuarantine := func(c messaging.Command, id string) {
		t.Helper()
		if _, e := pool.ExecContext(ctx, `INSERT INTO message_quarantine
 (transport_id,command_id,sender_id,client_message_id,payload_hash,raw_body,raw_headers,error_code,error_detail,state,created_at)
 VALUES (?,?,?,?,?,?,'{"PRIVATE_HEADER":"secret"}','SQL_DRIVER_ERROR','PRIVATE_DIAGNOSTIC','open',UTC_TIMESTAMP(6))`, id, c.CommandID, users[0].ID, c.ClientMessageID, c.PayloadHash, []byte("PRIVATE_RAW_BODY")); e != nil {
			t.Fatal(e)
		}
	}
	addQuarantine(quarantine, "status-quarantine")
	base := apiTestConfig()
	base.MYSQL, base.REDIS = conf.MYSQL, conf.REDIS
	client, e := utils.OpenRedis(ctx, base)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { client.Close() })
	tokens, e := utils.NewTokenService(base, utils.NewRedisTokenBlacklist(client))
	if e != nil {
		t.Fatal(e)
	}
	jwt := make([]string, len(users))
	for i, u := range users {
		jwt[i], e = tokens.GenerateToken(u.ID)
		if e != nil {
			t.Fatal(e)
		}
	}
	start := func(c config.Config) *apiService {
		t.Helper()
		o := defaultAPIOptions(c)
		o.Address = "127.0.0.1:0"
		o.ProbeInterval, o.ProbeTimeout = 100*time.Millisecond, 300*time.Millisecond
		s, e := newAPIService(c, application.RoleDependencies{Publisher: &apiPublisher{}, Health: apiTestHealth{}}, o, defaultAPIDependencies())
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { c, stop := context.WithTimeout(context.Background(), 5*time.Second); defer stop(); s.Close(c) })
		setup, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		if e := s.Start(setup, ctx); e != nil {
			t.Fatal(e)
		}
		return s
	}
	s := start(base)
	type reply struct {
		code   int
		body   []byte
		status messaging.CommandStatusResponse
	}
	request := func(s *apiService, token string, q url.Values) reply {
		t.Helper()
		r, e := http.NewRequestWithContext(ctx, "GET", "http://"+apiAddress(s)+"/auth/messages/status?"+q.Encode(), nil)
		if e != nil {
			t.Fatal(e)
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		resp, e := (&http.Client{Timeout: 5 * time.Second}).Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(io.LimitReader(resp.Body, messaging.MaxBodyBytes+1))
		if e != nil || len(body) > messaging.MaxBodyBytes {
			t.Fatal("invalid status response", e)
		}
		if resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("status caching enabled")
		}
		var decoded messaging.CommandStatusResponse
		if e := json.Unmarshal(body, &decoded); e != nil {
			t.Fatal(e)
		}
		return reply{resp.StatusCode, body, decoded}
	}
	query := func(c messaging.Command) url.Values {
		return url.Values{"clientMessageId": {c.ClientMessageID}, "payloadHash": {c.PayloadHash}}
	}
	checkStatus := func(r reply, state messaging.StatusState) {
		t.Helper()
		want := 200
		if state == messaging.StatusConflict {
			want = 409
		}
		if r.code != want || r.status.Data.State != state {
			t.Fatal(r.code, string(r.body))
		}
		if state != messaging.StatusPersisted && strings.Contains(string(r.body), "PRIVATE") {
			t.Fatal("private diagnostics/content leaked")
		}
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	run("five_states_and_canonical_HTTP_DTO", func(t *testing.T) {
		for _, tc := range []struct {
			c     messaging.Command
			state messaging.StatusState
		}{{persisted, messaging.StatusPersisted}, {rejected, messaging.StatusRejected}, {pending, messaging.StatusPendingOrUnknown}, {quarantine, messaging.StatusQuarantined}} {
			r := request(s, jwt[0], query(tc.c))
			checkStatus(r, tc.state)
			expected, e := repo.GetCommandStatus(ctx, uint64(users[0].ID), tc.c.ClientMessageID, tc.c.PayloadHash)
			if e != nil || !reflect.DeepEqual(r.status.Data, expected) {
				t.Fatal("HTTP changed SQL status", e)
			}
			if tc.state == messaging.StatusPendingOrUnknown || tc.state == messaging.StatusQuarantined {
				if r.status.Data.PollAfterMs != 2000 {
					t.Fatal("missing polling advice")
				}
			}
		}
		q := query(persisted)
		q.Set("payloadHash", strings.Repeat("a", 64))
		r := request(s, jwt[0], q)
		checkStatus(r, messaging.StatusConflict)
		if r.status.Code != -1 || r.status.Data.PayloadHash != q.Get("payloadHash") || strings.Contains(string(r.body), persisted.PayloadHash) || r.status.Data.Message != nil || r.status.Data.ReasonCode != "" {
			t.Fatal("conflict exposed canonical result")
		}
	})
	run("sender_scope_strict_query_and_auth", func(t *testing.T) {
		checkStatus(request(s, jwt[1], query(persisted)), messaging.StatusPendingOrUnknown)
		q := query(persisted)
		q.Set("senderId", fmt.Sprint(users[0].ID))
		if r := request(s, jwt[1], q); r.code != 400 {
			t.Fatal(r.code, string(r.body))
		}
		q = query(persisted)
		q.Add("payloadHash", persisted.PayloadHash)
		if r := request(s, jwt[0], q); r.code != 400 {
			t.Fatal(r.code)
		}
		q = query(persisted)
		q.Set("token", jwt[0])
		if r := request(s, "", q); r.code != 401 {
			t.Fatal("query token authorized status")
		}
		if _, e := tokens.RevokeToken(ctx, jwt[2]); e != nil {
			t.Fatal(e)
		}
		if r := request(s, jwt[2], query(persisted)); r.code != 401 {
			t.Fatal("revoked token authorized status")
		}
	})
	run("terminal_priority_and_later_permission_changes", func(t *testing.T) {
		addQuarantine(persisted, "terminal-with-quarantine")
		checkStatus(request(s, jwt[0], query(persisted)), messaging.StatusPersisted)
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=?", models.FriendRelation); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=?", group.ID); e != nil {
			t.Fatal(e)
		}
		checkStatus(request(s, jwt[0], query(persisted)), messaging.StatusPersisted)
		checkStatus(request(s, jwt[0], query(groupCommand)), messaging.StatusPersisted)
		checkStatus(request(s, jwt[0], query(rejected)), messaging.StatusRejected)
	})
	run("corrupt_SQL_result_is_503_without_partial_message", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE message SET content='CORRUPTED_PRIVATE' WHERE id=?", persisted.CommandID); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if _, e := pool.ExecContext(ctx, "UPDATE message SET content=? WHERE id=?", persisted.Content, persisted.CommandID); e != nil {
				t.Error(e)
			}
		}()
		r := request(s, jwt[0], query(persisted))
		if r.code != 503 || r.status.Data.State != "" || r.status.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" || strings.Contains(string(r.body), "PRIVATE") || r.status.Data.Message != nil {
			t.Fatal(r.code, string(r.body))
		}
	})
	run("auth_client_failure_fails_closed_then_fresh_API_recovers", func(t *testing.T) {
		s.mu.Lock()
		ownedRedis := s.resources.redis
		s.mu.Unlock()
		if e := ownedRedis.Close(); e != nil {
			t.Fatal(e)
		}
		r := request(s, jwt[0], query(persisted))
		if r.code != 503 || r.status.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" || r.status.Data.State != "" {
			t.Fatal(r.code, string(r.body))
		}
		// The injected client failure also makes its owner's repeated Close
		// return the known ErrClosed; all requests/probes must still join.
		if e := s.Close(ctx); e != nil && !errors.Is(e, redis.ErrClosed) {
			t.Fatal(e)
		}
		apiWait(t, s.Done())
		s = start(base)
		checkStatus(request(s, jwt[0], query(persisted)), messaging.StatusPersisted)
	})
	// Check the final authoritative collections before the intentional outage.
	for table, want := range map[string]int{"message_command": 3, "message": 2, "conversation": 2, "message_outbox": 5, "message_delivery_cursor": 0, "message_quarantine": 2} {
		var n int
		if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil || n != want {
			t.Fatal("wrong final collection", table, n, e)
		}
	}
	var untouched int
	if e := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0").Scan(&untouched); e != nil || untouched != 5 {
		t.Fatal("status read changed Outbox", untouched, e)
	}
	if e := migrations.ApplyReliable(ctx, pool); e != nil {
		t.Fatal("schema changed", e)
	}
	run("MySQL_offline_existing_and_new_API_return_503_then_recover", func(t *testing.T) {
		if e := env.SetMySQLRunning(ctx, false); e != nil {
			t.Fatal(e)
		}
		for _, c := range []messaging.Command{persisted, pending} {
			r := request(s, jwt[0], query(c))
			if r.code != 503 || r.status.Data.State != "" || r.status.Data.ReasonCode != "DEPENDENCY_UNAVAILABLE" {
				t.Fatal("SQL failure reported pending", r.code, string(r.body))
			}
		}
		if e := s.Close(ctx); e != nil {
			t.Fatal(e)
		}
		s = start(base)
		if r := request(s, jwt[0], query(pending)); r.code != 503 || r.status.Data.State != "" {
			t.Fatal("offline startup fabricated pending", r.code, string(r.body))
		}
		if e := env.SetMySQLRunning(ctx, true); e != nil {
			t.Fatal(e)
		}
		fresh, e := env.Config()
		if e != nil {
			t.Fatal(e)
		}
		if fresh.MYSQL.Host != base.MYSQL.Host || fresh.MYSQL.Port != base.MYSQL.Port {
			// Docker can assign a new mapped port; do not claim a fixed-address
			// pool follows a new address without explicit reconstruction.
			if e := s.Close(ctx); e != nil {
				t.Fatal(e)
			}
			base.MYSQL = fresh.MYSQL
			s = start(base)
			t.Log("Docker changed MySQL port; recovered through a fresh API pool")
		}
		deadline := time.Now().Add(20 * time.Second)
		var recovered bool
		for time.Now().Before(deadline) {
			r := request(s, jwt[0], query(persisted))
			if r.code == 200 {
				checkStatus(r, messaging.StatusPersisted)
				recovered = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !recovered {
			t.Fatal("status failed to recover after owned MySQL restart")
		}
		checkStatus(request(s, jwt[0], query(pending)), messaging.StatusPendingOrUnknown)
	})
	if e := s.Close(ctx); e != nil {
		t.Fatal(e)
	}
}
