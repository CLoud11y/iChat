package models

import (
	"gorm.io/gorm"
)

type Message struct {
	gorm.Model
	Identifier string `json:"identifier"`
	SenderId   string `json:"sender"`
	ReceiverId string `json:"receiver"`
	Type       string `json:"type"` // 群聊/私聊...
	Content    string `json:"content"`
	Media      uint   `json:"media"` // 文字/图片...
}

func (Message) TableName() string {
	return "message"
}
