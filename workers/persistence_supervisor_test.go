package workers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iChat/database"
	"iChat/messaging/rabbitmq"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

type supervisorTestGroup struct {
	service *PersistenceService
	closes  atomic.Int64
	close   func(context.Context) error
}

func (g *supervisorTestGroup) Consumers() []PersistenceConsumer { panic("unexpected group read") }
func (g *supervisorTestGroup) Close(ctx context.Context) error {
	g.closes.Add(1)
	if g.close != nil {
		return g.close(ctx)
	}
	return nil
}

func supervisorTestOptions() PersistenceSupervisorOptions {
	o := DefaultPersistenceSupervisorOptions()
	o.Worker = serviceTestOptions()
	o.BaseDelay, o.MaxDelay = 20*time.Millisecond, 80*time.Millisecond
	return o
}

func supervisorForTest(t *testing.T, probe func(context.Context) error, factory PersistenceConsumerFactory) *PersistenceSupervisor {
	t.Helper()
	s := newPersistenceSupervisor(supervisorTestOptions(), factory, probe, func(group PersistenceConsumerGroup) (*PersistenceService, error) {
		return group.(*supervisorTestGroup).service, nil
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.Close(ctx); errors.Is(err, ErrPersistenceSupervisorPending) {
			t.Error("supervisor cleanup still pending", err)
		}
	})
	return s
}

func supervisorStart(t *testing.T, s *PersistenceSupervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Start(ctx, context.Background()); err != nil {
		t.Fatal(err)
	}
}

type supervisorWait struct {
	delays chan time.Duration
	next   chan struct{}
}

func supervisorControlledWait(s *PersistenceSupervisor) *supervisorWait {
	w := &supervisorWait{delays: make(chan time.Duration, 4), next: make(chan struct{}, 4)}
	s.wait = func(ctx context.Context, delay time.Duration) error {
		w.delays <- delay
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.next:
			return ctx.Err()
		}
	}
	return w
}

func (w *supervisorWait) expect(t *testing.T, base, cap time.Duration, failures int) {
	t.Helper()
	ceiling := base
	for i := 1; i < failures; i++ {
		ceiling = min(2*ceiling, cap)
	}
	select {
	case delay := <-w.delays:
		if delay < ceiling/2 || delay > ceiling {
			t.Fatalf("backoff %v outside [%v,%v]", delay, ceiling/2, ceiling)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("backoff was not scheduled")
	}
}

func TestPersistenceSupervisorInitialOutageProbesBeforeAcquiring(t *testing.T) {
	var probes, factories atomic.Int64
	group := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
	s := supervisorForTest(t, func(context.Context) error {
		if probes.Add(1) <= 5 {
			return io.EOF
		}
		return nil
	}, func(context.Context) (PersistenceConsumerGroup, error) {
		factories.Add(1)
		return group, nil
	})
	w := supervisorControlledWait(s)
	supervisorStart(t, s)
	for failures := 1; failures <= 5; failures++ {
		w.expect(t, s.options.BaseDelay, s.options.MaxDelay, failures)
		if got := s.Status(); got.Ready || got.Generation != 0 || got.State != PersistenceStateBackoff || s.Err() != nil {
			t.Fatal("initial outage was not paused", got, s.Err())
		}
		if factories.Load() != 0 || probes.Load() != int64(failures) {
			t.Fatal("database outage opened consumers or spun probes")
		}
		w.next <- struct{}{}
	}
	serviceEventually(t, s.ConsumerReady)
	if got := s.Status(); got.Generation != 1 || got.Failures != 5 || got.LastFailure != FailureInfrastructure {
		t.Fatal("recovery lost generation/backoff evidence", got)
	}
	if err := s.Close(context.Background()); err != nil || group.closes.Load() != 1 || s.Err() != nil {
		t.Fatal("recovered outage leaked into final error or leaked group", err)
	}
}

func TestPersistenceSupervisorWaitsForOldWorkAndGroupBeforeRecovery(t *testing.T) {
	stream := newServiceStream()
	old := &supervisorTestGroup{service: serviceForTest(t, stream)}
	fresh := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	old.service.handle = func(ctx context.Context, _ *rabbitmq.ConsumerDelivery) error {
		close(entered)
		<-release
		return ctx.Err()
	}
	closeEntered, closeRelease := make(chan struct{}), make(chan struct{})
	var closeOnce sync.Once
	defer closeOnce.Do(func() { close(closeRelease) })
	old.close = func(context.Context) error { close(closeEntered); <-closeRelease; return nil }
	var calls atomic.Int64
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) {
		if calls.Add(1) == 1 {
			return old, nil
		}
		return fresh, nil
	})
	w := supervisorControlledWait(s)
	supervisorStart(t, s)
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	serviceAwait(t, entered)
	old.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", io.EOF))
	serviceEventually(t, func() bool { return s.Status().State == PersistenceStateRetiring })
	if s.ConsumerReady() || calls.Load() != 1 || old.closes.Load() != 0 || persistenceDone(old.service.Done()) {
		t.Fatal("recovery overtook admitted SQL/ACK work")
	}
	once.Do(func() { close(release) })
	serviceAwait(t, closeEntered)
	if calls.Load() != 1 || s.ConsumerReady() || persistenceDone(s.Done()) {
		t.Fatal("recovery overtook group cleanup")
	}
	closeOnce.Do(func() { close(closeRelease) })
	w.expect(t, s.options.BaseDelay, s.options.MaxDelay, 1)
	w.next <- struct{}{}
	serviceEventually(t, s.ConsumerReady)
	if got := s.Status(); got.Generation != 2 || calls.Load() != 2 || old.closes.Load() != 1 || old.service.ConsumerReady() {
		t.Fatal("old generation revived or failed to retire", got)
	}
	// A short ready period and a successful Ping must not reset backoff.
	fresh.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", io.EOF))
	w.expect(t, s.options.BaseDelay, s.options.MaxDelay, 2)
	if err := s.Close(context.Background()); err != nil || calls.Load() != 2 || fresh.closes.Load() != 1 {
		t.Fatal("close during backoff restarted a generation", err)
	}
}

