package clientsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"iChat/messaging"
)

func TestSenderSnapshotRoundTripAndRecoveryStates(t *testing.T) {
	for _, mode := range []string{"queued", "submitting", "publish_unknown", "not_published", "accepted", "accepted_inflight", "pending", "quarantined", "ID_CONFLICT", "persisted", "rejected", "terminal_inflight"} {
		t.Run(mode, func(t *testing.T) {
			s, store := simSender(t)
			e := simQueue(t, s, 1)
			want := Queued
			switch mode {
			case "queued":
			case "pending":
				if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPendingOrUnknown)); err != nil {
					t.Fatal(err)
				}
				want = PublishUnknown
			case "quarantined", "ID_CONFLICT", "persisted", "rejected":
				if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusState(mode))); err != nil {
					t.Fatal(err)
				}
				want = SendState(mode)
			default:
				a := simBegin(t, s, e.ClientMessageID)
				switch mode {
				case "submitting":
					want = PublishUnknown
				case "publish_unknown":
					if err := s.CompleteUnknown(context.Background(), a); err != nil {
						t.Fatal(err)
					}
					want = PublishUnknown
				case "not_published":
					if err := s.CompleteSubmission(context.Background(), a, simSubmission(t, e.Request, "not_published")); err != nil {
						t.Fatal(err)
					}
					want = NotPublished
				case "accepted", "accepted_inflight":
					if err := s.CompleteSubmission(context.Background(), a, simSubmission(t, e.Request, "accepted")); err != nil {
						t.Fatal(err)
					}
					want = Accepted
					if mode == "accepted_inflight" {
						simBegin(t, s, e.ClientMessageID)
					}
				case "terminal_inflight":
					if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPersisted)); err != nil {
						t.Fatal(err)
					}
					want = Persisted
				}
			}
			before := simEntry(t, s)
			writes := store.counts()
			reopened, err := OpenSender(context.Background(), 12, store)
			if err != nil {
				t.Fatal(err)
			}
			got := simEntry(t, reopened)
			if got.State != want || got.InFlight || got.Request != e.Request || got.Attempts != before.Attempts || !reflect.DeepEqual(got.Result, before.Result) {
				t.Fatal("snapshot round trip changed record", mode)
			}
			more := 0
			if before.InFlight {
				more = 1
			}
			if store.counts() != writes+more {
				t.Fatal("unexpected recovery write")
			}
		})
	}
}

func TestSenderSnapshotRejectsCorruptionAndWrongAccount(t *testing.T) {
	s, store := simSender(t)
	simQueue(t, s, 1)
	body, _ := store.Load(context.Background())
	for _, mode := range []string{"version", "sender", "id", "hash", "request", "state", "missing_result", "wrong_result", "duplicate_entry", "null_entries", "inflight_queued", "attempts_queued", "no_attempt_accepted", "noncanonical_sender", "reordered", "unknown", "duplicate_field", "trailing", "null_attempts", "number_attempts", "unicode", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			var snapshot senderSnapshot
			if err := json.Unmarshal(body, &snapshot); err != nil {
				t.Fatal(err)
			}
			e := &snapshot.Entries[0]
			switch mode {
			case "version":
				snapshot.Version++
			case "sender":
				snapshot.SenderID = "34"
			case "id":
				e.ClientMessageID = strings.ToUpper(e.ClientMessageID)
			case "hash":
				e.PayloadHash = strings.Repeat("f", 64)
			case "request":
				e.Request.Content += "changed"
			case "state":
				e.State = "delivered"
			case "missing_result":
				e.State = Persisted
			case "wrong_result":
				e.State = Persisted
				e.Result = new(messaging.CommandStatus)
			case "duplicate_entry":
				snapshot.Entries = append(snapshot.Entries, *e)
			case "null_entries":
				snapshot.Entries = nil
			case "inflight_queued":
				e.InFlight = true
			case "attempts_queued":
				e.Attempts = 1
			case "no_attempt_accepted":
				e.State = Accepted
			case "noncanonical_sender":
				snapshot.SenderID = "012"
			}
			bad, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "reordered":
				bad = append([]byte(" "), bad...)
			case "unknown":
				bad = bytes.Replace(bad, []byte(`"version":1`), []byte(`"version":1,"token":"PRIVATE"`), 1)
			case "duplicate_field":
				bad = bytes.Replace(bad, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1)
			case "trailing":
				bad = append(bad, []byte("{}")...)
			case "null_attempts":
				bad = bytes.Replace(bad, []byte(`"attempts":"0"`), []byte(`"attempts":null`), 1)
			case "number_attempts":
				bad = bytes.Replace(bad, []byte(`"attempts":"0"`), []byte(`"attempts":0`), 1)
			case "unicode":
				bad = bytes.Replace(bad, []byte("PRIVATE_CLIENT"), []byte(`\ud800`), 1)
			case "oversized":
				bad = make([]byte, MaxSnapshotBytes+1)
			}
			broken := &checkpointStore{body: bad}
			if result, err := OpenSender(context.Background(), 12, broken); !errors.Is(err, ErrSnapshot) || result != nil || broken.counts() != 0 {
				t.Fatal("accepted corrupt checkpoint", mode, err)
			}
		})
	}
	if _, err := OpenSender(context.Background(), 34, store); !errors.Is(err, ErrSnapshot) {
		t.Fatal("cross-account restore", err)
	}
}

