package clientsim

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/messaging"
)

func simResultFrame(t *testing.T, state messaging.StatusState, reference bool) []byte {
	t.Helper()
	status := simStatus(t, simRequest(1), state)
	data := messaging.CommandResultData{ClientMessageID: status.ClientMessageID, PayloadHash: status.PayloadHash, State: string(state), ReasonCode: status.ReasonCode, Message: status.Message}
	if status.Message != nil {
		seq := status.Message.Seq
		data.Seq = &seq
		data.MessageID = status.Message.MessageID
		data.ConversationID = status.Message.ConversationID
	}
	if reference {
		data.Message = nil
	}
	frame, err := messaging.NewCommandResultFrame(12, data)
	if err != nil {
		t.Fatal(err)
	}
	return frame.Bytes()
}

func TestCommandResultArrivesBeforeHTTPReceipt(t *testing.T) {
	for _, state := range []messaging.StatusState{messaging.StatusPersisted, messaging.StatusRejected} {
		t.Run(string(state), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			accepted := acceptedReply(t)
			r, s, store, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) { close(entered); <-release; simReply(w, 202, accepted) })
			r.options.MaxConcurrent = 1 // The notification must not borrow an HTTP slot.
			r.slots = make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() { done <- r.Submit(context.Background(), e.ClientMessageID) }()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("HTTP not entered")
			}
			body := simResultFrame(t, state, false)
			if needsQuery, err := r.ObserveCommandResult(context.Background(), body); err != nil || needsQuery {
				t.Fatal("early result could not be saved", err)
			}
			got := simEntry(t, s)
			if got.State != SendState(state) || !got.InFlight {
				t.Fatal("terminal was postponed until HTTP returned")
			}
			select {
			case <-done:
				t.Fatal("HTTP barrier released early")
			default:
			}
			writes := store.counts()
			if needed, err := r.ObserveCommandResult(context.Background(), body); err != nil || needed || store.counts() != writes {
				t.Fatal("duplicate terminal was not idempotent", err)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := simEntry(t, s); got.State != SendState(state) || got.InFlight {
				t.Fatal("accepted regressed terminal")
			}
		})
	}
}

func TestCommandResultReferencesNeedAuthoritativeQuery(t *testing.T) {
	terminal := statusReply(t, messaging.StatusPersisted)
	r, s, store, e := simHTTPFixture(t, func(w http.ResponseWriter, req *http.Request) { simReply(w, 200, terminal) })
	body := simResultFrame(t, messaging.StatusPersisted, true)
	before := store.counts()
	if needed, err := r.ObserveCommandResult(context.Background(), body); err != nil || !needed || store.counts() != before || simEntry(t, s).State != Queued {
		t.Fatal("reference fabricated terminal", err)
	}
	if err := r.Poll(context.Background(), e.ClientMessageID); err != nil {
		t.Fatal(err)
	}
	if needed, err := r.ObserveCommandResult(context.Background(), body); err != nil || needed || simEntry(t, s).State != Persisted {
		t.Fatal("known reference not idempotent", err)
	}
	changed := strings.Replace(string(body), `"seq":"1"`, `"seq":"2"`, 1)
	if _, err := r.ObserveCommandResult(context.Background(), []byte(changed)); !errors.Is(err, ErrContradiction) {
		t.Fatal("contradictory reference accepted", err)
	}
	// A syntactically valid reference to a different conversation is not the
	// local request, even when event ID/key/hash/message ID are all unchanged.
	changed = strings.Replace(string(body), `"conversationId":"p:12:34"`, `"conversationId":"p:12:35"`, 1)
	if _, err := r.ObserveCommandResult(context.Background(), []byte(changed)); !errors.Is(err, ErrProtocol) {
		t.Fatal("reference route changed", err)
	}
}