func TestPersistenceSupervisorGracefulDrainRetainsAdmittedWork(t *testing.T) {
	stream := newServiceStream()
	g := &supervisorTestGroup{service: serviceForTest(t, stream)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	g.service.handle = func(ctx context.Context, _ *rabbitmq.ConsumerDelivery) error {
		close(entered)
		<-release
		return ctx.Err()
	}
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) { return g, nil })
	supervisorStart(t, s)
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	serviceAwait(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Drain(ctx)
	cancel()
	if !errors.Is(err, ErrPersistenceSupervisorPending) || !errors.Is(err, context.DeadlineExceeded) || s.ConsumerReady() {
		t.Fatal("drain hid admitted work", err)
	}
	once.Do(func() { close(release) })
	if err := s.Drain(context.Background()); err != nil || g.closes.Load() != 0 || stream.closeCalls.Load() != 0 {
		t.Fatal("graceful drain canceled work or released sessions before Close", err)
	}
	if err := s.Close(context.Background()); err != nil || g.closes.Load() != 1 {
		t.Fatal(err)
	}
}

func TestPersistenceSupervisorForcedCloseRetainsOwnershipUntilJoin(t *testing.T) {
	stream := newServiceStream()
	g := &supervisorTestGroup{service: serviceForTest(t, stream)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var operation context.Context
	g.service.handle = func(ctx context.Context, _ *rabbitmq.ConsumerDelivery) error {
		operation = ctx
		close(entered)
		<-release // Deliberately ignores cancellation.
		return ctx.Err()
	}
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) { return g, nil })
	supervisorStart(t, s)
	stream.next <- serviceNext{delivery: &rabbitmq.ConsumerDelivery{}}
	serviceAwait(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(ctx)
	cancel()
	if !errors.Is(err, ErrPersistenceSupervisorPending) || operation.Err() == nil || g.closes.Load() != 0 || persistenceDone(s.Done()) {
		t.Fatal("forced observer timeout released active work", err)
	}
	once.Do(func() { close(release) })
	serviceAwait(t, s.Done())
	if !errors.Is(s.Err(), context.Canceled) || g.closes.Load() != 1 || s.Status().Generation != 1 {
		t.Fatal("forced close lost error or reopened consumers", s.Err())
	}
}

