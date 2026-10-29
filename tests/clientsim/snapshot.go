package clientsim

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"

	"iChat/messaging"
)

const MaxSnapshotBytes = 16 * 1024 * 1024
const snapshotVersion = 1

type senderSnapshot struct {
	Version  int            `json:"version"`
	SenderID string         `json:"senderId"`
	Entries  []PendingEntry `json:"entries"`
}

func encodeSnapshot(user uint64, entries map[string]PendingEntry) ([]byte, error) {
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	snapshot := senderSnapshot{Version: snapshotVersion, SenderID: strconv.FormatUint(user, 10), Entries: make([]PendingEntry, 0, len(entries))}
	for _, key := range keys {
		snapshot.Entries = append(snapshot.Entries, entries[key])
	}
	body, err := json.Marshal(snapshot)
	if err != nil || len(body) > MaxSnapshotBytes || len(entries) > MaxPendingEntries {
		return nil, ErrCapacity
	}
	return body, nil
}

func decodeSnapshot(user uint64, body []byte) (map[string]PendingEntry, error) {
	entries := make(map[string]PendingEntry)
	if len(body) == 0 {
		return entries, nil
	}
	if len(body) > MaxSnapshotBytes {
		return nil, ErrSnapshot
	}
	var snapshot senderSnapshot
	if json.Unmarshal(body, &snapshot) != nil || snapshot.Version != snapshotVersion || snapshot.SenderID != strconv.FormatUint(user, 10) || snapshot.Entries == nil || len(snapshot.Entries) > MaxPendingEntries {
		return nil, ErrSnapshot
	}
	previous := ""
	for _, e := range snapshot.Entries {
		c, err := messaging.Normalize(user, e.Request)
		if err != nil || c.ClientMessageID != e.ClientMessageID || c.CommandID != e.CommandID || c.PayloadHash != e.PayloadHash || c.ClientMessageID <= previous || !validEntry(user, e) {
			return nil, ErrSnapshot
		}
		previous = c.ClientMessageID
		entries[c.ClientMessageID] = e
	}
	// This private format is the exact canonical JSON emitted by encodeSnapshot.
	// Re-encoding rejects duplicate/unknown/null fields, noncanonical strings,
	// invalid UTF-8/UTF-16 replacements and trailing data without a second parser.
	canonical, err := encodeSnapshot(user, entries)
	if err != nil || !bytes.Equal(body, canonical) {
		return nil, ErrSnapshot
	}
	return entries, nil
}

func validEntry(user uint64, e PendingEntry) bool {
	if e.Attempts == 0 && (e.InFlight || e.State == Submitting || e.State == Accepted || e.State == NotPublished) {
		return false
	}
	switch e.State {
	case Queued:
		return e.Attempts == 0 && !e.InFlight && e.Result == nil
	case Submitting:
		return e.InFlight && e.Result == nil
	case Accepted, NotPublished:
		return e.Result == nil
	case PublishUnknown:
		if e.Result == nil {
			return e.Attempts > 0
		}
		if e.Result.State != messaging.StatusPendingOrUnknown {
			return false
		}
	case Persisted, Rejected, Quarantined, Conflict:
		if e.Result == nil || string(e.Result.State) != string(e.State) {
			return false
		}
	default:
		return false
	}
	c, err := messaging.Normalize(user, e.Request)
	if err != nil {
		return false
	}
	q, err := messaging.NormalizeStatusQuery(user, c.ClientMessageID, e.PayloadHash)
	return err == nil && e.Result.ValidateFor(q) == nil
}
