package messaging

import (
	"math"
	"strings"
	"testing"
)

func TestDeliveryACKClientFrames(t *testing.T) {
	for _, seq := range []string{"0", "1", "18446744073709551615"} {
		kind, ack, err := DecodeClientFrame([]byte(`{"type":"ack","data":{"conversationId":"p:1:2","deliveredSeq":"` + seq + `"}}`))
		if err != nil || kind != "ack" || ack.ConversationID != "p:1:2" || ack.DeliveredSeq.String() != seq {
			t.Fatal(kind, ack, err)
		}
	}
	_, ack, _ := DecodeClientFrame([]byte(`{"type":"ack","data":{"conversationId":"g:1","deliveredSeq":"18446744073709551615"}}`))
	if ack.DeliveredSeq != Sequence(math.MaxUint64) {
		t.Fatal(ack)
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{"type":"ack"}`, `{"type":"ack","data":null}`,
		`{"type":"ack","data":{"conversationId":"p:1:2"}}`,
		`{"type":"ack","data":{"deliveredSeq":"0"}}`,
		`{"type":"ack","data":{"conversationId":"p:2:1","deliveredSeq":"0"}}`,
		`{"type":"ack","data":{"conversationId":"g:01","deliveredSeq":"0"}}`,
		`{"Type":"ack","data":{}}`, `{"type":"ack","type":"ping","data":{}}`,
		`{"type":"ack","sessionId":"forged","data":{}}`,
		`{"type":"ack","data":{"conversationId":"g:1","deliveredSeq":"0","userId":"2"}}`,
		`{"type":"ack","data":{"conversationId":"g:1","deliveredSeq":"0","sessionId":"forged"}}`,
		`{"type":"ack","data":{"conversationId":"g:1","deliveredSeq":"0","deliveredSeq":"1"}}`,
		`{"type":"ack","data":{"conversationId":"g:1","DeliveredSeq":"0"}}`,
		`{"type":"ping","data":{"deliveredSeq":"0"}}`,
		`{"type":"ack.confirmed","data":{}}`,
	} {
		if k, a, e := DecodeClientFrame([]byte(body)); e == nil || k != "" || a != (DeliveryACK{}) {
			t.Fatalf("accepted %s", body)
		}
	}
	for _, raw := range []string{`0`, `null`, `""`, `"01"`, `"-1"`, `"+1"`, `"1.0"`, `"1e2"`, `"18446744073709551616"`} {
		body := `{"type":"ack","data":{"conversationId":"g:1","deliveredSeq":` + raw + `}}`
		if _, _, err := DecodeClientFrame([]byte(body)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	if _, _, err := DecodeClientFrame([]byte(strings.Repeat(" ", MaxBodyBytes) + `{}`)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	for _, kind := range []string{"ping", "pong"} {
		if k, a, err := DecodeClientFrame([]byte(`{"type":"` + kind + `","data":{}}`)); err != nil || k != kind || a != (DeliveryACK{}) {
			t.Fatal(k, a, err)
		}
	}
}
