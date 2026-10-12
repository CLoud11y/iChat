package messaging

import "strconv"

type StatusState string

const (
	StatusPersisted          StatusState = "persisted"
	StatusRejected           StatusState = "rejected"
	StatusQuarantined        StatusState = "quarantined"
	StatusPendingOrUnknown   StatusState = "pending_or_unknown"
	StatusConflict           StatusState = "ID_CONFLICT"
	DefaultStatusPollAfterMs             = 2000
)

// StatusQuery is derived from the authenticated sender, never a request sender
// field. Repository callers must supply the UID established by authentication.
type StatusQuery struct {
	SenderID        uint64
	ClientMessageID string
	PayloadHash     string
	CommandID       string
}

func NormalizeStatusQuery(authenticatedSender uint64, clientMessageID, payloadHash string) (StatusQuery, error) {
	if authenticatedSender == 0 {
		return StatusQuery{}, invalid("senderId")
	}
	clientID, err := normalizeUUID(clientMessageID)
	if err != nil {
		return StatusQuery{}, invalid("clientMessageId")
	}
	if len(payloadHash) != 64 {
		return StatusQuery{}, invalid("payloadHash")
	}
	for _, char := range payloadHash {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return StatusQuery{}, invalid("payloadHash")
		}
	}
	return StatusQuery{SenderID: authenticatedSender, ClientMessageID: clientID, PayloadHash: payloadHash,
		CommandID: digest("ichat:command:v1\x00", strconv.FormatUint(authenticatedSender, 10), clientID)}, nil
}

// CommandStatus describes this requested hash. Conflict never exposes the
// canonical hash, reason or message. Quarantine carries no raw diagnostic data.
// The HTTP adapter must map StatusConflict to 409; other states use 200.
type CommandStatus struct {
	ClientMessageID string      `json:"clientMessageId"`
	PayloadHash     string      `json:"payloadHash"`
	State           StatusState `json:"state"`
	ReasonCode      string      `json:"reasonCode,omitempty"`
	Message         *MessageDTO `json:"message,omitempty"`
	PollAfterMs     int         `json:"pollAfterMs,omitempty"`
}
