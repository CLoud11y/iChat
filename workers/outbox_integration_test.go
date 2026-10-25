//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
	"iChat/migrations"
	"iChat/models"
	"iChat/tests/testenv"
	"iChat/utils"
)

func TestOutboxLoopIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	env, err := testenv.Start(ctx, filepath.Join("..", "docker-compose.test.yml"))
	if env != nil {
		t.Cleanup(func() {
			c, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := env.Close(c); err != nil {
				t.Error(err)
			}
		})
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("outbox execution fixture run=%s", env.RunID())
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
		t.Fatal("refusing writes outside owned schema")
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).AutoMigrate(&models.User{}, &models.Group{}, &models.Relation{}); err != nil {
		t.Fatal(err)
	}
	users := make([]models.User, 3)
	for i := range users {
		v := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		users[i] = models.User{Identifier: v, Phone: v, Name: "outbox-fixture"}
	}
	if err := db.WithContext(ctx).Create(&users).Error; err != nil {
		t.Fatal(err)
	}
	group := models.Group{Name: "outbox-fixture", OwnerId: users[0].ID, MaxUser: 50}
	if err := db.WithContext(ctx).Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	relations := []models.Relation{{OwnerId: users[0].ID, TargetId: users[1].ID, Type: models.FriendRelation}, {OwnerId: users[1].ID, TargetId: users[0].ID, Type: models.FriendRelation}, {OwnerId: users[0].ID, TargetId: group.ID, Type: models.GroupRelation}}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		t.Fatal(err)
	}
	messages, _ := database.NewMessageRepository(pool)
	serial := 0
	seed := func(kind messaging.TargetType, target uint) {
		t.Helper()
		serial++
		c, err := messaging.Normalize(uint64(users[0].ID), messaging.SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", serial), TargetType: kind, TargetID: strconv.FormatUint(uint64(target), 10), Media: messaging.Text, Content: "outbox-private-secret"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := messages.PersistCommand(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	seed(messaging.Private, users[1].ID)
	seed(messaging.Group, group.ID)
	seed(messaging.Private, users[2].ID)
	rds, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rds.Close(); err != nil {
			t.Error(err)
		}
	})
	publisher, _ := NewRedisNotificationPublisher(rds)
	channels := []string{"ichat:notify:v1:user:" + strconv.FormatUint(uint64(users[0].ID), 10), "ichat:notify:v1:user:" + strconv.FormatUint(uint64(users[1].ID), 10), "ichat:notify:v1:group:" + strconv.FormatUint(uint64(group.ID), 10)}
	sub := rds.Subscribe(ctx, channels...)
	defer sub.Close()
	for range channels {
		if _, err := sub.Receive(ctx); err != nil {
			t.Fatal(err)
		}
	}
	outbox, _ := database.NewOutboxRepository(pool)
	o := OutboxOptions{Claim: database.OutboxClaimOptions{Limit: 10, Lease: 30 * time.Second}, Retry: database.OutboxRetryOptions{Base: 10 * time.Millisecond, Max: time.Second}, ClaimTimeout: 3 * time.Second, PublishTimeout: 2 * time.Second, SettlementTimeout: 3 * time.Second, PollInterval: 5 * time.Millisecond}
	// With one SQL connection, this query can only succeed after the claim
	// transaction has released the connection; Redis runs while row is leased.
	pool.SetMaxOpenConns(1)
	attempts := map[string]int{}
	unknown := false
	checkedPublisher := notificationPublisherFunc(func(c context.Context, n messaging.Notification) error {
		if pool.Stats().InUse != 0 {
			t.Fatal("network called while SQL connection held")
		}
		var hint struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal(n.Body(), &hint); err != nil {
			t.Fatal(err)
		}
		var status string
		if err := pool.QueryRowContext(c, "SELECT status FROM message_outbox WHERE event_id=?", hint.EventID).Scan(&status); err != nil || status != "leased" {
			t.Fatal("claim was not committed before network", err)
		}
		attempts[hint.EventID]++
		if err := publisher.Publish(c, n); err != nil {
			return err
		}
		if unknown {
			return errors.New("injected result unknown after successful publish")
		}
		return nil
	})
	w, err := NewOutboxLoop(outbox, checkedPublisher, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("committed_payloads_route_without_content", func(t *testing.T) {
		r, err := w.RunBatch(ctx)
		if err != nil || r.Claimed != 5 || r.Completed != 5 || r.Published != 5 || r.Deferred != 0 {
			t.Fatal(r, err)
		}
		seen := map[string]bool{}
		routes := map[string]int{}
		for range 5 {
			c, stop := context.WithTimeout(ctx, 2*time.Second)
			m, err := sub.ReceiveMessage(c)
			stop()
			if err != nil {
				t.Fatal(err)
			}
			var h struct {
				EventID   string `json:"eventId"`
				EventType string `json:"eventType"`
				State     string `json:"state"`
			}
			if err := json.Unmarshal([]byte(m.Payload), &h); err != nil || seen[h.EventID] || h.EventID == "" {
				t.Fatal("duplicate/invalid notification", err)
			}
			seen[h.EventID] = true
			routes[m.Channel]++
			if strings.Contains(m.Payload, "outbox-private-secret") || strings.Contains(m.Payload, "content") || strings.Contains(m.Payload, "media") {
				t.Fatal("content broadcast")
			}
			if h.EventType == "command.result" && m.Channel != channels[0] {
				t.Fatal("result not sender scoped")
			}
		}
		if routes[channels[0]] != 3 || routes[channels[1]] != 1 || routes[channels[2]] != 1 {
			t.Fatal("wrong route counts", routes)
		}
		if err := sub.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("zero_subscribers_and_unknown_retry", func(t *testing.T) {
		seed(messaging.Private, users[1].ID)
		unknown = true
		r, err := w.RunBatch(ctx)
		if err != nil || r.Retried != 2 || r.Completed != 0 || r.Published != 0 {
			t.Fatal(r, err)
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='pending' AND last_error='NOTIFICATION_UNKNOWN' AND lease_token IS NULL AND attempts=1").Scan(&count); err != nil || count != 2 {
			t.Fatal("unknown publication lost", err, count)
		}
		unknown = false
		completed := 0
		deadline := time.Now().Add(3 * time.Second)
		for completed < 2 && time.Now().Before(deadline) {
			r, err := w.RunBatch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			completed += r.Completed
			if completed < 2 {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if completed != 2 {
			t.Fatal("retry not reclaimed")
		}
		repeated := 0
		for _, n := range attempts {
			if n == 2 {
				repeated++
			} else if n != 1 {
				t.Fatal("unexpected publish count")
			}
		}
		if repeated != 2 {
			t.Fatal("event identity did not survive retry")
		}
	})
	t.Run("semantic_corruption_retries_without_publish", func(t *testing.T) {
		command := strings.Repeat("a", 64)
		id, _ := messaging.OutboxEventID(command, messaging.EventCommandResult)
		if _, err := pool.ExecContext(ctx, "INSERT INTO message_outbox(event_id,event_type,command_id,payload,status,available_at,created_at) VALUES (?,'command.result',?,'{}','pending',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))", id, command); err != nil {
			t.Fatal(err)
		}
		r, err := w.RunBatch(ctx)
		if err != nil || r.Retried != 1 || r.Published != 0 || len(attempts) != 7 {
			t.Fatal(r, err)
		}
		var reason, status string
		if err := pool.QueryRowContext(ctx, "SELECT status,last_error FROM message_outbox WHERE event_id=?", id).Scan(&status, &reason); err != nil || status != "pending" || reason != "INVALID_NOTIFICATION" {
			t.Fatal(status, reason, err)
		}
	})
	for _, tc := range []struct {
		table string
		want  int
	}{{"message", 3}, {"message_command", 4}, {"conversation", 2}, {"message_outbox", 8}, {"message_delivery_cursor", 0}, {"message_quarantine", 0}} {
		var n int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+tc.table).Scan(&n); err != nil || n != tc.want {
			t.Fatal("business set changed", tc.table, n, err)
		}
	}
	var complete int
	if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM message_outbox WHERE status='completed' AND lease_token IS NULL AND completed_at IS NOT NULL").Scan(&complete); err != nil || complete != 7 {
		t.Fatal(complete, err)
	}
	if err := migrations.ApplyReliable(ctx, pool); err != nil {
		t.Fatal("schema drift", err)
	}
}
