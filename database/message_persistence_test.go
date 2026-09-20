package database

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestLegacyMessageTransactionFailures(t *testing.T) {
	for _, tc := range []struct {
		stage  string
		events []string
	}{
		{"success", []string{"begin", "write", "write", "commit"}},
		{"begin", []string{"begin"}},
		{"write", []string{"begin", "write", "write", "rollback"}},
		{"commit", []string{"begin", "write", "write", "commit", "rollback"}},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			pool := &groupTransactionPool{stage: tc.stage, cause: errors.New("injected persistence failure")}
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			err = NewMessageManager(db, nil).SaveMsgs2DB(context.Background(), []string{`{"id":"one"}`, `{"id":"two"}`})
			if tc.stage == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, pool.cause) || !errors.Is(err, ErrStorageUnavailable) {
				t.Fatalf("error=%v", err)
			}
			if !reflect.DeepEqual(pool.events, tc.events) {
				t.Fatalf("events=%v want=%v", pool.events, tc.events)
			}
		})
	}
}
