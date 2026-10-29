package messaging

import "encoding/json"

// DeliveryACK contains no user or session supplied by the client. Those come
// exclusively from the authenticated, current physical WebSocket connection.
type DeliveryACK struct {
	ConversationID string   `json:"conversationId"`
	DeliveredSeq   Sequence `json:"deliveredSeq"`
}

// DecodeClientFrame accepts only v1 heartbeat and delivery ACK frames.
func DecodeClientFrame(body []byte) (kind string, ack DeliveryACK, err error) {
	var frame struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err = decodeObject(body, &frame); err != nil {
		return "", ack, err
	}
	switch frame.Type {
	case "ping", "pong":
		err = decodeObject(frame.Data, &struct{}{})
	case "ack":
		// A pointer distinguishes omitted deliveredSeq from an explicit "0".
		var data struct {
			ConversationID string    `json:"conversationId"`
			DeliveredSeq   *Sequence `json:"deliveredSeq"`
		}
		err = decodeObject(frame.Data, &data)
		if err == nil {
			_, err = ParseConversationID(data.ConversationID)
		}
		if err == nil && data.DeliveredSeq == nil {
			err = invalid("deliveredSeq")
		}
		if err == nil {
			ack = DeliveryACK{data.ConversationID, *data.DeliveredSeq}
		}
	default:
		err = invalid("client frame type")
	}
	if err != nil {
		return "", DeliveryACK{}, err
	}
	return frame.Type, ack, nil
}
