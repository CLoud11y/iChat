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