func TestSenderContextStoreFailureAndPanicBoundaries(t *testing.T) {
	ctx := context.Background()
	var typedNil *checkpointStore
	for _, store := range []SnapshotStore{nil, typedNil} {
		if s, err := OpenSender(ctx, 12, store); s != nil || !errors.Is(err, ErrInput) {
			t.Fatal(err)
		}
	}
	if _, err := OpenSender(nil, 12, &checkpointStore{}); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	if _, err := OpenSender(ctx, 0, &checkpointStore{}); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := OpenSender(canceled, 12, &checkpointStore{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	cause := errors.New("PRIVATE_LOAD")
	if _, err := OpenSender(ctx, 12, &checkpointStore{loadErr: cause}); !errors.Is(err, ErrStore) || !errors.Is(err, cause) || strings.Contains(fmt.Sprintf("%#v", err), "PRIVATE") {
		t.Fatal(err)
	}
	if _, err := OpenSender(ctx, 12, &checkpointStore{saveErr: cause}); !errors.Is(err, ErrStoreUncertain) {
		t.Fatal(err)
	}
	s, store := simSender(t)
	e := simQueue(t, s, 1)
	writes := store.counts()
	if _, err := s.BeginSubmit(canceled, e.ClientMessageID); !errors.Is(err, context.Canceled) || store.counts() != writes {
		t.Fatal(err)
	}
	if _, err := s.Queue(nil, simRequest(2)); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
	store.onSave = func(context.Context) { panic("PRIVATE_PANIC") }
	func() {
		defer func() {
			if recover() == nil {
				t.Error("expected storage panic")
			}
		}()
		s.BeginSubmit(ctx, e.ClientMessageID)
	}()
	if _, err := s.Entries(ctx); !errors.Is(err, ErrStoreUncertain) {
		t.Fatal("panic left owner usable", err)
	}
	store.onSave = nil
	reopened, err := OpenSender(ctx, 12, store)
	if err != nil || simEntry(t, reopened).State != PublishUnknown {
		t.Fatal(err)
	}
	// A nil Save result is known committed even if the observer was canceled.
	local, stop := context.WithCancel(ctx)
	store.onSave = func(context.Context) { stop() }
	a, err := reopened.BeginSubmit(local, e.ClientMessageID)
	if err != nil || a.Request != e.Request || local.Err() == nil {
		t.Fatal("known commit lost", err)
	}
}

func TestSenderUncertainTerminalCommitAndAliasing(t *testing.T) {
	for _, committed := range []bool{false, true} {
		s, store := simSender(t)
		e := simQueue(t, s, 1)
		a := simBegin(t, s, e.ClientMessageID)
		store.saveErr = errors.New("PRIVATE_COMMIT")
		store.commitOnError = committed
		status := simStatus(t, e.Request, messaging.StatusPersisted)
		if err := s.ApplyStatus(context.Background(), status); !errors.Is(err, ErrStoreUncertain) {
			t.Fatal(err)
		}
		status.Message.Content = "mutated caller status"
		store.saveErr = nil
		store.commitOnError = false
		reopened, err := OpenSender(context.Background(), 12, store)
		if err != nil {
			t.Fatal(err)
		}
		got := simEntry(t, reopened)
		want := PublishUnknown
		if committed {
			want = Persisted
		}
		if got.State != want || got.Request != a.Request || (committed && got.Result.Message.Content != e.Request.Content) {
			t.Fatal("unknown terminal save mishandled")
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v %v %#v %v %#v", got, got, got, a, a, reopened, reopened), "PRIVATE_CLIENT") {
			t.Fatal("default formatting exposed content")
		}
	}
}

func TestSenderCapacityOrderingAndAttemptExhaustion(t *testing.T) {
	entries := make(map[string]PendingEntry, MaxPendingEntries)
	for i := MaxPendingEntries; i > 0; i-- {
		r := simRequest(i)
		c, _ := messaging.Normalize(12, r)
		entries[c.ClientMessageID] = PendingEntry{Request: r, ClientMessageID: c.ClientMessageID, CommandID: c.CommandID, PayloadHash: c.PayloadHash, State: Queued}
	}
	body, err := encodeSnapshot(12, entries)
	if err != nil {
		t.Fatal(err)
	}
	store := &checkpointStore{body: body}
	s, err := OpenSender(context.Background(), 12, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue(context.Background(), simRequest(MaxPendingEntries+1)); !errors.Is(err, ErrCapacity) || store.counts() != 0 {
		t.Fatal(err)
	}
	got, err := s.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].ClientMessageID >= got[i].ClientMessageID {
			t.Fatal("unstable ordering")
		}
	}
	e := got[0]
	e.Attempts = ^messaging.Sequence(0)
	e.State = PublishUnknown
	body, err = encodeSnapshot(12, map[string]PendingEntry{e.ClientMessageID: e})
	if err != nil {
		t.Fatal(err)
	}
	exhausted := &checkpointStore{body: body}
	s, err = OpenSender(context.Background(), 12, exhausted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginSubmit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrCapacity) || exhausted.counts() != 0 {
		t.Fatal("attempt counter wrapped", err)
	}
	if _, err := s.BeginSubmit(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := s.Queue(context.Background(), messaging.SendRequest{}); !errors.Is(err, ErrInput) {
		t.Fatal(err)
	}
}
