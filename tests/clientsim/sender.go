// Package clientsim implements the protocol simulator, independently of API
// globals, network transports and frontend storage implementations.
package clientsim

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"iChat/messaging"
)

var (
	ErrInput          = errors.New("invalid simulator input")
	ErrStore          = errors.New("simulator storage unavailable")
	ErrStoreUncertain = errors.New("simulator storage outcome uncertain; reopen required")
	ErrSnapshot       = errors.New("invalid simulator snapshot")
	ErrNotFound       = errors.New("simulator command not found")
	ErrKeyConflict    = errors.New("simulator key already belongs to another request")
	ErrBlocked        = errors.New("simulator command requires reconciliation or user action")
	ErrInFlight       = errors.New("simulator command already submitting")
	ErrStaleAttempt   = errors.New("stale simulator attempt")
	ErrProtocol       = errors.New("invalid simulator result")
	ErrContradiction  = errors.New("contradictory simulator terminal result")
	ErrCapacity       = errors.New("simulator snapshot capacity exhausted")
)

const MaxPendingEntries = 512

// SnapshotStore is exclusively borrowed by one Sender. Load returns nil for a
// new store; Save atomically replaces the whole snapshot before returning nil.
// An error may mean the write committed: the Sender then refuses further work.
// Callbacks must honor ctx, must not call back into Sender, and must not retain
// or mutate input bytes. Sender never closes a borrowed store.
type SnapshotStore interface {
	Load(context.Context) ([]byte, error)
	Save(context.Context, []byte) error
}

type SendState string

const (
	Queued         SendState = "queued"
	Submitting     SendState = "submitting"
	PublishUnknown SendState = "publish_unknown"
	NotPublished   SendState = "not_published"
	Accepted       SendState = "accepted"
	Quarantined    SendState = "quarantined"
	Conflict       SendState = "ID_CONFLICT"
	Persisted      SendState = "persisted"
	Rejected       SendState = "rejected"
)

// PendingEntry keeps the original request, including clientSentAt. Attempts
// counts local submissions, not broker attempts or logical message deliveries.
type PendingEntry struct {
	Request         messaging.SendRequest    `json:"request"`
	ClientMessageID string                   `json:"clientMessageId"`
	CommandID       string                   `json:"commandId"`
	PayloadHash     string                   `json:"payloadHash"`
	State           SendState                `json:"state"`
	Attempts        messaging.Sequence       `json:"attempts"`
	InFlight        bool                     `json:"inFlight"`
	Result          *messaging.CommandStatus `json:"result,omitempty"`
}

func (PendingEntry) String() string     { return "simulator pending entry (content redacted)" }
func (e PendingEntry) GoString() string { return e.String() }

// Attempt is released only after the submitting checkpoint has been saved.
// Transports must check their context again before performing network I/O.
type Attempt struct {
	ClientMessageID string
	Number          messaging.Sequence
	Request         messaging.SendRequest
}

func (Attempt) String() string     { return "simulator attempt (content redacted)" }
func (a Attempt) GoString() string { return a.String() }

type Sender struct {
	mu      sync.Mutex
	user    uint64
	store   SnapshotStore
	entries map[string]PendingEntry
	faulted bool
}

func (*Sender) String() string     { return "simulator sender (content redacted)" }
func (s *Sender) GoString() string { return s.String() }

func OpenSender(ctx context.Context, user uint64, store SnapshotStore) (*Sender, error) {
	if ctx == nil || user == 0 || nilStore(store) {
		return nil, ErrInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := store.Load(ctx)
	if err != nil {
		return nil, safeError(ErrStore, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := decodeSnapshot(user, body)
	if err != nil {
		return nil, err
	}
	s := &Sender{user: user, store: store, entries: entries}
	changed := len(body) == 0
	for key, e := range entries {
		if e.InFlight {
			e.InFlight = false
			if e.State == Submitting {
				e.State = PublishUnknown
			}
			entries[key] = e
			changed = true
		}
	}
	if changed {
		if err := s.commit(ctx, entries); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func nilStore(store SnapshotStore) bool {
	if store == nil {
		return true
	}
	v := reflect.ValueOf(store)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Func, reflect.Slice, reflect.Interface, reflect.Chan:
		return v.IsNil()
	}
	return false
}

func (s *Sender) Queue(ctx context.Context, request messaging.SendRequest) (PendingEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return PendingEntry{}, err
	}
	c, err := messaging.Normalize(s.user, request)
	if err != nil {
		return PendingEntry{}, ErrInput
	}
	if e, ok := s.entries[c.ClientMessageID]; ok {
		if e.Request != request {
			return PendingEntry{}, ErrKeyConflict
		}
		return cloneEntry(e), nil
	}
	if len(s.entries) >= MaxPendingEntries {
		return PendingEntry{}, ErrCapacity
	}
	e := PendingEntry{Request: request, ClientMessageID: c.ClientMessageID, CommandID: c.CommandID, PayloadHash: c.PayloadHash, State: Queued}
	if err := s.put(ctx, c.ClientMessageID, e); err != nil {
		return PendingEntry{}, err
	}
	return cloneEntry(e), nil
}

func (s *Sender) Entries(ctx context.Context) ([]PendingEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(s.entries))
	for key := range s.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	entries := make([]PendingEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, cloneEntry(s.entries[key]))
	}
	return entries, nil
}

func (s *Sender) BeginSubmit(ctx context.Context, key string) (Attempt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return Attempt{}, err
	}
	e, ok := s.entries[key]
	if !ok {
		return Attempt{}, ErrNotFound
	}
	if e.State == Persisted || e.State == Rejected || e.State == Conflict || e.State == Quarantined {
		return Attempt{}, ErrBlocked
	}
	if e.InFlight {
		return Attempt{}, ErrInFlight
	}
	if e.Attempts == ^messaging.Sequence(0) {
		return Attempt{}, ErrCapacity
	}
	e.Attempts++
	e.InFlight = true
	if e.State == Queued || e.State == NotPublished {
		e.State = Submitting
	}
	if err := s.put(ctx, key, e); err != nil {
		return Attempt{}, err
	}
	return Attempt{ClientMessageID: key, Number: e.Attempts, Request: e.Request}, nil
}

