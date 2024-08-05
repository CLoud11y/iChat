package models

import (
	"time"

	"gorm.io/gorm"
)

type UserBasic struct {
	gorm.Model
	Identifier    string
	Name          string
	Password      string
	Phone         string
	Email         string
	ClientIp      string
	ClientPort    string
	LoginTime     time.Time
	LogoutTime    time.Time
	HeartBeatTime time.Time
	isLogout      bool
	DeviceInfo    string
}

func (UserBasic) TableName() string {
	return "user_basic"
}
