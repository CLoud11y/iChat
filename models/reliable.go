package models

import "time"

// These models describe migration v1 for repository code. Their schema is
// managed by migrations.ApplyReliable, never by GORM AutoMigrate.
type Conversation struct {
	ID           string    `gorm:"column:id;primaryKey"`
	Type         string    `gorm:"column:type"`
	PrivateUserA *uint64   `gorm:"column:private_user_a"`
	PrivateUserB *uint64   `gorm:"column:private_user_b"`
	GroupID      *uint64   `gorm:"column:group_id"`
	LastSeq      uint64    `gorm:"column:last_seq"`
	UpdatedAt    time.Time `gorm:"column:updated_at"`
}

func (Conversation) TableName() string { return "conversation" }

type ReliableMessage struct {
	ID              string     `gorm:"column:id;primaryKey"`
	ClientMessageID string     `gorm:"column:client_message_id"`
	SenderID        uint64     `gorm:"column:sender_id"`
	ReceiverID      uint64     `gorm:"column:receiver_id"`
	Type            string     `gorm:"column:type"`
	Media           string     `gorm:"column:media"`
	Content         string     `gorm:"column:content"`
	ConversationID  string     `gorm:"column:conversation_id"`
	Seq             uint64     `gorm:"column:seq"`
	ClientSentAt    *time.Time `gorm:"column:client_sent_at"`
	ServerCreatedAt time.Time  `gorm:"column:server_created_at"`
}

func (ReliableMessage) TableName() string { return "message" }

type MessageCommand struct {
	SenderID        uint64    `gorm:"column:sender_id;primaryKey"`
	ClientMessageID string    `gorm:"column:client_message_id;primaryKey"`
	CommandID       string    `gorm:"column:command_id"`
	PayloadHash     string    `gorm:"column:payload_hash"`
	State           string    `gorm:"column:state"`
	ReasonCode      *string   `gorm:"column:reason_code"`
	MessageID       *string   `gorm:"column:message_id"`
	ConversationID  *string   `gorm:"column:conversation_id"`
	CreatedAt       time.Time `gorm:"column:created_at"`
	CompletedAt     time.Time `gorm:"column:completed_at"`
}

func (MessageCommand) TableName() string { return "message_command" }

type MessageOutbox struct {
	EventID        string     `gorm:"column:event_id;primaryKey"`
	EventType      string     `gorm:"column:event_type"`
	CommandID      string     `gorm:"column:command_id"`
	ConversationID *string    `gorm:"column:conversation_id"`
	MessageID      *string    `gorm:"column:message_id"`
	Payload        []byte     `gorm:"column:payload"`
	Status         string     `gorm:"column:status"`
	AvailableAt    time.Time  `gorm:"column:available_at"`
	LeaseToken     *string    `gorm:"column:lease_token"`
	LeaseUntil     *time.Time `gorm:"column:lease_until"`
	Attempts       uint32     `gorm:"column:attempts"`
	LastError      *string    `gorm:"column:last_error"`
	CreatedAt      time.Time  `gorm:"column:created_at"`
	CompletedAt    *time.Time `gorm:"column:completed_at"`
}

func (MessageOutbox) TableName() string { return "message_outbox" }

type MessageDeliveryCursor struct {
	UserID         uint64    `gorm:"column:user_id;primaryKey"`
	ConversationID string    `gorm:"column:conversation_id;primaryKey"`
	DeliveredSeq   uint64    `gorm:"column:delivered_seq"`
	UpdatedAt      time.Time `gorm:"column:updated_at"`
}

func (MessageDeliveryCursor) TableName() string { return "message_delivery_cursor" }

type MessageQuarantine struct {
	TransportID     string     `gorm:"column:transport_id;primaryKey"`
	SenderID        *uint64    `gorm:"column:sender_id"`
	ClientMessageID *string    `gorm:"column:client_message_id"`
	CommandID       *string    `gorm:"column:command_id"`
	PayloadHash     *string    `gorm:"column:payload_hash"`
	RawBody         []byte     `gorm:"column:raw_body"`
	RawHeaders      []byte     `gorm:"column:raw_headers"`
	ErrorCode       string     `gorm:"column:error_code"`
	ErrorDetail     string     `gorm:"column:error_detail"`
	State           string     `gorm:"column:state"`
	ReplayAttemptID *string    `gorm:"column:replay_attempt_id"`
	CreatedAt       time.Time  `gorm:"column:created_at"`
	ResolvedAt      *time.Time `gorm:"column:resolved_at"`
}

func (MessageQuarantine) TableName() string { return "message_quarantine" }
