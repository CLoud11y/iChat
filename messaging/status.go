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

// ValidateFor prevents a broken status adapter from exposing another request's
// identity/content or treating an incomplete result as authoritative unknown.
func (s CommandStatus) ValidateFor(query StatusQuery) error {
	if s.ClientMessageID != query.ClientMessageID || s.PayloadHash != query.PayloadHash {
		return invalid("command status identity")
	}
	switch s.State {
	case StatusPersisted:
		if s.Message == nil || s.ReasonCode != "" || s.PollAfterMs != 0 {
			return invalid("persisted status")
		}
		command, err := canonicalFrameMessage(*s.Message)
		if err != nil || command.CommandID != query.CommandID || command.SenderID != strconv.FormatUint(query.SenderID, 10) || command.ClientMessageID != query.ClientMessageID || command.PayloadHash != query.PayloadHash {
			return invalid("persisted status message")
		}
	case StatusRejected:
		if !notificationReason(s.ReasonCode) || s.Message != nil || s.PollAfterMs != 0 {
			return invalid("rejected status")
		}
	case StatusConflict:
		if s.Message != nil || s.ReasonCode != "" || s.PollAfterMs != 0 {
			return invalid("conflict status")
		}
	case StatusPendingOrUnknown, StatusQuarantined:
		if s.Message != nil || s.ReasonCode != "" || s.PollAfterMs != DefaultStatusPollAfterMs {
			return invalid("pending status")
		}
	default:
		return invalid("command status state")
	}
	return nil
}

type CommandStatusResponse struct {
	Code int           `json:"code"`
	Msg  string        `json:"msg"`
	Data CommandStatus `json:"data"`
}
