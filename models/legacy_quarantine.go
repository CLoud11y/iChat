package models

import "time"

// LegacyMessageQuarantine preserves exact queue bytes, including invalid UTF-8.
// The legacy list has no delivery ID: identical raw payloads share one audit row.
// Rows are immutable and retained; this is separate from the future MQ schema.
type LegacyMessageQuarantine struct {
	PayloadHash  string    `gorm:"type:char(64);primaryKey"`
	RawBody      []byte    `gorm:"type:longblob;not null"`
	ReasonCode   string    `gorm:"type:varchar(64);not null"`
	ReasonDetail string    `gorm:"type:text;not null"`
	CreatedAt    time.Time `gorm:"not null"`
}

func (LegacyMessageQuarantine) TableName() string { return "legacy_message_quarantine" }