// CompleteUnknown is for lost/unusable responses, never proof of non-delivery.
func (s *Sender) CompleteUnknown(ctx context.Context, a Attempt) error {
	return s.complete(ctx, a, nil)
}

// CompleteSubmission consumes already-decoded submission evidence. HTTP
// status/code, body limits and strict wire decoding belong to the transport.
func (s *Sender) CompleteSubmission(ctx context.Context, a Attempt, result messaging.Submission) error {
	return s.complete(ctx, a, &result)
}

func (s *Sender) complete(ctx context.Context, a Attempt, result *messaging.Submission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	e, ok := s.entries[a.ClientMessageID]
	if !ok {
		return ErrNotFound
	}
	if !e.InFlight || e.Attempts != a.Number || a.Request != e.Request {
		return ErrStaleAttempt
	}
	next := PublishUnknown
	if result != nil {
		c, _ := messaging.Normalize(s.user, e.Request)
		if !validSubmission(*result, c) {
			return ErrProtocol
		}
		switch result.State {
		case "accepted":
			next = Accepted
		case "not_published", "unroutable", "nacked":
			next = NotPublished
		}
	}
	e.InFlight = false
	if !protected(e.State) && (e.State != Accepted || next == Accepted) {
		e.State = next
		e.Result = nil
	}
	return s.put(ctx, a.ClientMessageID, e)
}

// ApplyStatus also accepts a validated full command.result converted to the
// status DTO by a future transport. Reference-only notifications require a
// status query; they do not fabricate a full locally persisted message.
func (s *Sender) ApplyStatus(ctx context.Context, status messaging.CommandStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	e, ok := s.entries[status.ClientMessageID]
	if !ok {
		return ErrNotFound
	}
	q, err := messaging.NormalizeStatusQuery(s.user, status.ClientMessageID, e.PayloadHash)
	if err != nil || status.ValidateFor(q) != nil {
		return ErrProtocol
	}
	if e.State == Persisted || e.State == Rejected {
		if status.State == messaging.StatusPersisted || status.State == messaging.StatusRejected {
			if !reflect.DeepEqual(e.Result, &status) {
				return ErrContradiction
			}
		}
		return nil
	}
	next := SendState(status.State)
	if status.State == messaging.StatusPendingOrUnknown {
		if e.State == Accepted || e.State == Quarantined || e.State == Conflict {
			return nil
		}
		next = PublishUnknown
	} else if e.State == Conflict && status.State == messaging.StatusQuarantined {
		return nil
	}
	e.State = next
	e.Result = &status
	e = cloneEntry(e)
	return s.put(ctx, status.ClientMessageID, e)
}

func protected(state SendState) bool {
	return state == Persisted || state == Rejected || state == Quarantined || state == Conflict
}

func validSubmission(r messaging.Submission, c messaging.Command) bool {
	if !r.Matches(c) || (r.PublishAttemptID != "" && !messaging.ValidPublishAttemptID(r.PublishAttemptID)) {
		return false
	}
	switch r.State {
	case "accepted":
		return r.ReasonCode == "" && r.PublishAttemptID != ""
	case "publish_unknown":
		return r.ReasonCode == "PUBLISH_UNKNOWN"
	case "not_published":
		return r.ReasonCode == "OVERLOADED" || r.ReasonCode == "DEPENDENCY_UNAVAILABLE"
	case "unroutable":
		return r.ReasonCode == "PUBLISH_UNROUTABLE" && r.PublishAttemptID != ""
	case "nacked":
		return r.ReasonCode == "DEPENDENCY_UNAVAILABLE" && r.PublishAttemptID != ""
	}
	return false
}

func (s *Sender) ready(ctx context.Context) error {
	if ctx == nil {
		return ErrInput
	}
	if s.faulted {
		return ErrStoreUncertain
	}
	return ctx.Err()
}

func (s *Sender) put(ctx context.Context, key string, e PendingEntry) error {
	next := make(map[string]PendingEntry, len(s.entries)+1)
	for k, v := range s.entries {
		next[k] = v
	}
	next[key] = e
	return s.commit(ctx, next)
}
func (s *Sender) commit(ctx context.Context, next map[string]PendingEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := encodeSnapshot(s.user, next)
	if err != nil {
		return err
	}
	// Keep the owner faulted if a storage callback panics and an outer caller
	// recovers; that callback may already have committed its bytes.
	s.faulted = true
	if err := s.store.Save(ctx, body); err != nil {
		return safeError(ErrStoreUncertain, err)
	}
	// A known successful local commit is retained even if ctx expired meanwhile.
	s.entries = next
	s.faulted = false
	return nil
}

func cloneEntry(e PendingEntry) PendingEntry {
	if e.Result != nil {
		r := *e.Result
		e.Result = &r
		if r.Message != nil {
			m := *r.Message
			e.Result.Message = &m
		}
	}
	return e
}

type storageError struct{ kind, cause error }

func (e storageError) Error() string                     { return e.kind.Error() }
func (e storageError) Unwrap() []error                   { return []error{e.kind, e.cause} }
func (e storageError) Format(state fmt.State, verb rune) { fmt.Fprint(state, e.Error()) }
func safeError(kind, cause error) error                  { return storageError{kind, cause} }