func TestPersistenceSupervisorLateFactoryAndProbeCannotRevive(t *testing.T) {
	for _, stage := range []string{"probe", "factory"} {
		t.Run(stage, func(t *testing.T) {
			g := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			var calls atomic.Int64
			s := supervisorForTest(t, func(context.Context) error {
				if stage == "probe" {
					close(entered)
					<-release
				}
				return nil
			}, func(context.Context) (PersistenceConsumerGroup, error) {
				calls.Add(1)
				if stage == "factory" {
					close(entered)
					<-release
				}
				return g, nil
			})
			setup, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			started := make(chan error, 1)
			go func() { started <- s.Start(setup, context.Background()) }()
			serviceAwait(t, entered)
			ctx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			err := s.Close(ctx)
			stop()
			if !errors.Is(err, ErrPersistenceSupervisorPending) || persistenceDone(s.Done()) {
				t.Fatal("late callback was detached", err)
			}
			once.Do(func() { close(release) })
			if err := <-started; !errors.Is(err, context.Canceled) {
				t.Fatal("canceled startup succeeded", err)
			}
			serviceAwait(t, s.Done())
			if s.ConsumerReady() || g.service.started || (stage == "probe" && calls.Load() != 0) || g.closes.Load() != calls.Load() {
				t.Fatal("late success acquired/activated or leaked a group")
			}
		})
	}
}

func TestPersistenceSupervisorCloseDuringRetirementDoesNotDetachCloser(t *testing.T) {
	g := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	g.close = func(context.Context) error { close(entered); <-release; return nil }
	s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) { return g, nil })
	supervisorStart(t, s)
	g.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", io.EOF))
	serviceAwait(t, entered)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	err := s.Close(ctx)
	cancel()
	if !errors.Is(err, ErrPersistenceSupervisorPending) || persistenceDone(s.Done()) || s.Status().Generation != 1 {
		t.Fatal("group cleanup was detached", err)
	}
	once.Do(func() { close(release) })
	serviceAwait(t, s.Done())
	if s.Err() != nil || g.closes.Load() != 1 {
		t.Fatal("retired group restarted or cleanup repeated", s.Err())
	}
}

func TestPersistenceSupervisorTerminalFailuresNeverReconnect(t *testing.T) {
	for name, failure := range map[string]error{
		"malformed":   &PersistenceError{Kind: InvalidDelivery, cause: io.EOF},
		"constraint":  &PersistenceError{Kind: PersistenceFailed, cause: &mysql.MySQLError{Number: 3819}},
		"exhausted":   &PersistenceError{Kind: PersistenceFailed, cause: errors.Join(ErrPersistenceRetriesExhausted, &mysql.MySQLError{Number: 1205})},
		"ack":         &PersistenceError{Kind: AcknowledgementFailed, cause: io.EOF},
		"unknown":     errors.New("private raw payload"),
		"mixed":       errors.Join(io.EOF, &PersistenceError{Kind: InvalidDelivery}),
		"commit_only": database.ErrCommandCommitUnknown,
	} {
		t.Run(name, func(t *testing.T) {
			g := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
			s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) { return g, nil })
			supervisorStart(t, s)
			g.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "process", failure))
			serviceAwait(t, s.Done())
			if !errors.Is(s.Err(), ErrPersistenceSupervisorFailed) || !errors.Is(s.Err(), failure) || s.Status().Generation != 1 || s.ConsumerReady() || g.closes.Load() != 1 {
				t.Fatal("terminal failure restarted or lost cause", s.Err())
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", s.Err(), s.Err()), "private raw") {
				t.Fatal("failure formatting exposed a raw cause")
			}
		})
	}
}

