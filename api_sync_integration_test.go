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

// Uses the default API factories and real JWT/Redis/MySQL. Business commands
// enter PersistCommand directly; the MQ capability is deliberately unused.
func TestProductionMessageSyncIntegration(t *testing.T) {
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
	t.Logf("production message sync fixture run=%s", env.RunID())
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
		t.Fatal("refusing fixture writes outside owned schema")
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
		users[i] = models.User{Identifier: v, Phone: v, Name: "sync-http-fixture"}
	}
	if e := db.WithContext(ctx).Create(&users).Error; e != nil {
		t.Fatal(e)
	}
	groups := []models.Group{{Name: "sync-group", OwnerId: users[0].ID, MaxUser: 50}, {Name: "deleted-sync-group", OwnerId: users[0].ID, MaxUser: 50}}
	if e := db.WithContext(ctx).Create(&groups).Error; e != nil {
		t.Fatal(e)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: groups[0].ID, Type: models.GroupRelation}, {OwnerId: users[0].ID, TargetId: groups[1].ID, Type: models.GroupRelation}}
	if e := db.WithContext(ctx).Create(&relations).Error; e != nil {
		t.Fatal(e)
	}
	repo, _ := database.NewMessageRepository(pool)
	var commands []messaging.Command
	original := map[string]messaging.CommandStatus{}
	persist := func(kind messaging.TargetType, target uint, media messaging.Media) messaging.Command {
		t.Helper()
		c, e := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("71ee6c10-1234-4234-8234-%012d", len(commands)+1), TargetType: kind, TargetID: strconv.FormatUint(uint64(target), 10), Media: media, Content: fmt.Sprintf("PRIVATE_SYNC_%d", len(commands))})
		if e != nil {
			t.Fatal(e)
		}
		if _, e := repo.PersistCommand(ctx, c); e != nil {
			t.Fatal(e)
		}
		commands = append(commands, c)
		s, e := repo.GetCommandStatus(ctx, uint64(users[0].ID), c.ClientMessageID, c.PayloadHash)
		if e != nil || s.State != messaging.StatusPersisted {
			t.Fatal(e)
		}
		original[c.CommandID] = s
		return c
	}
	var privateCommands []messaging.Command
	for _, media := range []messaging.Media{messaging.Text, messaging.Picture, messaging.Audio} {
		privateCommands = append(privateCommands, persist(messaging.Private, users[1].ID, media))
	}
	persist(messaging.Group, groups[0].ID, messaging.Text)
	persist(messaging.Group, groups[0].ID, messaging.Picture)
	persist(messaging.Group, groups[1].ID, messaging.Audio)
	privateID := privateCommands[0].ConversationID
	groupID := fmt.Sprintf("g:%d", groups[0].ID)
	deletedGroupID := fmt.Sprintf("g:%d", groups[1].ID)
	emptyID := fmt.Sprintf("p:%d:%d", users[0].ID, users[2].ID)
	for _, a := range []struct {
		u   uint
		seq messaging.Sequence
	}{{users[0].ID, 1}, {users[1].ID, 3}} {
		if _, e := repo.AcknowledgeDelivery(ctx, uint64(a.u), privateID, a.seq); e != nil {
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
	jwt := make([]string, 3)
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
		data   messaging.SyncResponse
		reason string
	}
	request := func(s *apiService, token string, q url.Values) reply {
		t.Helper()
		req, e := http.NewRequestWithContext(ctx, "GET", "http://"+apiAddress(s)+"/auth/messages/sync?"+q.Encode(), nil)
		if e != nil {
			t.Fatal(e)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, e := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		body, e := io.ReadAll(io.LimitReader(resp.Body, 8*1024*1024+1))
		if e != nil || len(body) > 8*1024*1024 {
			t.Fatal("invalid sync response", e)
		}
		if resp.Header.Get("Cache-Control") != "no-store" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			t.Fatal("wrong sync headers")
		}
		var data messaging.SyncResponse
		var failure struct {
			Data struct {
				Reason string `json:"reasonCode"`
			} `json:"data"`
		}
		if e := json.Unmarshal(body, &data); e != nil {
			t.Fatal(e)
		}
		json.Unmarshal(body, &failure)
		return reply{resp.StatusCode, body, data, failure.Data.Reason}
	}
	query := func(id, after string, limit int) url.Values {
		q := url.Values{"conversationId": {id}}
		if after != "" {
			q.Set("afterSeq", after)
		}
		if limit > 0 {
			q.Set("limit", strconv.Itoa(limit))
		}
		return q
	}
	page := func(reader int, id, after string, n int) messaging.SyncPage {
		t.Helper()
		r := request(s, jwt[reader], query(id, after, n))
		if r.code != 200 || r.data.Code != 0 {
			t.Fatal(r.code, string(r.body))
		}
		return r.data.Data
	}
	checkError := func(r reply, code int, reason string) {
		t.Helper()
		if r.code != code || r.data.Code != -1 || r.reason != reason || strings.Contains(string(r.body), "PRIVATE") || strings.Contains(string(r.body), `"items"`) || strings.Contains(string(r.body), `"state"`) {
			t.Fatal(r.code, string(r.body))
		}
	}
	run := func(name string, fn func(*testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}
	run("canonical_media_and_local_zero_ignores_server_ACK", func(t *testing.T) {
		p := page(1, privateID, "", 0)
		canonical, e := repo.SyncMessages(ctx, uint64(users[1].ID), privateID, "", 0)
		if e != nil || !reflect.DeepEqual(p, canonical) || len(p.Items) != 3 || p.AfterSeq != 0 || p.NextAfterSeq != 3 || p.HighWatermark != 3 || p.HasMore {
			t.Fatal(p, e)
		}
		for i, m := range p.Items {
			if !reflect.DeepEqual(m, *original[privateCommands[i].CommandID].Message) {
				t.Fatal("canonical SQL DTO changed", m.MessageID)
			}
		}
		empty := page(0, emptyID, "0", 0)
		if empty.Items == nil || len(empty.Items) != 0 || empty.HighWatermark != 0 || empty.NextAfterSeq != 0 || empty.HasMore {
			t.Fatal(empty)
		}
		checkError(request(s, jwt[0], query(emptyID, "1", 0)), 400, "INVALID_REQUEST")
	})
	run("fresh_snapshot_each_page_includes_new_commit", func(t *testing.T) {
		first := page(1, privateID, "0", 2)
		if len(first.Items) != 2 || first.NextAfterSeq != 2 || first.HighWatermark != 3 || !first.HasMore {
			t.Fatal(first)
		}
		privateCommands = append(privateCommands, persist(messaging.Private, users[1].ID, messaging.Text))
		next := page(1, privateID, first.NextAfterSeq.String(), 2)
		if len(next.Items) != 2 || next.Items[0].Seq != 3 || next.Items[1].Seq != 4 || next.HighWatermark != 4 || next.NextAfterSeq != 4 || next.HasMore {
			t.Fatal(next)
		}
		empty := page(1, privateID, "4", 2)
		if empty.Items == nil || len(empty.Items) != 0 || empty.NextAfterSeq != 4 || empty.HighWatermark != 4 || empty.HasMore {
			t.Fatal(empty)
		}
		checkError(request(s, jwt[1], query(privateID, "5", 0)), 400, "INVALID_REQUEST")
	})
	run("current_group_permission_and_private_participant_scope", func(t *testing.T) {
		checkError(request(s, jwt[1], query(groupID, "0", 0)), 403, "FORBIDDEN")
		if e := db.WithContext(ctx).Create(&models.Relation{OwnerId: users[1].ID, TargetId: groups[0].ID, Type: models.GroupRelation}).Error; e != nil {
			t.Fatal(e)
		}
		joined := page(1, groupID, "0", 0)
		if len(joined.Items) != 2 || joined.Items[0].Seq != 1 || joined.Items[1].Seq != 2 {
			t.Fatal("new member could not recover pre-join history", joined)
		}
		if _, e := repo.AcknowledgeDelivery(ctx, uint64(users[1].ID), groupID, 2); e != nil {
			t.Fatal(e)
		}
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE owner_id=? AND target_id=? AND type=?", users[1].ID, groups[0].ID, models.GroupRelation); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[1], query(groupID, "2", 0)), 403, "FORBIDDEN")
		if _, e := pool.ExecContext(ctx, "UPDATE `group` SET deleted_at=UTC_TIMESTAMP(6) WHERE id=?", groups[1].ID); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[0], query(deletedGroupID, "0", 0)), 403, "FORBIDDEN")
		checkError(request(s, jwt[2], query(privateID, "0", 0)), 403, "FORBIDDEN")
		if _, e := pool.ExecContext(ctx, "UPDATE relation SET deleted_at=UTC_TIMESTAMP(6) WHERE type=?", models.FriendRelation); e != nil {
			t.Fatal(e)
		}
		if p := page(1, privateID, "0", 0); len(p.Items) != 4 || p.NextAfterSeq != 4 {
			t.Fatal("friendship change revoked existing private history", p)
		}
	})
	run("lookahead_corruption_and_sequence_hole_never_emit_partial_content", func(t *testing.T) {
		third := privateCommands[2]
		if _, e := pool.ExecContext(ctx, "UPDATE message SET content='PRIVATE_CORRUPTION' WHERE id=?", third.CommandID); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if _, e := pool.ExecContext(ctx, "UPDATE message SET content=? WHERE id=?", third.Content, third.CommandID); e != nil {
				t.Error(e)
			}
		}()
		checkError(request(s, jwt[1], query(privateID, "0", 2)), 503, "DEPENDENCY_UNAVAILABLE")
		if _, e := pool.ExecContext(ctx, "UPDATE message SET content=? WHERE id=?", third.Content, third.CommandID); e != nil {
			t.Fatal(e)
		}
		second := privateCommands[1]
		if _, e := pool.ExecContext(ctx, "UPDATE message SET seq=100 WHERE id=?", second.CommandID); e != nil {
			t.Fatal(e)
		}
		defer func() {
			if _, e := pool.ExecContext(ctx, "UPDATE message SET seq=2 WHERE id=?", second.CommandID); e != nil {
				t.Error(e)
			}
		}()
		checkError(request(s, jwt[1], query(privateID, "0", 2)), 503, "DEPENDENCY_UNAVAILABLE")
		if _, e := pool.ExecContext(ctx, "UPDATE message SET seq=2 WHERE id=?", second.CommandID); e != nil {
			t.Fatal(e)
		}
		if p := page(1, privateID, "0", 0); len(p.Items) != 4 {
			t.Fatal(p)
		}
	})
	run("strict_query_and_real_token_revocation", func(t *testing.T) {
		for _, q := range []url.Values{{"conversationId": {privateID}, "afterSeq": {"0", "0"}}, {"conversationId": {privateID}, "afterSeq": {""}}, {"conversationId": {privateID}, "afterSeq": {"01"}}, {"conversationId": {privateID}, "beforeSeq": {"1"}}, {"conversationId": {privateID}, "userId": {fmt.Sprint(users[2].ID)}}, {"conversationId": {privateID}, "limit": {"0"}}} {
			checkError(request(s, jwt[1], q), 400, "INVALID_REQUEST")
		}
		q := query(privateID, "0", 0)
		q.Set("token", jwt[0])
		checkError(request(s, "", q), 401, "UNAUTHENTICATED")
		if _, e := tokens.RevokeToken(ctx, jwt[2]); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[2], query(privateID, "0", 0)), 401, "UNAUTHENTICATED")
	})
	run("SQL_wait_uses_production_deadline_and_releases_request", func(t *testing.T) {
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
		checkError(request(s, jwt[1], query(privateID, "0", 0)), 503, "DEPENDENCY_UNAVAILABLE")
		if elapsed := time.Since(started); elapsed < 2500*time.Millisecond || elapsed > 5*time.Second {
			t.Fatal("wrong production sync deadline", elapsed)
		}
		for _, c := range held {
			c.Close()
		}
		held = nil
		if p := page(1, privateID, "0", 0); len(p.Items) != 4 {
			t.Fatal(p)
		}
	})
	run("auth_client_failure_and_new_API_recovery", func(t *testing.T) {
		s.mu.Lock()
		owned := s.resources.redis
		s.mu.Unlock()
		if e := owned.Close(); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[1], query(privateID, "0", 0)), 503, "DEPENDENCY_UNAVAILABLE")
		if e := s.Close(ctx); e != nil && !errors.Is(e, redis.ErrClosed) {
			t.Fatal(e)
		}
		apiWait(t, s.Done())
		s = start(base)
		if p := page(1, privateID, "0", 0); len(p.Items) != 4 {
			t.Fatal(p)
		}
	})
	run("MySQL_offline_existing_and_new_API_do_not_fake_empty", func(t *testing.T) {
		if e := env.SetMySQLRunning(ctx, false); e != nil {
			t.Fatal(e)
		}
		checkError(request(s, jwt[1], query(privateID, "0", 0)), 503, "DEPENDENCY_UNAVAILABLE")
		checkError(request(s, jwt[0], query(emptyID, "0", 0)), 503, "DEPENDENCY_UNAVAILABLE")
		if e := s.Close(ctx); e != nil {
			t.Fatal(e)
		}
		s = start(base)
		if apiRealStatus(apiAddress(s), "/health/live") != 200 {
			t.Fatal("offline SQL blocked liveness")
		}
		checkError(request(s, jwt[1], query(privateID, "0", 0)), 503, "DEPENDENCY_UNAVAILABLE")
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
			t.Log("Docker changed mapped address; explicitly rebuilt API pool")
		} else {
			t.Log("MySQL restarted at the same mapped address")
		}
		deadline := time.Now().Add(20 * time.Second)
		var recovered bool
		for time.Now().Before(deadline) {
			r := request(s, jwt[1], query(privateID, "0", 0))
			if r.code == 200 {
				if len(r.data.Data.Items) != 4 || r.data.Data.NextAfterSeq != 4 {
					t.Fatal(r.data.Data)
				}
				recovered = true
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !recovered {
			t.Fatal("sync failed to recover")
		}
		if p := page(0, emptyID, "0", 0); p.Items == nil || len(p.Items) != 0 || p.HighWatermark != 0 {
			t.Fatal(p)
		}
	})
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
	canonical, e := finalRepo.SyncMessages(ctx, uint64(users[1].ID), privateID, "0", 500)
	if e != nil || !reflect.DeepEqual(page(1, privateID, "0", 500), canonical) || len(canonical.Items) != 4 {
		t.Fatal("recovered DTO differs from canonical SQL", e)
	}
	for _, c := range commands {
		got, e := finalRepo.GetCommandStatus(ctx, uint64(users[0].ID), c.ClientMessageID, c.PayloadHash)
		if e != nil || !reflect.DeepEqual(got, original[c.CommandID]) {
			t.Fatal("sync changed terminal/message", c.CommandID, e)
		}
	}
	for table, want := range map[string]int{"message_command": 7, "message": 7, "conversation": 3, "message_outbox": 14, "message_delivery_cursor": 3, "message_quarantine": 0} {
		var n int
		if e := finalPool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); e != nil || n != want {
			t.Fatal("wrong final collection", table, n, e)
		}
	}
	var untouched int
	if e := finalPool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND attempts=0 AND lease_token IS NULL").Scan(&untouched); e != nil || untouched != 14 {
		t.Fatal(untouched, e)
	}
	for _, a := range []struct {
		u   uint
		id  string
		seq uint64
	}{{users[0].ID, privateID, 1}, {users[1].ID, privateID, 3}, {users[1].ID, groupID, 2}} {
		var n uint64
		if e := finalPool.QueryRowContext(ctx, "SELECT delivered_seq FROM message_delivery_cursor WHERE user_id=? AND conversation_id=?", a.u, a.id).Scan(&n); e != nil || n != a.seq {
			t.Fatal("sync modified server ACK", n, e)
		}
	}
	if e := migrations.ApplyReliable(ctx, finalPool); e != nil {
		t.Fatal("schema changed", e)
	}
	run("API_close_joins_before_closing_owned_clients", func(t *testing.T) {
		s.mu.Lock()
		ownedSQL, ownedRedis := s.resources.sql, s.resources.redis
		s.mu.Unlock()
		if e := s.Close(ctx); e != nil {
			t.Fatal(e)
		}
		apiWait(t, s.Done())
		if e := ownedSQL.PingContext(ctx); e == nil {
			t.Fatal("owned SQL pool open")
		}
		if e := ownedRedis.Get(ctx, "closed-sync-owner").Err(); !errors.Is(e, redis.ErrClosed) {
			t.Fatal("owned Redis open", e)
		}
		if e := finalPool.PingContext(ctx); e != nil {
			t.Fatal("external SQL pool closed", e)
		}
	})
}
