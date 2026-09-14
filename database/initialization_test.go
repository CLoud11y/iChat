package database

import (
	"iChat/config"
	"iChat/models"
	"iChat/utils"
	"testing"

	"github.com/go-redis/redis/v8"
	"gorm.io/gorm"
)

func TestImportDoesNotInitializeDependencies(t *testing.T) {
	if utils.DB != nil || utils.RDS != nil || Umanager != nil || Rmanager != nil || Gmanager != nil || Mmanager != nil {
		t.Fatal("package import initialized external dependencies or managers")
	}
	if config.Conf != (config.Config{}) {
		t.Fatal("package import loaded configuration")
	}
}

func TestManagerConstructorsUseSuppliedDependencies(t *testing.T) {
	db := &gorm.DB{}
	rds := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MinIdleConns: 0})
	t.Cleanup(func() { _ = rds.Close() })
	um := NewUserManager(db)
	rm := NewRelationManager(db)
	gm := NewGroupManager(db)
	mm := NewMessageManager(db, rds)
	if um.db != db || rm.db != db || gm.db != db || mm.db != db || mm.rds != rds {
		t.Fatal("manager constructor did not preserve the supplied dependencies")
	}
	user := &models.User{Model: gorm.Model{ID: 1}}
	um.Online(user)
	if um.onlineMap[1] != user {
		t.Fatal("user manager did not initialize its online user map")
	}
	um.Offline(1)
	if len(um.onlineMap) != 0 {
		t.Fatal("Offline did not remove the user")
	}
	if NewUserManager(db).onlineMap == nil {
		t.Fatal("each user manager needs its own initialized map")
	}
}
