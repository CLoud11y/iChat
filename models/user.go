package models

import (
	"gorm.io/gorm"
)

type User struct {
	gorm.Model
	Identifier string `gorm:"unique; type: varchar(32);"`
	Name       string `gorm:"not null; type: varchar(32);"`
	Password   string `gorm:"not null; type: char(32);"` // 存储md5 固定长度
	Phone      string `gorm:"unique; not null; type: varchar(32);"`
	Email      string `gorm:"type: varchar(32);"`
	ClientIp   string `gorm:"type: varchar(16);"`
	ClientPort string `gorm:"type: varchar(16);"`
	// LoginTime     time.Time
	// LogoutTime    time.Time
	// HeartBeatTime time.Time
	IsLogout   bool
	DeviceInfo string `gorm:"type: varchar(32);"`
}

func (User) TableName() string {
	return "user"
}

// 发送消息接口需要
type FromUserInfo struct {
	Id uint `json:"id"`
}
