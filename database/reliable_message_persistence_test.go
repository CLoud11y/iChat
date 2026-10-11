package database

import (
	"context"
	"errors"
	"iChat/messaging"
	"testing"
)

func TestReliableClientTimestampPrecision(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  any
	}{
		{"", nil},
		{"2026-10-11T01:02:03.999999999+11:00", "2026-10-10 14:02:03.999999"},
		{"2026-10-11T00:00:00.000000999Z", "2026-10-11 00:00:00.000000"},
		{"2026-10-11T00:00:00.123456789Z", "2026-10-11 00:00:00.123456"},
		{"9999-12-31T23:59:59.999999999Z", "9999-12-31 23:59:59.999999"},
	} {
		got, err := clientTimeForMySQL(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("%s -> %v err=%v want=%v", tc.input, got, err, tc.want)
		}
	}
	if _, err := clientTimeForMySQL("invalid"); err == nil {
		t.Fatal("invalid timestamp accepted")
	}
}

func TestReliablePersistenceValidation(t *testing.T) {
	var unavailable *MessageRepository
	if result, err := unavailable.PersistCommand(context.Background(), testCommand(t)); !errors.Is(err, ErrCommandRepositoryUnavailable) || result.Result != nil {
		t.Fatalf("unavailable repository: %+v %v", result, err)
	}
	state := &commandTestState{}
	repo := newCommandTestRepository(t, state)
	invalid := testCommand(t)
	invalid.PayloadHash = "tampered"
	if result, err := repo.PersistCommand(context.Background(), invalid); !errors.Is(err, messaging.ErrIdentityMismatch) || result.Result != nil {
		t.Fatalf("unvalidated command reached persistence: %+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := repo.PersistCommand(ctx, testCommand(t)); !errors.Is(err, context.Canceled) || result.Result != nil {
		t.Fatalf("canceled command: %+v %v", result, err)
	}
}
