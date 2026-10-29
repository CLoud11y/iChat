package messaging

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestHistoryPageCursorAndUnsignedBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, cursor                 string
		watermark, first, last, next Sequence
		more                         bool
	}{
		{"latest", "", 3, 2, 3, 2, true},
		{"exclusive", "3", 3, 1, 2, 1, false},
		{"above_watermark", "99", 3, 2, 3, 2, true},
		{"zero", "0", 3, 0, 0, 0, false},
		{"one", "1", 3, 0, 0, 1, false},
		{"uncreated", "", 0, 0, 0, 0, false},
		{"uncreated_above", "99", 0, 0, 0, 99, false},
		{"max_latest", "", math.MaxUint64, math.MaxUint64 - 1, math.MaxUint64, math.MaxUint64 - 1, true},
		{"max_explicit", "18446744073709551615", math.MaxUint64, math.MaxUint64 - 2, math.MaxUint64 - 1, math.MaxUint64 - 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, e := NormalizePageQuery(1, "p:1:2", tc.cursor, 2)
			if e != nil {
				t.Fatal(e)
			}
			p := HistoryPage{ConversationID: "p:1:2", HighWatermark: tc.watermark, Items: []MessageDTO{}, NextBeforeSeq: tc.next, HasMore: tc.more}
			if q.HasCursor {
				p.BeforeSeq = &q.Cursor
			}
			if tc.first > 0 {
				p.Items = []MessageDTO{syncPageMessage(t, 1, tc.first), syncPageMessage(t, 2, tc.last)}
			}
			if e := p.ValidateFor(q); e != nil {
				t.Fatal(e)
			}
			b, e := json.Marshal(p)
			if e != nil || strings.Contains(string(b), `"beforeSeq"`) != q.HasCursor || !strings.Contains(string(b), `"highWatermark":"`+tc.watermark.String()+`"`) {
				t.Fatal(string(b), e)
			}
		})
	}
}

func TestHistoryPageRejectsPartialAndContradictoryResults(t *testing.T) {
	for _, mode := range []string{"bad_query", "wrong_reader", "nil_items", "wrong_conversation", "missing_before", "wrong_before", "unexpected_before", "wrong_next", "short_page", "empty_jump", "wrong_more", "gap", "duplicate_id", "duplicate_seq", "reverse", "wrong_route", "bad_id", "bad_content", "bad_media", "bad_time", "watermark_behind"} {
		t.Run(mode, func(t *testing.T) {
			q, _ := NormalizePageQuery(1, "p:1:2", "4", 2)
			before := q.Cursor
			p := HistoryPage{ConversationID: "p:1:2", BeforeSeq: &before, HighWatermark: 4, Items: []MessageDTO{syncPageMessage(t, 1, 2), syncPageMessage(t, 2, 3)}, NextBeforeSeq: 2, HasMore: true}
			switch mode {
			case "bad_query":
				q.Limit = 0
			case "wrong_reader":
				q.ReaderID = 3
			case "nil_items":
				p.Items = nil
			case "wrong_conversation":
				p.ConversationID = "g:2"
			case "missing_before":
				p.BeforeSeq = nil
			case "wrong_before":
				before = 3
			case "unexpected_before":
				q.HasCursor = false
				q.Cursor = 0
			case "wrong_next":
				p.NextBeforeSeq = 3
			case "short_page":
				p.Items = p.Items[:1]
			case "empty_jump":
				p.Items = []MessageDTO{}
			case "wrong_more":
				p.HasMore = false
			case "gap":
				p.Items[1].Seq = 4
			case "duplicate_id":
				p.Items[1] = p.Items[0]
				p.Items[1].Seq = 3
			case "duplicate_seq":
				p.Items[1].Seq = 2
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
			case "watermark_behind":
				p.HighWatermark = 2
			}
			if e := p.ValidateFor(q); e == nil {
				t.Fatal("accepted contradictory history page", mode)
			}
		})
	}
}
