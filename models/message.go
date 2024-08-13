package models

import (
	"gorm.io/gorm"
)

type Message struct {
	gorm.Model
	Identifier string `json:"identifier" gorm:"unique; type: varchar(32);"`
	SenderId   string `json:"sender"`
	ReceiverId string `json:"receiver"`
	Type       string `json:"type" gorm:"type: varchar(16);"` // 群聊/私聊...
	Content    string `json:"content" gorm:"not null; type: varchar(32);"`
	Media      uint   `json:"media" gorm:"type: varchar(16);"` // 文字/图片...
}

func (Message) TableName() string {
	return "message"
}
