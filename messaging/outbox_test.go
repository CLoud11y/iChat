package messaging

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestOutboxEventIdentity(t *testing.T) {
	commandID := "55faac68e13e0a6d5b9562536d3b0c7cf90552416747c9986500b2202fe96fa2"
	// Independent Python hashlib/struct vectors, not a second call to digest.
	for eventType, want := range map[OutboxEventType]string{
		EventMessagePersisted: "de4b88c7c0deffb887f8c4d22bd07c788aee7de11c45b130bc63141fbac6de2d",
		EventCommandResult:    "56f143e224be2614fc13bc2fb656d1ab3e1cb46564871fbbcb85ca509c27ba30",
	} {
		got, err := OutboxEventID(commandID, eventType)
		if err != nil || got != want {
			t.Fatalf("%s id=%s err=%v", eventType, got, err)
		}
		other, err := OutboxEventID(strings.Repeat("0", 64), eventType)
		if err != nil || other == got {
			t.Fatalf("different command shared event identity: %s %v", other, err)
		}
	}
	for _, id := range []string{"", commandID[:63], commandID + "0", strings.ToUpper(commandID), strings.Repeat("z", 64)} {
		if _, err := OutboxEventID(id, EventCommandResult); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid command ID accepted: %v", err)
		}
	}
	if _, err := OutboxEventID(commandID, "unknown"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("invalid event type accepted: %v", err)
	}
}

func TestOutboxPayloadSequenceAndContentBoundary(t *testing.T) {
	hint, err := json.Marshal(MessagePersistedEvent{Seq: Sequence(math.MaxUint64)})
	if err != nil || !strings.Contains(string(hint), `"seq":"18446744073709551615"`) || strings.Contains(string(hint), `"content"`) {
		t.Fatalf("message hint has unsafe sequence/content: %s err=%v", hint, err)
	}
	rejected, err := json.Marshal(CommandResultEvent{State: "rejected", ReasonCode: "NOT_FRIENDS"})
	if err != nil || strings.Contains(string(rejected), `"message"`) || !strings.Contains(string(rejected), `"reasonCode":"NOT_FRIENDS"`) {
		t.Fatalf("rejection payload contains a success: %s err=%v", rejected, err)
	}
}
