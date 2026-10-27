package messaging

import "encoding/json"

// DecodeHeartbeat accepts only v1 heartbeat data frames. Business ACK and send
// commands have separate trust boundaries; neither is accepted here.
func DecodeHeartbeat(body []byte) (string, error) {
	var frame struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := decodeObject(body, &frame); err != nil {
		return "", err
	}
	if frame.Type != "ping" && frame.Type != "pong" {
		return "", invalid("heartbeat type")
	}
	if err := decodeObject(frame.Data, &struct{}{}); err != nil {
		return "", err
	}
	return frame.Type, nil
}