func TestCommandResultRejectsBadFramesWithoutChangingStorage(t *testing.T) {
	base := string(simResultFrame(t, messaging.StatusPersisted, false))
	for _, mode := range []string{"type", "event", "event_case", "duplicate_envelope", "duplicate_data", "nested_duplicate", "nested_alias", "null", "number_seq", "unicode", "hash", "message_id", "route", "extra", "trailing", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			body := base
			switch mode {
			case "type":
				body = strings.Replace(body, `"type":"command.result"`, `"type":"message"`, 1)
			case "event":
				body = strings.Replace(body, `"eventId":"`, `"eventId":"f`, 1)
			case "event_case":
				var wrapper struct {
					Event string `json:"eventId"`
				}
				json.Unmarshal([]byte(body), &wrapper)
				body = strings.Replace(body, wrapper.Event, strings.ToUpper(wrapper.Event), 1)
			case "duplicate_envelope":
				body = strings.Replace(body, `"type":"command.result"`, `"type":"command.result","type":"command.result"`, 1)
			case "duplicate_data":
				body = strings.Replace(body, `"state":"persisted"`, `"state":"persisted","\u0073tate":"persisted"`, 1)
			case "nested_duplicate":
				body = strings.Replace(body, `"media":"text"`, `"media":"text","media":"text"`, 1)
			case "nested_alias":
				body = strings.Replace(body, `"media":"text"`, `"Media":"text"`, 1)
			case "null":
				body = strings.Replace(body, `"seq":"1"`, `"seq":null`, 1)
			case "number_seq":
				body = strings.Replace(body, `"seq":"1"`, `"seq":1`, 1)
			case "unicode":
				body = strings.Replace(body, "PRIVATE_CLIENT", `\ud800`, 1)
			case "hash":
				body = strings.Replace(body, `"payloadHash":"`, `"payloadHash":"0`, 1)
			case "message_id":
				body = strings.Replace(body, `"messageId":"`, `"messageId":"0`, 1)
			case "route":
				body = strings.ReplaceAll(body, `"targetId":"34"`, `"targetId":"35"`)
			case "extra":
				body = strings.Replace(body, `"state":"persisted"`, `"state":"persisted","userId":"12"`, 1)
			case "trailing":
				body += "{}"
			case "oversized":
				body += strings.Repeat(" ", messaging.MaxBodyBytes)
			}
			r, s, store, _ := simHTTPFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("notification performed HTTP") })
			before := store.counts()
			if needed, err := r.ObserveCommandResult(context.Background(), []byte(body)); !errors.Is(err, ErrProtocol) || needed || store.counts() != before || simEntry(t, s).State != Queued {
				t.Fatal("bad result changed local state", mode, err)
			}
		})
	}
	// A valid frame for this user but absent from this local pending table is
	// ignored by the caller through ErrNotFound; no new record is invented.
	s, _ := simSender(t)
	r := simHTTPRunner(t, s, "http://127.0.0.1:1")
	if _, err := r.ObserveCommandResult(context.Background(), []byte(base)); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if entries, err := s.Entries(context.Background()); err != nil || len(entries) != 0 {
		t.Fatal(err)
	}
}

func TestCommandResultCloseJoinsStorageAndStopsLateFrames(t *testing.T) {
	r, s, store, e := simHTTPFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("notification performed HTTP") })
	body := simResultFrame(t, messaging.StatusPersisted, false)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	store.onSave = func(context.Context) { close(entered); <-release }
	done := make(chan error, 1)
	go func() { _, err := r.ObserveCommandResult(context.Background(), body); done <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notification storage not entered")
	}
	closed := make(chan struct{})
	go func() { r.Close(); close(closed) }()
	select {
	case <-r.work.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("runner not canceled")
	}
	select {
	case <-closed:
		t.Fatal("Close abandoned local write")
	default:
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("notification owner not joined")
	}
	got := simEntry(t, s)
	if got.State != Persisted || got.Request != e.Request {
		t.Fatal("known local commit lost")
	}
	if _, err := r.ObserveCommandResult(context.Background(), body); !errors.Is(err, ErrHTTPClosed) {
		t.Fatal("closed runner accepted late frame", err)
	}
	if !reflect.DeepEqual(got.Result, simEntry(t, s).Result) {
		t.Fatal("late result changed storage")
	}
}

type resultFailureStore struct {
	SnapshotStore
	cause  error
	commit bool
}

func (s resultFailureStore) Save(ctx context.Context, body []byte) error {
	entries, err := decodeSnapshot(12, body)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.State == Persisted {
			if s.commit {
				if err := s.SnapshotStore.Save(ctx, body); err != nil {
					return err
				}
			}
			return s.cause
		}
	}
	return s.SnapshotStore.Save(ctx, body)
}
func TestCommandResultStorageUncertaintyAndUnsignedSequence(t *testing.T) {
	for _, committed := range []bool{false, true} {
		backing := &checkpointStore{}
		cause := errors.New("PRIVATE_RESULT_STORE")
		s, err := OpenSender(context.Background(), 12, resultFailureStore{backing, cause, committed})
		if err != nil {
			t.Fatal(err)
		}
		e := simQueue(t, s, 1)
		r := simHTTPRunner(t, s, "http://127.0.0.1:1")
		body := simResultFrame(t, messaging.StatusPersisted, false)
		body = []byte(strings.ReplaceAll(string(body), `"seq":"1"`, `"seq":"18446744073709551615"`))
		if needed, err := r.ObserveCommandResult(context.Background(), body); needed || !errors.Is(err, ErrStoreUncertain) || !errors.Is(err, cause) {
			t.Fatal("uncertain result fabricated success", err)
		}
		if _, err := s.Entries(context.Background()); !errors.Is(err, ErrStoreUncertain) {
			t.Fatal(err)
		}
		r.Close()
		restored, err := OpenSender(context.Background(), 12, backing)
		if err != nil {
			t.Fatal(err)
		}
		got := simEntry(t, restored)
		want := Queued
		if committed {
			want = Persisted
		}
		if got.State != want || got.Request != e.Request || (committed && got.Result.Message.Seq != ^messaging.Sequence(0)) {
			t.Fatal("result save outcome not recovered")
		}
	}
}