func TestPersistenceSupervisorPartialAcquisitionAndCleanupFailure(t *testing.T) {
	for _, mode := range []string{"partial", "close", "panic"} {
		t.Run(mode, func(t *testing.T) {
			g := &supervisorTestGroup{service: serviceForTest(t, newServiceStream())}
			cause := errors.New("private credential")
			if mode == "close" {
				g.close = func(context.Context) error { return cause }
			} else if mode == "panic" {
				g.close = func(context.Context) error { panic("private credential") }
			}
			s := supervisorForTest(t, func(context.Context) error { return nil }, func(context.Context) (PersistenceConsumerGroup, error) {
				if mode == "partial" {
					return g, cause
				}
				return g, nil
			})
			setup, cancel := context.WithTimeout(context.Background(), time.Second)
			err := s.Start(setup, context.Background())
			cancel()
			if mode == "partial" {
				if !errors.Is(err, cause) {
					t.Fatal("partial factory error lost", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				g.service.fail(persistenceServiceError(ErrPersistenceServiceStopped, "storage", io.EOF))
			}
			serviceAwait(t, s.Done())
			if g.closes.Load() != 1 || s.Status().Generation != 1 || !errors.Is(s.Err(), ErrPersistenceSupervisorFailed) {
				t.Fatal("partial group leaked or cleanup failure recovered", s.Err())
			}
			if strings.Contains(fmt.Sprintf("%v %#v", s, s.Err()), "private credential") {
				t.Fatal("cleanup formatting exposed a raw cause")
			}
		})
	}
}

func TestPersistenceSupervisorRecoveryRequiresCompleteInfrastructureEvidence(t *testing.T) {
	storage := func(err error) error { return persistenceServiceError(ErrPersistenceServiceStopped, "storage", err) }
	for name, tc := range map[string]struct {
		err error
		ok  bool
	}{
		"connection":                  {io.EOF, true},
		"wrapped_connection":          {fmt.Errorf("context: %w", io.EOF), true},
		"storage_deadline":            {storage(context.DeadlineExceeded), true},
		"processing_deadline":         {persistenceServiceError(ErrPersistenceServiceStopped, "process", context.DeadlineExceeded), false},
		"storage_cancel":              {storage(context.Canceled), false},
		"commit_connection":           {&PersistenceError{Kind: CommitUnknown, cause: errors.Join(database.ErrCommandCommitUnknown, io.EOF)}, true},
		"rollback_connection":         {&PersistenceError{Kind: PersistenceFailed, cause: errors.Join(database.ErrCommandRollbackUnknown, io.EOF)}, true},
		"commit_constraint":           {errors.Join(database.ErrCommandCommitUnknown, &mysql.MySQLError{Number: 3819}), false},
		"infrastructure_with_unknown": {errors.Join(io.EOF, errors.New("unknown")), false},
		"infrastructure_with_cancel":  {errors.Join(io.EOF, context.Canceled), false},
		"infrastructure_with_ack":     {errors.Join(io.EOF, &PersistenceError{Kind: AcknowledgementFailed}), false},
		"cleanup":                     {errors.Join(io.EOF, persistenceServiceError(ErrPersistenceServiceClose, "subscription", io.EOF)), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := persistenceRecoveryAllowed(tc.err); got != tc.ok {
				t.Fatalf("recovery allowed=%v, want %v", got, tc.ok)
			}
		})
	}
}

func TestPersistenceSupervisorConstructionAndConcurrentClose(t *testing.T) {
	o := DefaultPersistenceSupervisorOptions()
	var calls atomic.Int64
	factory := func(context.Context) (PersistenceConsumerGroup, error) { calls.Add(1); return nil, nil }
	s, err := NewPersistenceSupervisor(&sql.DB{}, factory, o)
	if err != nil || calls.Load() != 0 || s.Status().State != PersistenceStateIdle || s.ConsumerReady() {
		t.Fatal("constructor performed I/O or activated", err)
	}
	if _, err := NewPersistenceSupervisor(nil, factory, o); !errors.Is(err, ErrPersistenceSupervisorInput) {
		t.Fatal("nil pool accepted")
	}
	if _, err := NewPersistenceSupervisor(&sql.DB{}, nil, o); !errors.Is(err, ErrPersistenceSupervisorInput) {
		t.Fatal("nil factory accepted")
	}
	for _, change := range []func(*PersistenceSupervisorOptions){
		func(o *PersistenceSupervisorOptions) { o.BaseDelay = time.Millisecond },
		func(o *PersistenceSupervisorOptions) { o.MaxDelay = o.BaseDelay - 1 },
		func(o *PersistenceSupervisorOptions) { o.MaxDelay = time.Minute + 1 },
		func(o *PersistenceSupervisorOptions) { o.SetupTimeout = 0 },
		func(o *PersistenceSupervisorOptions) { o.ResetAfter = 0 },
		func(o *PersistenceSupervisorOptions) { o.Worker.Retry.MaxAttempts = 99 },
	} {
		bad := o
		change(&bad)
		if ValidatePersistenceSupervisorOptions(bad) == nil {
			t.Fatal("invalid supervisor options accepted")
		}
	}
	if s.Start(context.Background(), context.Background()) != ErrPersistenceSupervisorInput || s.Start(nil, nil) != ErrPersistenceSupervisorInput {
		t.Fatal("unbounded/nil setup accepted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.StopAdmission()
			if err := s.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	serviceAwait(t, s.Done())
	if s.Status().State != PersistenceStateStopped || calls.Load() != 0 {
		t.Fatal("idle close activated or changed final state", s.Status())
	}
	setup, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if s.Start(setup, context.Background()) != ErrPersistenceSupervisorStart {
		t.Fatal("stopped supervisor restarted")
	}
}
