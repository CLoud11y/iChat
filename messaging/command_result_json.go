package messaging

import "encoding/json"

// DecodeCommandResult validates an untrusted command.result frame for the
// local authenticated sender. Its event ID is derived from the logical command;
// a reference-only persisted result is not a complete MessageDTO.
func DecodeCommandResult(body []byte, sender uint64) (CommandResultData, error) {
	var wrapper struct {
		Type    string          `json:"type"`
		EventID string          `json:"eventId"`
		Data    json.RawMessage `json:"data"`
	}
	if err := decodeObject(body, &wrapper); err != nil {
		return CommandResultData{}, err
	}
	if wrapper.Type != "command.result" || wrapper.Data == nil {
		return CommandResultData{}, invalid("command result frame")
	}
	var value struct {
		ClientMessageID string          `json:"clientMessageId"`
		PayloadHash     string          `json:"payloadHash"`
		State           string          `json:"state"`
		ReasonCode      string          `json:"reasonCode,omitempty"`
		MessageID       string          `json:"messageId,omitempty"`
		ConversationID  string          `json:"conversationId,omitempty"`
		Seq             *Sequence       `json:"seq,omitempty"`
		Message         json.RawMessage `json:"message,omitempty"`
	}
	if err := decodeObject(wrapper.Data, &value); err != nil {
		return CommandResultData{}, err
	}
	result := CommandResultData{ClientMessageID: value.ClientMessageID, PayloadHash: value.PayloadHash, State: value.State, ReasonCode: value.ReasonCode, MessageID: value.MessageID, ConversationID: value.ConversationID, Seq: value.Seq}
	if value.Message != nil {
		var message MessageDTO
		if err := decodeObject(value.Message, &message); err != nil {
			return CommandResultData{}, err
		}
		result.Message = &message
	}
	frame, err := NewCommandResultFrame(sender, result)
	if err != nil || wrapper.EventID != frame.EventID() {
		return CommandResultData{}, invalid("command result identity")
	}
	return result, nil
}
