package messaging

import "encoding/json"

// DecodeSubmissionResponse and DecodeCommandStatusResponse enforce the same
// exact fields, duplicate/null/Unicode and bounded JSON rules as v1 requests.
// HTTP status/code and logical identity are checked by the calling transport.
func DecodeSubmissionResponse(body []byte) (SubmissionResponse, error) {
	code, msg, data, err := decodeResponse(body)
	if err != nil {
		return SubmissionResponse{}, err
	}
	var submission Submission
	if err := decodeObject(data, &submission); err != nil {
		return SubmissionResponse{}, err
	}
	return SubmissionResponse{Code: code, Msg: msg, Data: submission}, nil
}

func DecodeCommandStatusResponse(body []byte) (CommandStatusResponse, error) {
	code, msg, data, err := decodeResponse(body)
	if err != nil {
		return CommandStatusResponse{}, err
	}
	var value struct {
		ClientMessageID string          `json:"clientMessageId"`
		PayloadHash     string          `json:"payloadHash"`
		State           StatusState     `json:"state"`
		ReasonCode      string          `json:"reasonCode,omitempty"`
		Message         json.RawMessage `json:"message,omitempty"`
		PollAfterMs     int             `json:"pollAfterMs,omitempty"`
	}
	if err := decodeObject(data, &value); err != nil {
		return CommandStatusResponse{}, err
	}
	status := CommandStatus{ClientMessageID: value.ClientMessageID, PayloadHash: value.PayloadHash, State: value.State, ReasonCode: value.ReasonCode, PollAfterMs: value.PollAfterMs}
	if value.Message != nil {
		var message MessageDTO
		if err := decodeObject(value.Message, &message); err != nil {
			return CommandStatusResponse{}, err
		}
		status.Message = &message
	}
	return CommandStatusResponse{Code: code, Msg: msg, Data: status}, nil
}

func decodeResponse(body []byte) (int, string, json.RawMessage, error) {
	var wrapper struct {
		Code *int            `json:"code"`
		Msg  *string         `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := decodeObject(body, &wrapper); err != nil {
		return 0, "", nil, err
	}
	if wrapper.Code == nil || wrapper.Msg == nil || *wrapper.Msg == "" || len(*wrapper.Msg) > 256 || wrapper.Data == nil {
		return 0, "", nil, invalid("response envelope")
	}
	return *wrapper.Code, *wrapper.Msg, wrapper.Data, nil
}
