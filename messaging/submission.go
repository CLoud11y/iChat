package messaging

// Submission describes one HTTP publish attempt, never a persisted/delivered
// terminal result. Error responses keep the original logical identity when the
// request could be normalized; attempt identity is omitted before allocation.
type Submission struct {
	State            string `json:"state,omitempty"`
	CommandID        string `json:"commandId,omitempty"`
	ClientMessageID  string `json:"clientMessageId,omitempty"`
	PayloadHash      string `json:"payloadHash,omitempty"`
	PublishAttemptID string `json:"publishAttemptId,omitempty"`
	ReasonCode       string `json:"reasonCode,omitempty"`
}

type SubmissionResponse struct {
	Code int        `json:"code"`
	Msg  string     `json:"msg"`
	Data Submission `json:"data"`
}

// ValidPublishAttemptID accepts only the canonical wire spelling. It validates
// syntax, not broker receipt; only the publisher's complete result proves that.
func ValidPublishAttemptID(value string) bool {
	normal, err := normalizeUUID(value)
	return err == nil && normal == value
}

func (s Submission) Matches(command Command) bool {
	return s.CommandID == command.CommandID && s.ClientMessageID == command.ClientMessageID && s.PayloadHash == command.PayloadHash
}
