package messaging

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func syncPageMessage(t *testing.T, key int, seq Sequence) MessageDTO {
	t.Helper()
	c, e := Normalize(1, SendRequest{ClientMessageID: fmt.Sprintf("12345678-1234-4234-8234-%012d", key), TargetType: Private, TargetID: "2", Media: Text, Content: "canonical-sync"})
	if e != nil {
		t.Fatal(e)
	}
	m, e := NewMessageDTO(c, seq, time.Date(2026, 10, 30, 0, 0, 0, 123456000, time.UTC))
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestSyncPageValidationAndUnsignedBoundary(t *testing.T) {
	for _, after := range []Sequence{0, Sequence(math.MaxUint64 - 2)} {
		q, e := NormalizePageQuery(1, "p:1:2", after.String(), 2)
		if e != nil {
			t.Fatal(e)
		}
		p := SyncPage{ConversationID: q.Conversation.ID, AfterSeq: after, Items: []MessageDTO{syncPageMessage(t, 1, after+1), syncPageMessage(t, 2, after+2)}, NextAfterSeq: after + 2, HighWatermark: after + 2}
		if e := p.ValidateFor(q); e != nil {
			t.Fatal(e)
		}
		b, e := json.Marshal(p)
		if e != nil || after > 0 && !strings.Contains(string(b), `"nextAfterSeq":"18446744073709551615"`) {
			t.Fatal(string(b), e)
		}
	}
	for _, after := range []Sequence{0, Sequence(math.MaxUint64)} {
		q, _ := NormalizePageQuery(1, "p:1:2", after.String(), 1)
		p := SyncPage{ConversationID: q.Conversation.ID, AfterSeq: after, NextAfterSeq: after, HighWatermark: after, Items: []MessageDTO{}}
		if e := p.ValidateFor(q); e != nil {
			t.Fatal(e)
		}
	}
	q, _ := NormalizePageQuery(1, "p:1:2", "", 1)
	p := SyncPage{ConversationID: "p:1:2", Items: []MessageDTO{syncPageMessage(t, 1, 1)}, NextAfterSeq: 1, HighWatermark: 2, HasMore: true}
	if e := p.ValidateFor(q); e != nil {
		t.Fatal(e)
	}
}
func TestSyncPageRejectsPartialAndContradictoryResults(t *testing.T) {
	for _, mode := range []string{"bad_query", "wrong_reader", "nil_items", "wrong_conversation", "wrong_after", "watermark_behind", "wrong_next", "empty_jump", "short_page", "wrong_more", "gap", "duplicate_id", "duplicate_seq", "reverse", "wrong_route", "bad_id", "bad_content", "bad_media", "bad_time", "noncanonical_seq"} {
		t.Run(mode, func(t *testing.T) {
			q, _ := NormalizePageQuery(1, "p:1:2", "0", 2)
			p := SyncPage{ConversationID: "p:1:2", Items: []MessageDTO{syncPageMessage(t, 1, 1), syncPageMessage(t, 2, 2)}, NextAfterSeq: 2, HighWatermark: 2}
			switch mode {
			case "bad_query":
				q.Limit = 0
			case "wrong_reader":
				q.ReaderID = 3
			case "nil_items":
				p.Items = nil
			case "wrong_conversation":
				p.ConversationID = "g:2"
			case "wrong_after":
				p.AfterSeq = 1
			case "watermark_behind":
				q.Cursor = 3
				p.AfterSeq = 3
			case "wrong_next":
				p.NextAfterSeq = 1
			case "empty_jump":
				p.Items = []MessageDTO{}
				p.NextAfterSeq = 2
			case "short_page":
				p.Items = p.Items[:1]
				p.NextAfterSeq = 1
			case "wrong_more":
				p.HasMore = true
			case "gap":
				p.Items[1].Seq = 3
			case "duplicate_id":
				p.Items[1] = p.Items[0]
				p.Items[1].Seq = 2
			case "duplicate_seq":
				p.Items[1].Seq = 1
			case "reverse":
				p.Items[0], p.Items[1] = p.Items[1], p.Items[0]
			case "wrong_route":
				p.Items[1].ConversationID = "p:2:3"
			case "bad_id":
				p.Items[1].MessageID = strings.Repeat("f", 64)
			case "bad_content":
				p.Items[1].Content = ""
			case "bad_media":
				p.Items[1].Media = "video"
			case "bad_time":
				p.Items[1].ServerCreatedAt = "2026-10-30"
			case "noncanonical_seq":
				p.Items[0].Seq = 0
			}
			if e := p.ValidateFor(q); e == nil {
				t.Fatal("accepted contradictory page", mode)
			}
		})
	}
}
