//go:build integration

package main

import (
	"context"
	"database/sql"
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

// Default production API factories with real JWT/Redis/SQL. Commands are
// produced by PersistCommand; the MQ capability is explicitly unused.
func TestProductionConversationsIntegration(t *testing.T) {
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
	t.Logf("production conversations fixture run=%s", env.RunID())
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
	users := make([]models.User, 5)
	for i, id := range []uint{1, 2, 10, 11, 99} {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], id)
		users[i] = models.User{Identifier: v, Phone: v, Name: "conversation-http-fixture"}
		users[i].ID = id
	}
	if e := db.WithContext(ctx).Create(&users).Error; e != nil {
		t.Fatal(e)
	}
	groups := make([]models.Group, 5)
	for i, id := range []uint{1, 2, 10, 20, 30} {
		groups[i] = models.Group{Name: fmt.Sprint("http-group-", id), OwnerId: 1, MaxUser: 50}
		groups[i].ID = id
	}
	if e := db.WithContext(ctx).Create(&groups).Error; e != nil {
		t.Fatal(e)
	}
	relations := []models.Relation{{OwnerId: 1, TargetId: 2, Type: models.FriendRelation}, {OwnerId: 2, TargetId: 1, Type: models.FriendRelation}, {OwnerId: 1, TargetId: 10, Type: models.FriendRelation}, {OwnerId: 10, TargetId: 1, Type: models.FriendRelation}, {OwnerId: 2, TargetId: 11, Type: models.FriendRelation}, {OwnerId: 11, TargetId: 2, Type: models.FriendRelation}, {OwnerId: 1, TargetId: 1, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 2, Type: models.GroupRelation}, {OwnerId: 2, TargetId: 2, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 10, Type: models.GroupRelation}, {OwnerId: 11, TargetId: 20, Type: models.GroupRelation}, {OwnerId: 1, TargetId: 30, Type: models.GroupRelation}}
	if e := db.WithContext(ctx).Create(&relations).Error; e != nil {
		t.Fatal(e)
	}
	repo, _ := database.NewMessageRepository(pool)
	var commands []messaging.Command
	original := map[string]messaging.CommandStatus{}
	persist := func(sender, target uint64, kind messaging.TargetType) {
		t.Helper()
		c, e := messaging.Normalize(sender, messaging.SendRequest{ClientMessageID: fmt.Sprintf("e2605f1b-1234-4234-8234-%012d", len(commands)+1), TargetType: kind, TargetID: strconv.FormatUint(target, 10), Media: messaging.Text, Content: fmt.Sprintf("PRIVATE_CONVERSATIONS_%d", len(commands))})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := repo.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
		s, e := repo.GetCommandStatus(ctx, sender, c.ClientMessageID, c.PayloadHash)
		if e != nil || s.State != messaging.StatusPersisted {
			t.Fatal(e)
		}
		original[c.CommandID] = s
	}
	for i := 0; i < 3; i++ {
		persist(1, 2, messaging.Private)
	}
	persist(1, 10, messaging.Private)
	persist(2, 11, messaging.Private)
	persist(1, 2, messaging.Group)
	persist(1, 10, messaging.Group)
	persist(11, 20, messaging.Group)
	for _, a := range []struct {
		u   uint64
		id  string
		seq messaging.Sequence
	}{{1, "p:1:2", 2}, {2, "p:1:2", 3}, {1, "g:2", 1}} {
		if _, e := repo.AcknowledgeDelivery(ctx, a.u, a.id, a.seq); e != nil {
			t.Fatal(e)
		}
	}
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
	jwt := map[uint]string{}
	for _, u := range users {
		jwt[u.ID], e = tokens.GenerateToken(u.ID)
		if e != nil {
			t.Fatal(e)
		}
	}
	start := func(c config.Config) *apiService {
		t.Helper()
		o := defaultAPIOptions(c)
		o.Address = "127.0.0.1:0"
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
		code     int
		body     []byte
		envelope messaging.ConversationsResponse
		reason   string
	}
	request := func(s *apiService, token string, q url.Values) reply {
		t.Helper()
		r, e := http.NewRequestWithContext(ctx, "GET", "http://"+apiAddress(s)+"/auth/conversations?"+q.Encode(), nil)
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
		body, e := io.ReadAll(io.LimitReader(resp.Body, 128*1024+1))
		if e != nil || len(body) > 128*1024 {
			t.Fatal("unbounded HTTP list response", e)
		}
		if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatal("wrong response headers")
		}
		var decoded messaging.ConversationsResponse
		var errorBody struct {
			Data struct {
				Reason string `json:"reasonCode"`
			} `json:"data"`
		}
		if e := json.Unmarshal(body, &decoded); e != nil {
			t.Fatal(e)
		}
		json.Unmarshal(body, &errorBody)
		return reply{resp.StatusCode, body, decoded, errorBody.Data.Reason}
	}
	page := func(reader uint, q url.Values) messaging.ConversationPage {
		t.Helper()
		r := request(s, jwt[reader], q)
		if r.code != 200 || r.envelope.Code != 0 || strings.Contains(string(r.body), "PRIVATE") {
			t.Fatal(r.code, string(r.body))
		}
		return r.envelope.Data
	}
	ids := func(p messaging.ConversationPage) []string {
		r := make([]string, 0, len(p.Items))
		for _, v := range p.Items {
			r = append(r, v.ConversationID)
		}
		return r
	}
	item := func(p messaging.ConversationPage, id string) messaging.ConversationSummary {
		t.Helper()
		for _, v := range p.Items {
			if v.ConversationID == id {
				return v
			}
		}
		t.Fatal("missing authorized conversation", id)
		return messaging.ConversationSummary{}
	}
	checkError := func(r reply, code int, reason string) {
		t.Helper()
		if r.code != code || r.reason != reason || r.envelope.Code != -1 || strings.Contains(string(r.body), `"items"`) || strings.Contains(string(r.body), `"state"`) || strings.Contains(string(r.body), "PRIVATE") {
			t.Fatal(r.code, string(r.body))
		}
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	run("canonical_default_page_current_user_and_empty_array", func(t *testing.T) {
		p := page(1, nil)
		expected, e := repo.ListConversations(ctx, 1, "", 0)
		if e != nil || !reflect.DeepEqual(p, expected) || !reflect.DeepEqual(ids(p), []string{"g:10", "g:2", "p:1:10", "p:1:2"}) {
			t.Fatal(p, e)
		}
		if item(p, "p:1:2").DeliveredSeq != 2 || item(p, "p:1:10").DeliveredSeq != 0 || item(page(2, nil), "p:1:2").DeliveredSeq != 3 {
			t.Fatal("cross-user ACK mixed")
		}
		empty := page(99, nil)
		if empty.Items == nil || len(empty.Items) != 0 || empty.HasMore || empty.NextCursor != "" {
			t.Fatal(empty)
		}
	})
	run("strict_query_bearer_auth_and_revoked_token", func(t *testing.T) {
		for _, q := range []url.Values{{"userId": {"2"}}, {"limit": {"1", "1"}}, {"cursor": {"", ""}}, {"limit": {"0"}}, {"limit": {"01"}}, {"cursor": {"g:01"}}} {
			checkError(request(s, jwt[1], q), 400, "INVALID_REQUEST")
		}
		checkError(request(s, "", url.Values{"token": {jwt[1]}}), 401, "UNAUTHENTICATED")
		if _, e := tokens.RevokeToken(ctx, jwt[11]); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[11], nil), 401, "UNAUTHENTICATED")
	})
	run("exclusive_pagination_and_periodic_rescan_after_new_ID", func(t *testing.T) {
		first := page(1, url.Values{"limit": {"2"}})
		if !reflect.DeepEqual(ids(first), []string{"g:10", "g:2"}) || first.NextCursor != "g:2" || !first.HasMore {
			t.Fatal(first)
		}
		persist(1, 1, messaging.Group)
		next := page(1, url.Values{"cursor": {first.NextCursor}, "limit": {"2"}})
		if !reflect.DeepEqual(ids(next), []string{"p:1:10", "p:1:2"}) || next.NextCursor != "p:1:2" || next.HasMore {
			t.Fatal(next)
		}
		empty := page(1, url.Values{"cursor": {next.NextCursor}, "limit": {"2"}})
		if len(empty.Items) != 0 || empty.NextCursor != next.NextCursor || empty.HasMore {
			t.Fatal(empty)
		}
		if all := page(1, nil); !reflect.DeepEqual(ids(all), []string{"g:1", "g:10", "g:2", "p:1:10", "p:1:2"}) {
			t.Fatal("new ID behind cursor lost", all)
		}
		if p := page(1, url.Values{"cursor": {"g:20"}}); !reflect.DeepEqual(ids(p), []string{"p:1:10", "p:1:2"}) {
			t.Fatal("unauthorized cursor treated as grant/required row", p)
		}
	})
	run("current_group_permission_old_cursor_and_forbidden_readers", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=1 AND target_id=1 AND type=?", models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=10"); e != nil {
			t.Fatal(e)
		}
		if e := db.WithContext(ctx).Create(&models.Relation{OwnerId: 1, TargetId: 20, Type: models.FriendRelation}).Error; e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=? AND ((owner_id=1 AND target_id=2) OR (owner_id=2 AND target_id=1))", models.FriendRelation); e != nil {
			t.Fatal(e)
		}
		p := page(1, url.Values{"cursor": {"g:1"}})
		if !reflect.DeepEqual(ids(p), []string{"g:2", "p:1:10", "p:1:2"}) {
			t.Fatal(p)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE `user` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=99"); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[99], nil), 403, "FORBIDDEN")
		if _, e := pool.ExecContext(ctx, "UPDATE `user` SET deleted_at=NULL WHERE id=99"); e != nil {
			t.Fatal(e)
		}
		missing, e := tokens.GenerateToken(100)
		if e != nil {
			t.Fatal(e)
		}
		checkError(request(s, missing, nil), 403, "FORBIDDEN")
	})
	run("corrupt_SQL_cursor_returns_503_without_partial_page", func(t *testing.T) {
		if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=4 WHERE user_id=1 AND conversation_id='p:1:2'"); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if _, e := pool.ExecContext(ctx, "UPDATE message_delivery_cursor SET delivered_seq=2 WHERE user_id=1 AND conversation_id='p:1:2'"); e != nil {
				t.Error(e)
			}
		}()
		checkError(request(s, jwt[1], nil), 503, "DEPENDENCY_UNAVAILABLE")
	})
	run("SQL_pool_wait_uses_production_deadline_then_recovers", func(t *testing.T) {
		s.mu.Lock()
		owned := s.resources.sql.(*sql.DB)
		s.mu.Unlock()
		held := make([]*sql.Conn, 0, 4)
		defer func() {
			for _, c := range held {
				c.Close()
			}
		}()
		for i := 0; i < 4; i++ {
			c, e := owned.Conn(ctx)
			if e != nil {
				t.Fatal(e)
			}
			held = append(held, c)
		}
		started := time.Now()
		checkError(request(s, jwt[1], nil), 503, "DEPENDENCY_UNAVAILABLE")
		elapsed := time.Since(started)
		if elapsed < 2500*time.Millisecond || elapsed > 5*time.Second {
			t.Fatal("unexpected production read deadline", elapsed)
		}
		for _, c := range held {
			c.Close()
		}
		held = nil
		if p := page(1, nil); len(p.Items) != 3 {
			t.Fatal("pool did not recover", p)
		}
	})
	run("real_auth_client_failure_and_fresh_API_recovery", func(t *testing.T) {
		s.mu.Lock()
		owned := s.resources.redis
		s.mu.Unlock()
		if e := owned.Close(); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[1], nil), 503, "DEPENDENCY_UNAVAILABLE")
		if e := s.Close(ctx); e != nil && !errors.Is(e, redis.ErrClosed) {
			t.Fatal(e)
		}
		apiWait(t, s.Done())
		s = start(base)
		if p := page(1, nil); len(p.Items) != 3 {
			t.Fatal(p)
		}
	})
	run("MySQL_offline_startup_is_503_and_restart_recovers", func(t *testing.T) {
		if e := env.SetMySQLRunning(ctx, false); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[1], nil), 503, "DEPENDENCY_UNAVAILABLE")
		checkError(request(s, jwt[99], nil), 503, "DEPENDENCY_UNAVAILABLE")
		if e := s.Close(ctx); e != nil {
			t.Fatal(e)
		}
		s = start(base)
		if apiRealStatus(apiAddress(s), "/health/live") != 200 {
			t.Fatal("offline SQL prevented HTTP liveness")
		}
		checkError(request(s, jwt[1], nil), 503, "DEPENDENCY_UNAVAILABLE")
		if e := env.SetMySQLRunning(ctx, true); e != nil {
			t.Fatal(e)
		}
		fresh, e := env.Config()
		if e != nil {
			t.Fatal(e)
		}
		changed := fresh.MYSQL.Host != base.MYSQL.Host || fresh.MYSQL.Port != base.MYSQL.Port
		base.MYSQL = fresh.MYSQL
		if changed {
			if e := s.Close(ctx); e != nil {
				t.Fatal(e)
			}
			s = start(base)
			t.Log("Docker mapping changed; explicitly rebuilt API pool")
		} else {
			t.Log("MySQL recovered on the same mapped address")
		}
		deadline := time.Now().Add(20 * time.Second)
		var recovered bool
		for time.Now().Before(deadline) {
			r := request(s, jwt[1], nil)
			if r.code == 200 {
				if !reflect.DeepEqual(ids(r.envelope.Data), []string{"g:2", "p:1:10", "p:1:2"}) {
					t.Fatal(r.envelope.Data)
				}
				recovered = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !recovered {
			t.Fatal("HTTP list did not recover")
		}
		if p := page(99, nil); len(p.Items) != 0 || p.Items == nil {
			t.Fatal(p)
		}
	})
	// Use refreshed connection settings to validate collections even if Docker
	// assigned a different mapped port at restart; no fixed-address assumption.
	finalDB, e := utils.OpenMySQL(base)
	if e != nil {
		t.Fatal(e)
	}
	finalPool, e := finalDB.DB()
	if e != nil {
		t.Fatal(e)
	}
	defer finalPool.Close()
	finalRepo, _ := database.NewMessageRepository(finalPool)
	finalPage := page(1, nil)
	canonicalPage, e := finalRepo.ListConversations(ctx, 1, "", 500)
	wantItems := []messaging.ConversationSummary{{ConversationID: "g:2", LastSeq: 1, DeliveredSeq: 1}, {ConversationID: "p:1:10", LastSeq: 1, DeliveredSeq: 0}, {ConversationID: "p:1:2", LastSeq: 3, DeliveredSeq: 2}}
	if e != nil || !reflect.DeepEqual(finalPage, canonicalPage) || !reflect.DeepEqual(finalPage.Items, wantItems) || item(page(2, nil), "p:1:2").DeliveredSeq != 3 {
		t.Fatal("recovered HTTP metadata/ACK changed", finalPage, e)
	}
	for _, c := range commands {
		sender, _ := strconv.ParseUint(c.SenderID, 10, 64)
		got, e := finalRepo.GetCommandStatus(ctx, sender, c.ClientMessageID, c.PayloadHash)
		if e != nil || !reflect.DeepEqual(got, original[c.CommandID]) {
			t.Fatal("HTTP read changed terminal/message", c.CommandID, e)
		}
	}
	for table, want := range map[string]int{"message_command": 9, "message": 9, "conversation": 7, "message_outbox": 18, "message_delivery_cursor": 3, "message_quarantine": 0} {
		var n int
		if e := finalPool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil || n != want {
			t.Fatal("wrong final collection", table, n, e)
		}
	}
	var untouched int
	if e := finalPool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0 AND lease_token IS NULL").Scan(&untouched); e != nil || untouched != 18 {
		t.Fatal(untouched, e)
	}
	if e := migrations.ApplyReliable(ctx, finalPool); e != nil {
		t.Fatal("schema changed", e)
	}
	run("API_close_joins_and_closes_owned_clients_only", func(t *testing.T) {
		s.mu.Lock()
		ownedSQL, ownedRedis := s.resources.sql, s.resources.redis
		s.mu.Unlock()
		if e := s.Close(ctx); e != nil {
			t.Fatal(e)
		}
		apiWait(t, s.Done())
		if e := ownedSQL.PingContext(ctx); e == nil {
			t.Fatal("owned SQL pool remained open")
		}
		if e := ownedRedis.Get(ctx, "closed-client").Err(); !errors.Is(e, redis.ErrClosed) {
			t.Fatal("owned Redis client remained open", e)
		}
		if e := finalPool.PingContext(ctx); e != nil {
			t.Fatal("API closed external fixture pool", e)
		}
	})
}
