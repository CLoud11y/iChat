package messaging

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestPageQueryBoundaries(t *testing.T) {
	for _, value := range []string{"p:2:10", "p:9007199254740993:18446744073709551615", "g:18446744073709551615"} {
		query, err := NormalizePageQuery(math.MaxUint64, value, "18446744073709551615", 500)
		if err != nil || query.Conversation.ID != value || query.Cursor != Sequence(math.MaxUint64) || query.Limit != 500 || !query.HasCursor {
			t.Fatalf("query=%+v err=%v", query, err)
		}
	}
	for _, value := range []string{"", "p:10:2", "p:1:1", "p:0:1", "p:01:2", "p:1:2:3", "p:+1:2", "g:0", "g:01", "G:1", "g:18446744073709551616", " g:1", "g:١", strings.Repeat(":", 1024)} {
		if _, err := NormalizePageQuery(1, value, "", 0); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("accepted conversation %q: %v", value, err)
		}
	}
	for _, cursor := range []string{"00", "01", "-1", "+1", " 1", "1.0", "1e2", "18446744073709551616"} {
		if _, err := NormalizePageQuery(1, "p:1:2", cursor, 1); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("accepted cursor %q: %v", cursor, err)
		}
	}
	for _, limit := range []int{-1, 501} {
		if _, err := NormalizePageQuery(1, "g:1", "0", limit); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("accepted limit %d", limit)
		}
	}
	if _, err := NormalizePageQuery(0, "g:1", "", 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("accepted unauthenticated reader")
	}
	query, err := NormalizePageQuery(1, "g:1", "", 0)
	if err != nil || query.HasCursor || query.Cursor != 0 || query.Limit != DefaultPageLimit {
		t.Fatalf("defaults=%+v %v", query, err)
	}
}

func TestMessagePagesJSONUsesStringCursors(t *testing.T) {
	page := SyncPage{ConversationID: "g:1", Items: []MessageDTO{}, HighWatermark: Sequence(math.MaxUint64)}
	body, err := json.Marshal(page)
	if err != nil || !strings.Contains(string(body), `"highWatermark":"18446744073709551615"`) || !strings.Contains(string(body), `"items":[]`) {
		t.Fatalf("JSON=%s %v", body, err)
	}
	for _, hasCursor := range []bool{false, true} {
		history := HistoryPage{Items: []MessageDTO{}}
		zero := Sequence(0)
		if hasCursor {
			history.BeforeSeq = &zero
		}
		body, err := json.Marshal(history)
		if err != nil || strings.Contains(string(body), `"beforeSeq"`) != hasCursor {
			t.Fatalf("optional cursor=%s %v", body, err)
		}
	}
}
