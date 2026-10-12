package messaging

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestStatusQueryNormalization(t *testing.T) {
	clientID := "8FD7C2EF-AE26-4BB5-91A5-32A302BB05F1"
	hash := "55405570ca8bd409cbc5ed2f2b5914c09b4b5141fb4b377ef6d46045550ad591"
	query, err := NormalizeStatusQuery(12, clientID, hash)
	if err != nil || query.SenderID != 12 || query.ClientMessageID != strings.ToLower(clientID) || query.PayloadHash != hash ||
		query.CommandID != "55faac68e13e0a6d5b9562536d3b0c7cf90552416747c9986500b2202fe96fa2" {
		t.Fatalf("status identity=%+v err=%v", query, err)
	}
	if query, err := NormalizeStatusQuery(math.MaxUint64, clientID, hash); err != nil || query.SenderID != math.MaxUint64 {
		t.Fatalf("uint64 sender rejected: %+v %v", query, err)
	}
	for _, tc := range []struct {
		sender       uint64
		client, hash string
	}{
		{0, clientID, hash}, {12, "", hash}, {12, "00000000-0000-0000-0000-000000000000", hash},
		{12, " " + clientID, hash}, {12, clientID, ""}, {12, clientID, hash[:63]}, {12, clientID, hash + "0"},
		{12, clientID, strings.ToUpper(hash)}, {12, clientID, strings.Repeat("g", 64)},
	} {
		if _, err := NormalizeStatusQuery(tc.sender, tc.client, tc.hash); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid query accepted: %v", err)
		}
	}
}

func TestStatusJSONDoesNotInventSuccess(t *testing.T) {
	for _, state := range []StatusState{StatusConflict, StatusQuarantined, StatusPendingOrUnknown} {
		body, err := json.Marshal(CommandStatus{ClientMessageID: "request-key", PayloadHash: "requested-hash", State: state})
		if err != nil || strings.Contains(string(body), `"message"`) || strings.Contains(string(body), `"reasonCode"`) || strings.Contains(string(body), `"pollAfterMs"`) {
			t.Fatalf("status=%s body=%s err=%v", state, body, err)
		}
	}
}
