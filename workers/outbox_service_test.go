package workers

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"iChat/database"
	"iChat/messaging"
)

func outboxServiceTestOptions() OutboxServiceOptions {
	o := DefaultOutboxServiceOptions()
	o.Loop = outboxTestOptions()
	o.ProbeInterval = 10 * time.Millisecond
	o.ProbeTimeout = 10 * time.Millisecond
	return o
}
func awaitOutbox(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("outbox callback did not join")
	}
}
func startOutbox(t *testing.T, s *OutboxService) {
	t.Helper()
	c, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.Start(c, context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOutboxServiceGracefulAndPendingOwnership(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "drain", true: "force"}[force], func(t *testing.T) {
			l := outboxTestLease(t)
			entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var claims, completes, closes atomic.Int32
			store := &outboxTestStore{claim: func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) {
				claims.Add(1)
				return []database.OutboxLease{l}, nil
			}, complete: func(c context.Context, l database.OutboxLease) (database.OutboxSettlement, error) {
				completes.Add(1)
				return outboxTestReceipt(l, true), nil
			}}
			publisher := notificationPublisherFunc(func(c context.Context, _ messaging.Notification) error {
				close(entered)
				<-release
				if !force && c.Err() != nil {
					t.Error("graceful stop canceled admitted send")
				}
				return c.Err()
			})
			s, _ := NewOutboxService(func(context.Context) (OutboxResources, error) {
				return OutboxResources{Store: store, Publisher: publisher, ProbeStorage: func(context.Context) error { return nil }, ProbeNotification: func(context.Context) error { return nil }, Close: func() error { closes.Add(1); close(closed); return nil }}, nil
			}, outboxServiceTestOptions())
			startOutbox(t, s)
			awaitOutbox(t, entered)
			s.StopAdmission()
			if s.OutboxReady() {
				t.Fatal("stop retained readiness")
			}
			c, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
			var err error
			if force {
				err = s.Close(c)
			} else {
				err = s.Drain(c)
			}
			stop()
			if !errors.Is(err, ErrOutboxServicePending) || closes.Load() != 0 {
				t.Fatal("observer released owned resources", err)
			}
			select {
			case <-s.Done():
				t.Fatal("Done closed with blocked publication")
			default:
			}
			close(release)
			c, stop = context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := s.CloseAfterDrainForTest(c, force); err != nil {
				t.Fatal(err)
			}
			awaitOutbox(t, closed)
			want := int32(1)
			if force {
				want = 0
			}
			if completes.Load() != want || claims.Load() != 1 || closes.Load() != 1 {
				t.Fatal("wrong admission/cleanup", completes.Load(), claims.Load(), closes.Load())
			}
		})
	}
}

// Keep the graceful test's processing lifetime intact until it joins.
func (s *OutboxService) CloseAfterDrainForTest(c context.Context, force bool) error {
	if force {
		return s.Close(c)
	}
	return s.Drain(c)
}

func TestOutboxServiceStartupLateSourceAndCloseJoin(t *testing.T) {
	for _, stage := range []string{"source", "probe", "close"} {
		t.Run(stage, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var closes atomic.Int32
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			block := func() { close(entered); <-release }
			r := OutboxResources{Store: &outboxTestStore{claim: func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) { return nil, nil }}, Publisher: notificationPublisherFunc(func(context.Context, messaging.Notification) error { return nil }), ProbeStorage: func(context.Context) error {
				if stage == "probe" {
					block()
				}
				return nil
			}, ProbeNotification: func(context.Context) error { return nil }, Close: func() error {
				if stage == "close" {
					block()
				}
				closes.Add(1)
				return nil
			}}
			s, _ := NewOutboxService(func(context.Context) (OutboxResources, error) {
				if stage == "source" {
					block()
				}
				return r, nil
			}, outboxServiceTestOptions())
			setup, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
			started := make(chan error, 1)
			go func() { started <- s.Start(setup, context.Background()) }()
			if stage == "close" {
				if err := <-started; err != nil {
					t.Fatal(err)
				}
				s.StopAdmission()
				awaitOutbox(t, entered)
			} else {
				awaitOutbox(t, entered)
				if err := <-started; !errors.Is(err, ErrOutboxServiceStart) {
					t.Fatal(err)
				}
			}
			stop()
			c, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			err := s.Close(c)
			cancel()
			if !errors.Is(err, ErrOutboxServicePending) || closes.Load() != 0 {
				t.Fatal("late callback lost owner", err)
			}
			select {
			case <-s.Done():
				t.Fatal("premature Done")
			default:
			}
			close(release)
			awaitOutbox(t, s.Done())
			if closes.Load() != 1 || s.OutboxReady() {
				t.Fatal("cleanup did not join")
			}
		})
	}
}

