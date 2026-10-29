package clientsim

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"iChat/messaging"
)

// Byte checkpoints survive Sender recreation, not OS process termination.
// Disk durability and actual transports are separate P6.3 steps.
type checkpointStore struct {
	mu               sync.Mutex
	body             []byte
	loadErr, saveErr error
	commitOnError    bool
	saves            int
	onSave           func(context.Context)
}

func (s *checkpointStore) Load(ctx context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), s.body...), s.loadErr
}
func (s *checkpointStore) Save(ctx context.Context, b []byte) error {
	s.mu.Lock()
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	s.saves++
	err := s.saveErr
	if err == nil || s.commitOnError {
		s.body = append([]byte(nil), b...)
	}
	hook := s.onSave
	s.mu.Unlock()
	if hook != nil {
		hook(ctx)
	}
	return err
}
func (s *checkpointStore) counts() int { s.mu.Lock(); defer s.mu.Unlock(); return s.saves }

func simRequest(key int) messaging.SendRequest {
	return messaging.SendRequest{ClientMessageID: fmt.Sprintf("ABCDEFAB-1234-4234-8234-%012d", key), TargetType: messaging.Private, TargetID: "00034", Media: messaging.Text, Content: "PRIVATE_CLIENT \r\n中😀\x00e\u0301", ClientSentAt: "2026-10-30T12:34:56.123456789+11:00"}
}
func simSender(t *testing.T) (*Sender, *checkpointStore) {
	t.Helper()
	store := &checkpointStore{}
	s, err := OpenSender(context.Background(), 12, store)
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}
func simQueue(t *testing.T, s *Sender, key int) PendingEntry {
	t.Helper()
	e, err := s.Queue(context.Background(), simRequest(key))
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func simEntry(t *testing.T, s *Sender) PendingEntry {
	t.Helper()
	entries, err := s.Entries(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatal(len(entries), err)
	}
	return entries[0]
}
func simBegin(t *testing.T, s *Sender, key string) Attempt {
	t.Helper()
	a, err := s.BeginSubmit(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func simSubmission(t *testing.T, request messaging.SendRequest, state string) messaging.Submission {
	t.Helper()
	c, err := messaging.Normalize(12, request)
	if err != nil {
		t.Fatal(err)
	}
	r := messaging.Submission{State: state, CommandID: c.CommandID, ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, PublishAttemptID: "11111111-1234-4234-8234-123456789012"}
	switch state {
	case "publish_unknown":
		r.ReasonCode = "PUBLISH_UNKNOWN"
	case "not_published", "nacked":
		r.ReasonCode = "DEPENDENCY_UNAVAILABLE"
	case "unroutable":
		r.ReasonCode = "PUBLISH_UNROUTABLE"
	}
	return r
}
func simStatus(t *testing.T, request messaging.SendRequest, state messaging.StatusState) messaging.CommandStatus {
	t.Helper()
	c, err := messaging.Normalize(12, request)
	if err != nil {
		t.Fatal(err)
	}
	r := messaging.CommandStatus{ClientMessageID: c.ClientMessageID, PayloadHash: c.PayloadHash, State: state}
	switch state {
	case messaging.StatusPersisted:
		m, err := messaging.NewMessageDTO(c, 1, time.Date(2026, 10, 30, 0, 0, 0, 123456000, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		r.Message = &m
	case messaging.StatusRejected:
		r.ReasonCode = "NOT_FRIENDS"
	case messaging.StatusPendingOrUnknown, messaging.StatusQuarantined:
		r.PollAfterMs = messaging.DefaultStatusPollAfterMs
	}
	return r
}

func TestSenderCheckpointBeforeReleaseAndStableRequest(t *testing.T) {
	s, store := simSender(t)
	e := simQueue(t, s, 1)
	c, _ := messaging.Normalize(12, e.Request)
	if store.counts() != 2 || e.State != Queued || e.ClientMessageID != c.ClientMessageID || e.CommandID != c.CommandID || e.PayloadHash != c.PayloadHash || e.Request != simRequest(1) {
		t.Fatal("request not checkpointed unchanged")
	}
	if _, err := s.Queue(context.Background(), e.Request); err != nil || store.counts() != 2 {
		t.Fatal("duplicate queue changed state", err)
	}
	for _, mutate := range []func(*messaging.SendRequest){func(r *messaging.SendRequest) { r.Content += "changed" }, func(r *messaging.SendRequest) { r.ClientSentAt = "" }, func(r *messaging.SendRequest) { r.TargetID = "34" }, func(r *messaging.SendRequest) { r.ClientMessageID = strings.ToLower(r.ClientMessageID) }} {
		r := e.Request
		mutate(&r)
		if _, err := s.Queue(context.Background(), r); !errors.Is(err, ErrKeyConflict) {
			t.Fatal(err)
		}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	store.onSave = func(context.Context) { close(entered); <-release }
	type result struct {
		a   Attempt
		err error
	}
	done := make(chan result, 1)
	go func() { a, err := s.BeginSubmit(context.Background(), e.ClientMessageID); done <- result{a, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("checkpoint not entered")
	}
	select {
	case <-done:
		t.Fatal("attempt released before Save returned")
	default:
	}
	body, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := decodeSnapshot(12, body)
	if err != nil || !checkpoint[e.ClientMessageID].InFlight || checkpoint[e.ClientMessageID].State != Submitting {
		t.Fatal("submitting checkpoint missing", err)
	}
	unblock()
	r := <-done
	if r.err != nil || r.a.Request != e.Request || r.a.Number != 1 {
		t.Fatal(r.err)
	}
}

func TestSenderRestartAndStaleAttempt(t *testing.T) {
	s, store := simSender(t)
	e := simQueue(t, s, 1)
	first := simBegin(t, s, e.ClientMessageID)
	if _, err := s.BeginSubmit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrInFlight) {
		t.Fatal(err)
	}
	// Retire the old Sender before handing its store to a new instance.
	reopened, err := OpenSender(context.Background(), 12, store)
	if err != nil {
		t.Fatal(err)
	}
	recovered := simEntry(t, reopened)
	if recovered.State != PublishUnknown || recovered.InFlight || recovered.Attempts != 1 || recovered.Request != first.Request || recovered.CommandID != e.CommandID || recovered.PayloadHash != e.PayloadHash {
		t.Fatal("restart changed logical identity")
	}
	second := simBegin(t, reopened, e.ClientMessageID)
	if second.Number != 2 || second.Request != first.Request {
		t.Fatal("retry did not preserve request")
	}
	if err := reopened.CompleteSubmission(context.Background(), first, simSubmission(t, first.Request, "accepted")); !errors.Is(err, ErrStaleAttempt) {
		t.Fatal(err)
	}
	if err := reopened.CompleteSubmission(context.Background(), second, simSubmission(t, second.Request, "accepted")); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CompleteUnknown(context.Background(), second); !errors.Is(err, ErrStaleAttempt) {
		t.Fatal(err)
	}
	if simEntry(t, reopened).State != Accepted {
		t.Fatal("late callback regressed acceptance")
	}
}

func TestSenderInterruptedRetryCannotRetainNotPublished(t *testing.T) {
	s, store := simSender(t)
	e := simQueue(t, s, 1)
	first := simBegin(t, s, e.ClientMessageID)
	if err := s.CompleteSubmission(context.Background(), first, simSubmission(t, e.Request, "not_published")); err != nil {
		t.Fatal(err)
	}
	second := simBegin(t, s, e.ClientMessageID)
	if second.Number != 2 || simEntry(t, s).State != Submitting {
		t.Fatal("retry retained old non-publication evidence")
	}
	reopened, err := OpenSender(context.Background(), 12, store)
	if err != nil || simEntry(t, reopened).State != PublishUnknown {
		t.Fatal("interrupted retry claimed not published", err)
	}
}

func TestSenderQueueUnknownCommitRetainsOriginalKey(t *testing.T) {
	for _, committed := range []bool{false, true} {
		s, store := simSender(t)
		request := simRequest(1)
		store.saveErr = errors.New("PRIVATE_QUEUE")
		store.commitOnError = committed
		entry, err := s.Queue(context.Background(), request)
		if !errors.Is(err, ErrStoreUncertain) || entry.Request.Content != "" {
			t.Fatal("failed queue released record", err)
		}
		store.saveErr = nil
		store.commitOnError = false
		reopened, err := OpenSender(context.Background(), 12, store)
		if err != nil {
			t.Fatal(err)
		}
		entry, err = reopened.Queue(context.Background(), request)
		if err != nil || entry.Request != request || entry.State != Queued {
			t.Fatal("queue recovery changed request", err)
		}
		if entries, err := reopened.Entries(context.Background()); err != nil || len(entries) != 1 {
			t.Fatal("queue recovery duplicated record", err)
		}
	}
}

func TestSenderTerminalBeforeAcceptedAndNoRegression(t *testing.T) {
	for _, terminal := range []messaging.StatusState{messaging.StatusPersisted, messaging.StatusRejected} {
		t.Run(string(terminal), func(t *testing.T) {
			s, store := simSender(t)
			e := simQueue(t, s, 1)
			a := simBegin(t, s, e.ClientMessageID)
			status := simStatus(t, e.Request, terminal)
			if err := s.ApplyStatus(context.Background(), status); err != nil {
				t.Fatal(err)
			}
			original := simEntry(t, s)
			if err := s.CompleteSubmission(context.Background(), a, simSubmission(t, a.Request, "accepted")); err != nil {
				t.Fatal(err)
			}
			for _, weak := range []messaging.StatusState{messaging.StatusPendingOrUnknown, messaging.StatusQuarantined, messaging.StatusConflict} {
				if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, weak)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ApplyStatus(context.Background(), status); err != nil {
				t.Fatal(err)
			}
			if _, err := s.BeginSubmit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrBlocked) {
				t.Fatal(err)
			}
			got := simEntry(t, s)
			if got.State != SendState(terminal) || got.InFlight || !reflect.DeepEqual(got.Result, original.Result) {
				t.Fatal("terminal regressed")
			}
			other := simStatus(t, e.Request, messaging.StatusRejected)
			if terminal == messaging.StatusRejected {
				other = simStatus(t, e.Request, messaging.StatusPersisted)
			}
			if err := s.ApplyStatus(context.Background(), other); !errors.Is(err, ErrContradiction) {
				t.Fatal(err)
			}
			if terminal == messaging.StatusPersisted {
				changed := simStatus(t, e.Request, terminal)
				changed.Message.Seq = 2
				if err := s.ApplyStatus(context.Background(), changed); !errors.Is(err, ErrContradiction) {
					t.Fatal(err)
				}
				status.Message.Content = "caller mutation"
				got.Result.Message.Content = "returned mutation"
				if simEntry(t, s).Result.Message.Content != e.Request.Content {
					t.Fatal("message pointer alias")
				}
			}
			reopened, err := OpenSender(context.Background(), 12, store)
			if err != nil || !reflect.DeepEqual(simEntry(t, reopened).Result, original.Result) {
				t.Fatal("terminal checkpoint changed", err)
			}
		})
	}
}

func TestSenderUncertainStorageRequiresReopen(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			s, store := simSender(t)
			e := simQueue(t, s, 1)
			cause := errors.New("PRIVATE_STORE_DIAGNOSTIC")
			store.saveErr = cause
			store.commitOnError = committed
			a, err := s.BeginSubmit(context.Background(), e.ClientMessageID)
			if !errors.Is(err, ErrStoreUncertain) || !errors.Is(err, cause) || a.Request.Content != "" || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "PRIVATE") {
				t.Fatal("uncertain commit leaked result", err)
			}
			store.saveErr = nil
			store.commitOnError = false
			if _, err := s.Entries(context.Background()); !errors.Is(err, ErrStoreUncertain) {
				t.Fatal(err)
			}
			if _, err := s.Queue(context.Background(), simRequest(2)); !errors.Is(err, ErrStoreUncertain) {
				t.Fatal(err)
			}
			if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPersisted)); !errors.Is(err, ErrStoreUncertain) {
				t.Fatal(err)
			}
			reopened, err := OpenSender(context.Background(), 12, store)
			if err != nil {
				t.Fatal(err)
			}
			want := Queued
			if committed {
				want = PublishUnknown
			}
			if got := simEntry(t, reopened); got.State != want || got.Request != e.Request {
				t.Fatal("wrong recovered commit outcome")
			}
			if a := simBegin(t, reopened, e.ClientMessageID); a.Request != e.Request {
				t.Fatal("recovery changed request")
			}
		})
	}
}

