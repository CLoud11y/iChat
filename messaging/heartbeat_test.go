package messaging

import "testing"

func TestHeartbeatStrictFrames(t *testing.T) {
	for _, body := range []string{`{"type":"ping","data":{}}`, `{"type":"pong","data":{ }}`} {
		if _, e := DecodeHeartbeat([]byte(body)); e != nil {
			t.Fatal(e)
		}
	}
	for _, body := range []string{`{"type":"ack","data":{}}`, `{"type":"ping","type":"pong","data":{}}`, `{"Type":"ping","data":{}}`, `{"type":"ping"}`, `{"type":"ping","data":null}`, `{"type":"ping","data":{"ack":true}}`, `{"type":"ping","data":{}}{}`} {
		if _, e := DecodeHeartbeat([]byte(body)); e == nil {
			t.Fatal("invalid heartbeat accepted")
		}
	}
}
