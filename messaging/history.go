package messaging

import (
	"strconv"
	"strings"
)

const (
	DefaultPageLimit = 100
	MaxPageLimit     = 500
)

// Conversation identifies a canonical conversation, not an authorization grant.
type Conversation struct {
	ID       string
	Type     TargetType
	LowUser  uint64
	HighUser uint64
	GroupID  uint64
}

func ParseConversationID(value string) (Conversation, error) {
	if len(value) > 64 {
		return Conversation{}, invalid("conversationId")
	}
	parts := strings.Split(value, ":")
	result := Conversation{ID: value}
	if len(parts) == 2 && parts[0] == "g" {
		id, err := canonicalDecimal(parts[1], false)
		if err != nil {
			return Conversation{}, invalid("conversationId")
		}
		result.Type, result.GroupID = Group, id
		return result, nil
	}
	if len(parts) == 3 && parts[0] == "p" {
		low, lowErr := canonicalDecimal(parts[1], false)
		high, highErr := canonicalDecimal(parts[2], false)
		if lowErr == nil && highErr == nil && low < high {
			result.Type, result.LowUser, result.HighUser = Private, low, high
			return result, nil
		}
	}
	return Conversation{}, invalid("conversationId")
}

func canonicalDecimal(value string, allowZero bool) (uint64, error) {
	n, err := decimal(value, allowZero)
	if err != nil || strconv.FormatUint(n, 10) != value {
		return 0, invalid("decimal")
	}
	return n, nil
}

type PageQuery struct {
	ReaderID     uint64
	Conversation Conversation
	Cursor       Sequence
	HasCursor    bool
	Limit        int
}

// NormalizePageQuery accepts an omitted cursor (sync: zero; history: latest).
// The authenticated reader is supplied by the adapter, never by query data.
func NormalizePageQuery(authenticatedReader uint64, conversationID, cursor string, limit int) (PageQuery, error) {
	if authenticatedReader == 0 {
		return PageQuery{}, invalid("readerId")
	}
	conversation, err := ParseConversationID(conversationID)
	if err != nil {
		return PageQuery{}, err
	}
	if limit == 0 {
		limit = DefaultPageLimit
	}
	if limit < 1 || limit > MaxPageLimit {
		return PageQuery{}, invalid("limit")
	}
	query := PageQuery{ReaderID: authenticatedReader, Conversation: conversation, Limit: limit, HasCursor: cursor != ""}
	if query.HasCursor {
		n, err := canonicalDecimal(cursor, true)
		if err != nil {
			return PageQuery{}, invalid("cursor")
		}
		query.Cursor = Sequence(n)
	}
	return query, nil
}

type SyncPage struct {
	ConversationID string       `json:"conversationId"`
	AfterSeq       Sequence     `json:"afterSeq"`
	Items          []MessageDTO `json:"items"`
	NextAfterSeq   Sequence     `json:"nextAfterSeq"`
	HighWatermark  Sequence     `json:"highWatermark"`
	HasMore        bool         `json:"hasMore"`
}

// ValidateFor checks a complete contiguous prefix within this snapshot's
// watermark. It does not use the server ACK as the caller's local cursor.
func (p SyncPage) ValidateFor(query PageQuery) error {
	cursor := ""
	if query.HasCursor {
		cursor = query.Cursor.String()
	}
	normal, err := NormalizePageQuery(query.ReaderID, query.Conversation.ID, cursor, query.Limit)
	if err != nil || normal != query || p.ConversationID != query.Conversation.ID || p.AfterSeq != query.Cursor || p.Items == nil || query.Cursor > p.HighWatermark ||
		(query.Conversation.Type == Private && query.ReaderID != query.Conversation.LowUser && query.ReaderID != query.Conversation.HighUser) {
		return invalid("sync page")
	}
	available := uint64(p.HighWatermark - query.Cursor)
	count := min(available, uint64(query.Limit))
	if uint64(len(p.Items)) != count || p.HasMore != (available > uint64(query.Limit)) || p.NextAfterSeq != query.Cursor+Sequence(count) {
		return invalid("sync range")
	}
	expected := query.Cursor
	seen := make(map[string]bool, len(p.Items))
	for _, item := range p.Items {
		expected++ // count <= watermark - cursor, including at MaxUint64.
		if item.ConversationID != query.Conversation.ID || item.Seq != expected || seen[item.MessageID] {
			return invalid("sync message range")
		}
		if _, err := canonicalFrameMessage(item); err != nil {
			return err
		}
		seen[item.MessageID] = true
	}
	return nil
}

type SyncResponse struct {
	Code int      `json:"code"`
	Msg  string   `json:"msg"`
	Data SyncPage `json:"data"`
}

// History items are ascending for display; NextBeforeSeq is the oldest item.
// An omitted BeforeSeq requests the latest page, including MaxUint64.
type HistoryPage struct {
	ConversationID string       `json:"conversationId"`
	BeforeSeq      *Sequence    `json:"beforeSeq,omitempty"`
	Items          []MessageDTO `json:"items"`
	NextBeforeSeq  Sequence     `json:"nextBeforeSeq"`
	HighWatermark  Sequence     `json:"highWatermark"`
	HasMore        bool         `json:"hasMore"`
}

// ValidateFor checks the newest contiguous suffix below an exclusive cursor.
// Omission includes MaxUint64; explicit MaxUint64 excludes that sequence.
func (p HistoryPage) ValidateFor(query PageQuery) error {
	cursor := ""
	if query.HasCursor {
		cursor = query.Cursor.String()
	}
	normal, err := NormalizePageQuery(query.ReaderID, query.Conversation.ID, cursor, query.Limit)
	if err != nil || normal != query || p.ConversationID != query.Conversation.ID || p.Items == nil ||
		(query.HasCursor && (p.BeforeSeq == nil || *p.BeforeSeq != query.Cursor)) || (!query.HasCursor && p.BeforeSeq != nil) ||
		(query.Conversation.Type == Private && query.ReaderID != query.Conversation.LowUser && query.ReaderID != query.Conversation.HighUser) {
		return invalid("history page")
	}
	start := p.HighWatermark
	if query.HasCursor {
		if query.Cursor == 0 {
			start = 0
		} else {
			start = min(start, query.Cursor-1)
		}
	}
	count := min(uint64(start), uint64(query.Limit))
	next := query.Cursor
	if count > 0 {
		next = start - Sequence(count) + 1
	}
	if uint64(len(p.Items)) != count || p.HasMore != (uint64(start) > uint64(query.Limit)) || p.NextBeforeSeq != next {
		return invalid("history range")
	}
	seen := make(map[string]bool, len(p.Items))
	for i, item := range p.Items {
		if item.ConversationID != query.Conversation.ID || item.Seq != next+Sequence(i) || seen[item.MessageID] {
			return invalid("history message range")
		}
		if _, err := canonicalFrameMessage(item); err != nil {
			return err
		}
		seen[item.MessageID] = true
	}
	return nil
}

type HistoryResponse struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data HistoryPage `json:"data"`
}