func TestSenderSubmissionEvidenceAndBlockedStates(t *testing.T) {
	for _, state := range []string{"accepted", "publish_unknown", "not_published", "unroutable", "nacked", "lost_response"} {
		t.Run(state, func(t *testing.T) {
			s, _ := simSender(t)
			e := simQueue(t, s, 1)
			a := simBegin(t, s, e.ClientMessageID)
			var err error
			if state == "lost_response" {
				err = s.CompleteUnknown(context.Background(), a)
			} else {
				err = s.CompleteSubmission(context.Background(), a, simSubmission(t, a.Request, state))
			}
			want := NotPublished
			if state == "accepted" {
				want = Accepted
			} else if state == "publish_unknown" || state == "lost_response" {
				want = PublishUnknown
			}
			if err != nil || simEntry(t, s).State != want || simEntry(t, s).InFlight {
				t.Fatal(state, err)
			}
			if state == "accepted" {
				if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPendingOrUnknown)); err != nil {
					t.Fatal(err)
				}
				b := simBegin(t, s, e.ClientMessageID)
				if err := s.CompleteUnknown(context.Background(), b); err != nil || simEntry(t, s).State != Accepted {
					t.Fatal("known acceptance regressed", err)
				}
			}
		})
	}
	for _, state := range []messaging.StatusState{messaging.StatusQuarantined, messaging.StatusConflict} {
		s, _ := simSender(t)
		e := simQueue(t, s, 1)
		if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, state)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BeginSubmit(context.Background(), e.ClientMessageID); !errors.Is(err, ErrBlocked) {
			t.Fatal(err)
		}
		if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPendingOrUnknown)); err != nil || simEntry(t, s).State != SendState(state) {
			t.Fatal(err)
		}
		if err := s.ApplyStatus(context.Background(), simStatus(t, e.Request, messaging.StatusPersisted)); err != nil || simEntry(t, s).State != Persisted {
			t.Fatal("terminal did not reconcile blocked state", err)
		}
	}
}

