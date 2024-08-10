package models

import (
	"gorm.io/gorm"
)

type Message struct {
	gorm.Model
	Identifier string `gorm:"unique; type: varchar(32);"`
	SenderId   uint
	ReceiverId uint
	Type       string `gorm:"type: varchar(16);"` // 群聊/私聊...
	Content    string `gorm:"not null; type: varchar(32);"`
	Media      uint   `gorm:"type: varchar(16);"` // 文字/图片...

}

func (Message) TableName() string {
	return "message"
}
