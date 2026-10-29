package messaging

// ConversationListQuery's cursor is an exclusive position in canonical ASCII
// conversation-ID order. It need not still exist or be visible to this reader.
type ConversationListQuery struct {
	ReaderID uint64
	Cursor   string
	Limit    int
}

func NormalizeConversationListQuery(reader uint64, cursor string, limit int) (ConversationListQuery, error) {
	if reader == 0 {
		return ConversationListQuery{}, invalid("readerId")
	}
	if cursor != "" {
		if _, err := ParseConversationID(cursor); err != nil {
			return ConversationListQuery{}, err
		}
	}
	if limit == 0 {
		limit = DefaultPageLimit
	}
	if limit < 1 || limit > MaxPageLimit {
		return ConversationListQuery{}, invalid("limit")
	}
	return ConversationListQuery{ReaderID: reader, Cursor: cursor, Limit: limit}, nil
}

// DeliveredSeq is the server's durable ACK, not proof of this client's local
// data. A client that lost its local store must still sync from its own cursor.
type ConversationSummary struct {
	ConversationID string   `json:"conversationId"`
	LastSeq        Sequence `json:"lastSeq"`
	DeliveredSeq   Sequence `json:"deliveredSeq"`
}

type ConversationPage struct {
	Items      []ConversationSummary `json:"items"`
	NextCursor string                `json:"nextCursor"`
	HasMore    bool                  `json:"hasMore"`
}

// ValidateFor checks the HTTP adapter's result before exposing metadata. Group
// authorization remains the repository's responsibility within its snapshot.
func (p ConversationPage) ValidateFor(query ConversationListQuery) error {
	normal, err := NormalizeConversationListQuery(query.ReaderID, query.Cursor, query.Limit)
	if err != nil || normal != query || p.Items == nil || len(p.Items) > query.Limit || (p.HasMore && len(p.Items) != query.Limit) {
		return invalid("conversation page")
	}
	previous := query.Cursor
	for _, item := range p.Items {
		conversation, err := ParseConversationID(item.ConversationID)
		if err != nil || item.ConversationID <= previous || item.DeliveredSeq > item.LastSeq ||
			(conversation.Type == Private && query.ReaderID != conversation.LowUser && query.ReaderID != conversation.HighUser) {
			return invalid("conversation summary")
		}
		previous = item.ConversationID
	}
	if p.NextCursor != previous {
		return invalid("nextCursor")
	}
	return nil
}

type ConversationsResponse struct {
	Code int              `json:"code"`
	Msg  string           `json:"msg"`
	Data ConversationPage `json:"data"`
}
