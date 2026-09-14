//go:build integration

package testenv

import (
	"context"
	"fmt"
	"iChat/config"
	"iChat/models"
	"iChat/utils"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

// LegacyFixture owns a fresh environment for one test/benchmark invocation.
// Users[0] and Users[1] are friends; Users[4] has no relations.
// Groups[0] contains Users[0:2], Groups[1] contains Users[0] and Users[2],
// and Groups[2] contains only Users[3]. IDs always come from MySQL inserts.
type LegacyFixture struct {
	Env    *Environment
	DB     *gorm.DB
	Redis  *redis.Client
	Users  []models.User
	Groups []models.Group
	nextID uint
}

func NewLegacyFixture(tb testing.TB) *LegacyFixture {
	tb.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		tb.Fatal(err)
	}
	root := config.FindProjectRoot(workingDirectory)
	if root == "" {
		tb.Fatal("cannot locate repository test Compose file")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := Start(ctx, filepath.Join(root, "docker-compose.test.yml"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Logf("legacy fixture run=%s", env.RunID())
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := env.Close(ctx); err != nil {
			tb.Errorf("cleanup run %s: %v", env.RunID(), err)
		}
	})
	conf, err := env.Config()
	if err != nil {
		tb.Fatal(err)
	}
	db, err := utils.OpenMySQL(conf)
	if err != nil {
		tb.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := pool.Close(); err != nil {
			tb.Error(err)
		}
	})
	rds, err := utils.OpenRedis(ctx, conf)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := rds.Close(); err != nil {
			tb.Error(err)
		}
	})
	var schema string
	if err := db.WithContext(ctx).Raw("SELECT DATABASE()").Scan(&schema).Error; err != nil {
		tb.Fatal(err)
	}
	if schema != "ichat_test_"+env.RunID() {
		tb.Fatalf("refusing fixture writes in schema %q", schema)
	}
	if err := utils.MigrateLegacy(db.WithContext(ctx)); err != nil {
		tb.Fatal(err)
	}
	f := &LegacyFixture{Env: env, DB: db, Redis: rds}
	for i, name := range []string{"alice", "bob", "carol", "dave", "unrelated"} {
		identity := fmt.Sprintf("%s-%d", env.RunID()[:24], i)
		f.Users = append(f.Users, models.User{Identifier: identity, Phone: identity, Name: name, Password: "fixture-only"})
	}
	if err := db.WithContext(ctx).Create(&f.Users).Error; err != nil {
		tb.Fatal(err)
	}
	f.Groups = []models.Group{
		{Name: "shared", OwnerId: f.Users[0].ID, MaxUser: 50},
		{Name: "second", OwnerId: f.Users[0].ID, MaxUser: 50},
		{Name: "unrelated", OwnerId: f.Users[3].ID, MaxUser: 50},
	}
	if err := db.WithContext(ctx).Create(&f.Groups).Error; err != nil {
		tb.Fatal(err)
	}
	relations := []models.Relation{
		{OwnerId: f.Users[0].ID, TargetId: f.Users[1].ID, Type: models.FriendRelation},
		{OwnerId: f.Users[1].ID, TargetId: f.Users[0].ID, Type: models.FriendRelation},
		{OwnerId: f.Users[2].ID, TargetId: f.Users[3].ID, Type: models.FriendRelation},
		{OwnerId: f.Users[3].ID, TargetId: f.Users[2].ID, Type: models.FriendRelation},
		{OwnerId: f.Users[0].ID, TargetId: f.Groups[0].ID, Type: models.GroupRelation},
		{OwnerId: f.Users[1].ID, TargetId: f.Groups[0].ID, Type: models.GroupRelation},
		{OwnerId: f.Users[0].ID, TargetId: f.Groups[1].ID, Type: models.GroupRelation},
		{OwnerId: f.Users[2].ID, TargetId: f.Groups[1].ID, Type: models.GroupRelation},
		{OwnerId: f.Users[3].ID, TargetId: f.Groups[2].ID, Type: models.GroupRelation},
	}
	if err := db.WithContext(ctx).Create(&relations).Error; err != nil {
		tb.Fatal(err)
	}
	return f
}

// Message creates an unsaved legacy message with a nonempty, per-run unique ID.
// Pairs share timestamps so pagination also exercises the identifier tie-break.
func (f *LegacyFixture) Message(senderID, receiverID, kind, media uint, content string) models.Message {
	f.nextID++
	return models.Message{
		Id: fmt.Sprintf("%s-%d", f.Env.RunID(), f.nextID), Identifier: f.nextID,
		TimeStamp: 1700000000000 + int64((f.nextID-1)/2), SenderId: senderID,
		ReceiverId: receiverID, Type: kind, Media: media, Content: content,
	}
}
