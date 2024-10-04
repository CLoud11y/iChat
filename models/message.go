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
	Id         string `json:"id"`
	Identifier uint   `json:"identifier"`
	TimeStamp  int64  `json:"createTime"`
	SenderId   uint   `json:"userId"`
	ReceiverId uint   `json:"targetId"`
	Type       uint   `json:"type"` // 群聊/私聊...
	Content    string `json:"content" gorm:"type:varchar(1024)"`
	Media      uint   `json:"media"` // 文字/图片...
}

func (Message) TableName() string {
	return "message"
}

// 重写此方法 redis存储时会调用 否则无法直接存储message
func (msg Message) MarshalBinary() ([]byte, error) {
	return json.Marshal(msg)
}

// 发送消息接口需要
type MsgInfo struct {
	Id          string       `json:"id"`
	Identifier  uint         `json:"identifier"`
	Content     string       `json:"content"`
	SendTime    int64        `json:"sendTime"`
	ToContactId uint         `json:"toContactId"`
	Type        string       `json:"type"`
	FromUser    FromUserInfo `json:"fromUser"`
	IsGroup     int          `json:"is_group"`
}

func (info *MsgInfo) Conv2Msg() *Message {
	msg := Message{
		Id:         info.Id,
		Identifier: info.Identifier,
		TimeStamp:  info.SendTime,
		SenderId:   info.FromUser.Id,
		ReceiverId: info.ToContactId,
		Content:    info.Content,
	}
	if info.IsGroup == 1 {
		msg.Type = GroupType
	} else {
		msg.Type = PrivateType
	}
	switch info.Type {
	case "text":
		msg.Media = TextMedia
	case "picture":
		msg.Media = PictureMedia
	case "audio":
		msg.Media = AudioMedia
	default:
	}
	return &msg
}

func (msg *Message) Conv2MsgInfo() *MsgInfo {
	msgInfo := MsgInfo{
		Id:          msg.Id,
		Identifier:  msg.Identifier,
		Content:     msg.Content,
		SendTime:    msg.TimeStamp,
		ToContactId: msg.ReceiverId,
		Type:        "",
		FromUser:    FromUserInfo{Id: msg.SenderId},
		IsGroup:     0,
	}
	if msg.Type == GroupType {
		msgInfo.IsGroup = 1
	}
	switch msg.Media {
	case TextMedia:
		msgInfo.Type = "text"
	case PictureMedia:
		msgInfo.Type = "picture"
	case AudioMedia:
		msgInfo.Type = "audio"
	default:
	}
	return &msgInfo
}