func TestSenderRejectsInvalidResultsWithoutChangingCheckpoint(t *testing.T) {
	for _, mode := range []string{"identity", "hash", "attempt", "reason", "state", "request_ticket", "number_ticket"} {
		s, store := simSender(t)
		e := simQueue(t, s, 1)
		a := simBegin(t, s, e.ClientMessageID)
		r := simSubmission(t, a.Request, "accepted")
		switch mode {
		case "identity":
			r.CommandID = strings.Repeat("f", 64)
		case "hash":
			r.PayloadHash = strings.Repeat("f", 64)
		case "attempt":
			r.PublishAttemptID = ""
		case "reason":
			r.ReasonCode = "PUBLISH_UNKNOWN"
		case "state":
			r.State = "persisted"
		case "request_ticket":
			a.Request.Content += "changed"
		case "number_ticket":
			a.Number++
		}
		before := store.counts()
		if err := s.CompleteSubmission(context.Background(), a, r); err == nil || store.counts() != before || !simEntry(t, s).InFlight {
			t.Fatal("invalid receipt changed state", mode, err)
		}
	}
	for _, mode := range []string{"id", "hash", "content", "route", "seq", "time", "poll", "reason"} {
		s, store := simSender(t)
		e := simQueue(t, s, 1)
		r := simStatus(t, e.Request, messaging.StatusPersisted)
		switch mode {
		case "id":
			r.ClientMessageID = "11111111-1234-4234-8234-123456789012"
		case "hash":
			r.PayloadHash = strings.Repeat("f", 64)
		case "content":
			r.Message.Content += "changed"
		case "route":
			r.Message.TargetID = "35"
		case "seq":
			r.Message.Seq = 0
		case "time":
			r.Message.ServerCreatedAt = "bad"
		case "poll":
			r.PollAfterMs = 2000
		case "reason":
			r.ReasonCode = "NOT_FRIENDS"
		}
		before := store.counts()
		if err := s.ApplyStatus(context.Background(), r); err == nil || store.counts() != before || simEntry(t, s).State != Queued {
			t.Fatal("invalid status changed state", mode, err)
		}
	}
}

func TestSenderConcurrentCallbacksAndSingleSubmission(t *testing.T) {
	s, _ := simSender(t)
	e := simQueue(t, s, 1)
	const n = 24
	var wg sync.WaitGroup
	attempts := make(chan Attempt, n)
	failures := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := s.BeginSubmit(context.Background(), e.ClientMessageID)
			if err == nil {
				attempts <- a
			} else if !errors.Is(err, ErrInFlight) {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(attempts)
	close(failures)
	if len(attempts) != 1 || len(failures) != 0 {
		t.Fatal("multiple concurrent submissions", len(attempts), len(failures))
	}
	a := <-attempts
	status := simStatus(t, e.Request, messaging.StatusPersisted)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.ApplyStatus(context.Background(), status); err != nil {
				t.Error(err)
			}
		}()
	}
	if err := s.CompleteSubmission(context.Background(), a, simSubmission(t, a.Request, "accepted")); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got := simEntry(t, s); got.State != Persisted || got.InFlight || got.Attempts != 1 {
		t.Fatal("concurrent terminal lost")
	}
}
