package clientsim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type failAcceptedStore struct {
	SnapshotStore
	cause error
}

func (s failAcceptedStore) Save(ctx context.Context, body []byte) error {
	if err := s.SnapshotStore.Save(ctx, body); err != nil {
		return err
	}
	entries, err := decodeSnapshot(12, body)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.State == Accepted {
			return s.cause
		}
	}
	return nil
}

func TestHTTPRecoveryUnknownStorageStopsNetwork(t *testing.T) {
	backing := &checkpointStore{}
	cause := errors.New("PRIVATE_SAVE_RESULT_LOST")
	s, err := OpenSender(context.Background(), 12, failAcceptedStore{backing, cause})
	if err != nil {
		t.Fatal(err)
	}
	e := simQueue(t, s, 1)
	accepted := acceptedReply(t)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { requests.Add(1); simReply(w, 202, accepted) }))
	defer server.Close()
	r := simHTTPRunner(t, s, server.URL)
	r.wait = func(context.Context, time.Duration) error { t.Error("storage failure entered HTTP retry"); return nil }
	err = r.Recover(context.Background(), e.ClientMessageID, fastRecovery())
	if !errors.Is(err, ErrStoreUncertain) || !errors.Is(err, cause) || requests.Load() != 1 {
		t.Fatal("uncertain storage retried HTTP", err)
	}
	if _, err := s.Entries(context.Background()); !errors.Is(err, ErrStoreUncertain) {
		t.Fatal(err)
	}
	r.Close()
	reopened, err := OpenSender(context.Background(), 12, backing)
	if err != nil || simEntry(t, reopened).State != Accepted || simEntry(t, reopened).Request != e.Request {
		t.Fatal("known HTTP response not recoverable", err)
	}
}
