package database

import (
	"context"
	"errors"
	"iChat/models"
	"reflect"
	"testing"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestQuarantineFailureClassification(t *testing.T) {
	for _, number := range []uint16{1264, 1366, 1406, 3819} {
		if quarantineReason(&mysqlDriver.MySQLError{Number: number}) == "" {
			t.Fatalf("missing data classification %d", number)
		}
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, errors.New("connection lost"), &mysqlDriver.MySQLError{Number: 1062}, &mysqlDriver.MySQLError{Number: 1146}, &mysqlDriver.MySQLError{Number: 1205}, &mysqlDriver.MySQLError{Number: 1213}, &mysqlDriver.MySQLError{Number: 1045}} {
		if reason := quarantineReason(err); reason != "" {
			t.Fatalf("infrastructure error %v classified %s", err, reason)
		}
	}
	if quarantineReason(ErrMessageConflict) != "ID_CONFLICT" {
		t.Fatal("missing ID conflict classification")
	}
	for _, raw := range []string{"null", `{}`, `{"id":"x","userId":1,"targetId":2,"type":2,"media":99}`} {
		if _, code, _ := decodeDirtyMessage(raw); code != "INVALID_MESSAGE" {
			t.Fatalf("invalid message %q: %s", raw, code)
		}
	}
	for _, raw := range []string{"", "{broken", "\xff"} {
		if _, code, _ := decodeDirtyMessage(raw); code != "INVALID_JSON" {
			t.Fatalf("invalid JSON %q: %s", raw, code)
		}
	}
	if err := verifyQuarantineBody(models.LegacyMessageQuarantine{RawBody: []byte("other")}, "original"); err == nil {
		t.Fatal("corrupt quarantine accepted")
	}
}

func TestDirtyProcessingTransactionPrefix(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		records     []string
		completed   int
		events      []string
	}{
		{"messages", "", []string{`{"id":"one","userId":1,"targetId":2,"type":2}`, `{"id":"two","userId":1,"targetId":2,"type":2}`}, 2, []string{"begin", "write", "commit", "begin", "write", "commit"}},
		{"isolation", "", []string{"{broken"}, 1, []string{"begin", "write", "commit"}},
		{"second_write", "write", []string{"{first", "{second"}, 1, []string{"begin", "write", "commit", "begin", "write", "rollback"}},
		{"commit_unknown", "commit", []string{"{first", "{second"}, 0, []string{"begin", "write", "commit", "rollback"}},
		{"begin", "begin", []string{"{first"}, 0, []string{"begin"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &groupTransactionPool{stage: tc.stage, cause: errors.New("injected transaction failure")}
			db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
			if err != nil {
				t.Fatal(err)
			}
			// Only stub the absence lookup; all writes and commit boundaries use
			// the existing no-network transaction pool.
			if err := db.Callback().Query().Replace("gorm:query", func(tx *gorm.DB) { tx.RowsAffected = 0 }); err != nil {
				t.Fatal(err)
			}
			n, err := NewMessageManager(db, nil).ProcessDirtyMsgs(context.Background(), tc.records)
			if n != tc.completed {
				t.Fatalf("completed=%d want=%d", n, tc.completed)
			}
			if tc.stage == "" {
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
	if n, err := NewMessageManager(nil, nil).ProcessDirtyMsgs(context.Background(), []string{"bad"}); n != 0 || !errors.Is(err, ErrStorageUnavailable) {
		t.Fatalf("missing dependency=%d %v", n, err)
	}
}
