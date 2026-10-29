package messaging

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestConversationListQuery(t *testing.T) {
	for _, cursor := range []string{"", "g:1", "g:18446744073709551615", "p:1:2", "p:9007199254740993:18446744073709551615"} {
		q, e := NormalizeConversationListQuery(math.MaxUint64, cursor, 0)
		if e != nil || q.ReaderID != math.MaxUint64 || q.Cursor != cursor || q.Limit != 100 {
			t.Fatal(q, e)
		}
	}
	for _, cursor := range []string{"g:0", "g:01", "g:18446744073709551616", "p:2:1", "p:1:1", " p:1:2", "G:1", "1", "g:1\x00", strings.Repeat("x", 65)} {
		if _, e := NormalizeConversationListQuery(1, cursor, 1); !errors.Is(e, ErrInvalidInput) {
			t.Fatal("bad cursor accepted", cursor, e)
		}
	}
	for _, n := range []int{-1, 501, math.MaxInt} {
		if _, e := NormalizeConversationListQuery(1, "", n); !errors.Is(e, ErrInvalidInput) {
			t.Fatal("bad limit accepted", n, e)
		}
	}
	if _, e := NormalizeConversationListQuery(0, "", 1); !errors.Is(e, ErrInvalidInput) {
		t.Fatal(e)
	}
	for _, limit := range []int{1, 500} {
		q, e := NormalizeConversationListQuery(1, "g:99", limit)
		if e != nil || q.Limit != limit {
			t.Fatal(q, e)
		}
	}
}

func TestConversationPageJSON(t *testing.T) {
	p := ConversationPage{Items: []ConversationSummary{{ConversationID: "g:18446744073709551615", LastSeq: Sequence(math.MaxUint64), DeliveredSeq: Sequence(math.MaxUint64)}}, NextCursor: "g:18446744073709551615", HasMore: true}
	b, e := json.Marshal(p)
	if e != nil || !strings.Contains(string(b), `"lastSeq":"18446744073709551615"`) || !strings.Contains(string(b), `"deliveredSeq":"18446744073709551615"`) {
		t.Fatal(string(b), e)
	}
	var round ConversationPage
	if e := json.Unmarshal(b, &round); e != nil || round.Items[0] != p.Items[0] || round.NextCursor != p.NextCursor || !round.HasMore {
		t.Fatal(round, e)
	}
	b, e = json.Marshal(ConversationPage{Items: []ConversationSummary{}})
	if e != nil || !strings.Contains(string(b), `"items":[]`) || !strings.Contains(string(b), `"nextCursor":""`) {
		t.Fatal(string(b), e)
	}
}
