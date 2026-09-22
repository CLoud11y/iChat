package scheduledtasks

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type ownershipProbe struct {
	checks int
	failAt int
	acked  []string
}

var ownershipFailure = errors.New("ownership uncertain")

func (p *ownershipProbe) Check(context.Context) error {
	p.checks++
	if p.checks >= p.failAt {
		return ownershipFailure
	}
	return nil
}
func (p *ownershipProbe) Acknowledge(_ context.Context, msgs []string) error {
	if p.checks >= p.failAt {
		return ownershipFailure
	}
	p.acked = append(p.acked, msgs...)
	return nil
}

type ownedStoreProbe struct {
	processed  []string
	failSecond bool
}

func (p *ownedStoreProbe) GetAllDirtyMsgs(context.Context) ([]string, int, error) {
	return []string{"first", "second"}, 2, nil
}
func (p *ownedStoreProbe) ProcessDirtyMsgs(_ context.Context, msgs []string) (int, error) {
	if p.failSecond && msgs[0] == "second" {
		return 0, errors.New("database offline")
	}
	p.processed = append(p.processed, msgs...)
	return len(msgs), nil
}
func (*ownedStoreProbe) RemDirtyMsgs(context.Context, []string) error {
	panic("unguarded acknowledgement")
}

func TestOwnedMessagesBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name             string
		failAt           int
		failSecond       bool
		processed, acked []string
		wantError        bool
	}{
		{"success", 100, false, []string{"first", "second"}, []string{"first", "second"}, false},
		{"lost before read", 1, false, nil, nil, true},
		{"lost before second transaction", 3, false, []string{"first"}, nil, true},
		{"durable prefix on database failure", 100, true, []string{"first"}, []string{"first"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := &ownershipProbe{failAt: tc.failAt}
			store := &ownedStoreProbe{failSecond: tc.failSecond}
			err := SyncMsg(context.Background(), OwnedMessages{MessageStore: store, Owner: owner})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v", err)
			}
			if !reflect.DeepEqual(store.processed, tc.processed) || !reflect.DeepEqual(owner.acked, tc.acked) {
				t.Fatalf("processed=%v acknowledged=%v", store.processed, owner.acked)
			}
		})
	}
}
