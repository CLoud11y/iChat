package models

import (
	"encoding/json"
)

const (
	HeartBeatType = iota
	InvalidType
	PrivateType
	GroupType
)

const (
	TextMedia = iota
	PictureMedia
	AudioMedia
)

type Message struct {
	Identifier uint   `json:"id"`
	TimeStamp  int64  `json:"createTime"`
	SenderId   uint   `json:"userId"`
	ReceiverId uint   `json:"targetId"`
	Type       uint   `json:"type"` // 群聊/私聊...
	Content    string `json:"content"`
	Media      uint   `json:"media"` // 文字/图片...
}

func (Message) TableName() string {
	return "message"
}

// 重写此方法 redis存储时会调用 否则无法直接存储message
func (msg Message) MarshalBinary() ([]byte, error) {
	return json.Marshal(msg)
}
