package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"

	"iChat/messaging"
)

func TestMessageSyncRangeDenialAndReleaseFailure(t *testing.T) {
	for _, brokenRelease := range []bool{false, true} {
		s := &pageTestState{conversation: []driver.Value{"private", int64(1), int64(2), nil, "0"}, cause: errors.New("PRIVATE_RELEASE_FAILURE")}
		if brokenRelease {
			s.fail = "rollback"
		}
		r := pageTestRepository(t, s)
		p, e := r.SyncMessages(context.Background(), 1, "p:1:2", "1", 1)
		if !errors.Is(e, ErrSyncCursorAhead) || !errors.Is(e, messaging.ErrInvalidInput) || !reflect.DeepEqual(p, messaging.SyncPage{}) {
			t.Fatal(p, e)
		}
		if brokenRelease {
			if e == ErrSyncCursorAhead || !errors.Is(e, s.cause) {
				t.Fatal("release failure looked like a pure client error", e)
			}
		} else if e != ErrSyncCursorAhead {
			t.Fatal("range denial not distinguishable", e)
		}
	}
}