func TestOutboxServiceIdleFailStopAndFreshness(t *testing.T) {
	for _, stage := range []string{"idle", "source_error", "probe_error", "claim_error", "probe_panic", "close_error", "freshness"} {
		t.Run(stage, func(t *testing.T) {
			var sources, probes, closes atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			source := func(context.Context) (OutboxResources, error) {
				sources.Add(1)
				r := OutboxResources{Store: &outboxTestStore{claim: func(context.Context, database.OutboxClaimOptions) ([]database.OutboxLease, error) {
					if stage == "claim_error" {
						return nil, database.ErrOutboxCommitUnknown
					}
					if stage == "freshness" {
						close(entered)
						<-release
					}
					return nil, nil
				}}, Publisher: notificationPublisherFunc(func(context.Context, messaging.Notification) error { return nil }), ProbeStorage: func(context.Context) error {
					n := probes.Add(1)
					if stage == "probe_error" && n > 1 {
						return errors.New("SECRET")
					}
					if stage == "probe_panic" {
						panic("SECRET")
					}
					return nil
				}, ProbeNotification: func(context.Context) error { return nil }, Close: func() error {
					closes.Add(1)
					if stage == "close_error" {
						return errors.New("SECRET")
					}
					return nil
				}}
				if stage == "source_error" {
					return r, errors.New("SECRET")
				}
				return r, nil
			}
			s, err := NewOutboxService(source, outboxServiceTestOptions())
			if err != nil || s.OutboxReady() || sources.Load() != 0 {
				t.Fatal("constructor performed I/O", err)
			}
			if stage == "idle" {
				if err := s.Close(context.Background()); err != nil {
					t.Fatal(err)
				}
				if sources.Load() != 0 {
					t.Fatal("idle close opened source")
				}
				return
			}
			setup, stop := context.WithTimeout(context.Background(), time.Second)
			err = s.Start(setup, context.Background())
			stop()
			if stage == "source_error" || stage == "probe_panic" {
				if !errors.Is(err, ErrOutboxServiceStart) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if stage == "freshness" {
				awaitOutbox(t, entered)
				s.mu.Lock()
				s.checked = time.Now().Add(-time.Second)
				s.mu.Unlock()
				if s.OutboxReady() {
					t.Fatal("stale probe retained readiness")
				}
				s.StopAdmission()
				close(release)
			}
			if stage == "close_error" {
				s.StopAdmission()
			}
			awaitOutbox(t, s.Done())
			if closes.Load() != 1 || s.OutboxReady() {
				t.Fatal("failure retained resources/readiness")
			}
			if e := s.Err(); e != nil && strings.Contains(e.Error(), "SECRET") {
				t.Fatal("error leaks dependency details")
			}
			if stage != "freshness" && s.Err() == nil {
				t.Fatal("dependency failure was hidden")
			}
			if stage == "claim_error" && !errors.Is(s.Err(), database.ErrOutboxCommitUnknown) {
				t.Fatal("commit uncertainty lost")
			}
		})
	}
}

func TestOutboxServiceRejectsInvalidLifecycleInputs(t *testing.T) {
	for _, o := range []OutboxServiceOptions{{}, {Loop: outboxTestOptions(), ProbeInterval: time.Second}, {Loop: outboxTestOptions(), ProbeInterval: time.Hour, ProbeTimeout: time.Second}} {
		if _, err := NewOutboxService(func(context.Context) (OutboxResources, error) { panic("invalid options called source") }, o); !errors.Is(err, ErrOutboxServiceInput) {
			t.Fatal(err)
		}
	}
	if _, err := NewOutboxService(nil, DefaultOutboxServiceOptions()); err == nil {
		t.Fatal("nil source accepted")
	}
	var closes atomic.Int32
	s, _ := NewOutboxService(func(context.Context) (OutboxResources, error) {
		return OutboxResources{Store: (*outboxTestStore)(nil), Close: func() error { closes.Add(1); return nil }}, nil
	}, outboxServiceTestOptions())
	if err := s.Start(context.Background(), context.Background()); !errors.Is(err, ErrOutboxServiceInput) {
		t.Fatal("unbounded setup accepted", err)
	}
	if err := s.Drain(nil); !errors.Is(err, ErrOutboxServiceInput) {
		t.Fatal(err)
	}
	if err := s.Close(nil); !errors.Is(err, ErrOutboxServiceInput) {
		t.Fatal(err)
	}
	setup, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.Start(setup, context.Background()); !errors.Is(err, ErrOutboxServiceStart) {
		t.Fatal("typed nil resources accepted", err)
	}
	awaitOutbox(t, s.Done())
	if closes.Load() != 1 {
		t.Fatal("partial invalid source not closed")
	}
	if err := s.Start(setup, context.Background()); !errors.Is(err, ErrOutboxServiceStart) {
		t.Fatal("service reused", err)
	}
}
